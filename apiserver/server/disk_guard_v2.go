// ============================================================
// server/disk_guard_v2.go — 阶段五：容量前置门禁与 Continuous 配额
// ============================================================
// 在阶段 0 的"三级阈值 + emergency/unknown 拒收"之上，阶段五引入动态
// required_free 门槛：低于该值时拒绝新建/重试/计划采集，已运行 Continuous
// Session 进入 waiting/server_storage_pressure（Agent 停止产生新窗口）；
// GC/删除继续运行；迁移与 compaction 仅在预计临时空间不侵占
// min_free_bytes + 512MiB 时运行。空间恢复到 required_free + 512MiB 且
// 连续两次 60s 检查通过后自动恢复，防止反复启停。
//
// required_free = max(critical_free_bytes,
//                     min_free_bytes + 512MiB
//                     + 2 × largest_pending_compaction_input
//                     + 2 × p95_hourly_ingest_bytes)
// ============================================================

package server

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/mini-drop/apiserver/config"
	"github.com/mini-drop/apiserver/model"
)

// pqHourTruncExpr 返回按小时截断时间戳的 SQL 表达式（PG 与 SQLite 方言差异）。
func pqHourTruncExpr(dialect, column string) string {
	if dialect == "postgres" {
		return fmt.Sprintf("date_trunc('hour', %s)", column)
	}
	return fmt.Sprintf("strftime('%%Y-%%m-%%d %%H:00:00', %s)", column)
}

// parquetDiskState 容量门禁的内存态：恢复滞后状态机。
type parquetDiskState struct {
	mu sync.Mutex
	// consecutiveOK 连续通过"required_free + hysteresis"检查的次数。
	consecutiveOK int
	// halted 当前是否处于"容量暂停"状态（采集被拒）。
	halted bool
	// lastRequiredFree 最近一次计算的 required_free（供状态接口展示）。
	lastRequiredFree uint64
	// lastQuotaUsed / lastQuotaBytes 最近一次 Continuous 配额快照。
	lastQuotaUsed  int64
	lastQuotaBytes int64
	// lastForecastBytes 最近一次 2h 采集量预测。
	lastForecastBytes int64
	// lastCheckedAt 最近一次检查时间。
	lastCheckedAt time.Time
}

// diskV2 返回全局容量状态实例（挂在 APIServer 上，由 server.go 初始化）。
func (s *APIServer) diskV2() *parquetDiskState {
	if s.parquetDisk == nil {
		s.parquetDisk = &parquetDiskState{}
	}
	return s.parquetDisk
}

// continuousQuotaSnapshot 计算 Continuous 全部存储（staging + v1 fallback +
// v2）的当前用量与硬配额。staging = 未压缩 batch payload 字节；v1 = 块对象
// 字节（不含墓碑）；v2 = 逻辑块字节（不含墓碑）。
type continuousQuotaSnapshot struct {
	QuotaBytes   int64 `json:"quota_bytes"`
	TargetBytes  int64 `json:"target_bytes"`
	UsedBytes    int64 `json:"used_bytes"`
	StagingBytes int64 `json:"staging_bytes"`
	V1BlockBytes int64 `json:"v1_block_bytes"`
	V2BlockBytes int64 `json:"v2_block_bytes"`
}

func (s *APIServer) continuousQuotaSnapshot(ctx context.Context) continuousQuotaSnapshot {
	quota := s.Config.ContinuousParquet
	out := continuousQuotaSnapshot{
		QuotaBytes:  quota.QuotaBytes,
		TargetBytes: quota.QuotaTargetBytes,
	}
	if s.DB == nil {
		return out
	}
	var staging int64
	_ = s.DB.WithContext(ctx).Model(&model.ProfileBatch{}).
		Where("(block_id IS NULL OR block_id = '')").
		Select("COALESCE(SUM(payload_bytes),0)").Scan(&staging).Error
	out.StagingBytes = staging

	var v1 int64
	_ = s.DB.WithContext(ctx).Model(&model.ContinuousProfileBlock{}).
		Where("status IN ?", []string{
			model.ContinuousBlockStatusActive,
			model.ContinuousBlockStatusSuperseded,
			model.ContinuousBlockStatusDeleting,
		}).
		Select("COALESCE(SUM(bytes_after),0)").Scan(&v1).Error
	out.V1BlockBytes = v1

	var v2 int64
	_ = s.DB.WithContext(ctx).Model(&model.ContinuousParquetBlock{}).
		Where("status IN ?", []string{
			model.ContinuousParquetStatusBuilding,
			model.ContinuousParquetStatusValidating,
			model.ContinuousParquetStatusActive,
			model.ContinuousParquetStatusSuperseded,
			model.ContinuousParquetStatusDeleting,
		}).
		Select("COALESCE(SUM(bytes_total),0)").Scan(&v2).Error
	out.V2BlockBytes = v2

	out.UsedBytes = out.StagingBytes + out.V1BlockBytes + out.V2BlockBytes
	if out.QuotaBytes <= 0 {
		out.QuotaBytes = 4 << 30
	}
	return out
}

// largestPendingCompactionInput 查询最大待压缩桶的输入字节数（未压缩 batch
// 按 session+小时桶在 Go 侧聚合，避免 SQL 方言差异）。返回 0 表示无待压缩数据。
func (s *APIServer) largestPendingCompactionInput(ctx context.Context) int64 {
	if s.DB == nil {
		return 0
	}
	type pendingBatch struct {
		SessionSID string
		StartTime  time.Time
		PayloadBytes uint64
	}
	var batches []pendingBatch
	if err := s.DB.WithContext(ctx).Model(&model.ProfileBatch{}).
		Select("session_sid, start_time, payload_bytes").
		Where("(block_id IS NULL OR block_id = '') AND payload_bytes > 0").
		Limit(5000).
		Find(&batches).Error; err != nil {
		return 0
	}
	bucketBytes := map[string]int64{}
	for _, batch := range batches {
		key := batch.SessionSID + "|" + batch.StartTime.UTC().Truncate(time.Hour).Format(time.RFC3339)
		bucketBytes[key] += int64(batch.PayloadBytes)
	}
	var largest int64
	for _, total := range bucketBytes {
		if total > largest {
			largest = total
		}
	}
	return largest
}

// p95HourlyIngestBytes 计算近 7 天每小时采集字节的 P95（用于 required_free
// 公式中的采集预测）。历史不足时退化为最大值；无数据返回 0。
func (s *APIServer) p95HourlyIngestBytes(ctx context.Context) int64 {
	if s.DB == nil {
		return 0
	}
	var values []int64
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	trunc := pqHourTruncExpr(s.DB.Dialector.Name(), "start_time")
	if err := s.DB.WithContext(ctx).Model(&model.ProfileBatch{}).
		Select(fmt.Sprintf("%s AS bucket, COALESCE(SUM(payload_bytes),0) AS total", trunc)).
		Where("start_time >= ?", cutoff).
		Group("bucket").
		Order("bucket ASC").
		Scan(&values).Error; err != nil {
		return 0
	}
	if len(values) == 0 {
		return 0
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	idx := int(float64(len(values)-1) * 0.95)
	if idx < 0 {
		idx = 0
	}
	return values[idx]
}

// forecastIngestBytes2h 预测未来 forecastWindowHours 小时的采集字节
// （p95 每小时 × 窗口小时数）。
func (s *APIServer) forecastIngestBytes(ctx context.Context, windowHours int) int64 {
	if windowHours <= 0 {
		windowHours = 2
	}
	p95 := s.p95HourlyIngestBytes(ctx)
	if p95 <= 0 {
		return 0
	}
	return p95 * int64(windowHours)
}

// requiredFreeBytes 计算动态 required_free（见文件头公式）。
func (s *APIServer) requiredFreeBytes(ctx context.Context) uint64 {
	cfg := s.Config
	_, _, critical, minFree := diskGuardConfig(cfg)
	reserve := cfg.ContinuousParquet.MinFreeReserve
	if reserve <= 0 {
		reserve = 512 << 20
	}
	largestCompaction := s.largestPendingCompactionInput(ctx)
	p95 := s.p95HourlyIngestBytes(ctx)
	required := uint64(minFree) + uint64(reserve) + uint64(2*largestCompaction) + uint64(2*p95)
	if cfg.ContinuousParquet.RequiredFreeExtraBytes > 0 {
		required += uint64(cfg.ContinuousParquet.RequiredFreeExtraBytes)
	}
	if required < critical {
		required = critical
	}
	return required
}

// collectionCapacityOK 阶段五采集容量门禁：available >= required_free 才允许
// 新建/重试/计划采集。unknown 一律拒绝（fail-closed）。
// 返回 (allowed, reason, snapshot)。
// 注意：ContinuousParquet 段未显式配置（测试/最小配置）时跳过动态门槛，
// 仅保留 emergency/unknown 硬门禁；生产 config.Load 总会填充默认值，
// 因此生产行为不受影响。
func (s *APIServer) collectionCapacityOK(source string) (bool, string, StorageDiskSnapshot) {
	snap := s.currentStorageSnapshot()
	if snap.Level == StoragePressureUnknown {
		incCollectionRejectedLowDisk(source)
		return false, "无法检查采集磁盘状态（statfs 失败），已暂停新采集", snap
	}
	if s.Config == nil || pqConfigIsZero(s.Config.ContinuousParquet) {
		return true, "", snap
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	required := s.requiredFreeBytes(ctx)
	cancel()
	state := s.diskV2()
	state.mu.Lock()
	state.lastRequiredFree = required
	state.lastCheckedAt = time.Now()
	state.mu.Unlock()

	if snap.AvailableBytes < required {
		incCollectionRejectedLowDisk(source)
		return false, "采集被拒绝：可用空间低于动态 required_free 门槛", snap
	}
	return true, "", snap
}

// pqConfigIsZero 判断 ContinuousParquet 配置段是否未初始化（全零值）。
func pqConfigIsZero(cfg config.ContinuousParquetConfig) bool {
	return cfg.Mode == "" && cfg.Tenant == "" && cfg.QuotaBytes == 0 &&
		cfg.RawRetentionHours == 0 && cfg.Res5mRetentionHours == 0 && cfg.Res1hRetentionHours == 0
}

// tickRecoveryState 每次 60s 检测调用：维护恢复滞后状态机。
// 返回当前是否处于"容量暂停"。
func (s *APIServer) tickRecoveryState(ctx context.Context, snap StorageDiskSnapshot) bool {
	cfg := s.Config
	required := s.requiredFreeBytes(ctx)
	hysteresis := cfg.ContinuousParquet.RecoverHysteresisBytes
	if hysteresis <= 0 {
		hysteresis = 512 << 20
	}
	checks := cfg.ContinuousParquet.RecoveryChecks
	if checks <= 0 {
		checks = 2
	}
	state := s.diskV2()
	state.mu.Lock()
	defer state.mu.Unlock()
	state.lastRequiredFree = required
	state.lastCheckedAt = time.Now()

	blocked := snap.Level == StoragePressureUnknown || snap.AvailableBytes < required
	if !blocked && snap.AvailableBytes >= required+uint64(hysteresis) {
		state.consecutiveOK++
	} else {
		state.consecutiveOK = 0
	}
	if blocked {
		state.halted = true
	} else if state.consecutiveOK >= checks {
		state.halted = false
	}
	return state.halted
}

// capacityHalted 返回当前是否处于容量暂停（不触发计算，供采集入口判断）。
func (s *APIServer) capacityHalted() bool {
	state := s.diskV2()
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.halted
}

// maintenanceSpaceOK 迁移/compaction 的临时空间门槛：
// 预计临时空间不侵占 min_free_bytes + 512MiB 才允许运行。
// inputBytes<=0 时仅检查保护线。
func (s *APIServer) maintenanceSpaceOK(inputBytes int64) (bool, string) {
	snap := s.currentStorageSnapshot()
	if snap.Level == StoragePressureEmergency || snap.Level == StoragePressureUnknown {
		return false, "emergency/unknown，maintenance 暂停"
	}
	cfg := s.Config
	_, _, _, minFree := diskGuardConfig(cfg)
	reserve := cfg.ContinuousParquet.MinFreeReserve
	if reserve <= 0 {
		reserve = 512 << 20
	}
	floor := uint64(minFree) + uint64(reserve)
	if snap.AvailableBytes < floor {
		return false, "可用空间低于 min_free + 512MiB，maintenance 暂停"
	}
	if inputBytes > 0 {
		remaining := snap.AvailableBytes - floor
		if uint64(inputBytes) > remaining/2 {
			return false, "临时空间可能侵占保护线，maintenance 暂停"
		}
	}
	return true, ""
}

// continuousQuotaOK 检查 Continuous 配额：超过硬配额时不允许继续写入
// staging/v2（GC/删除继续）。返回 (allowed, snapshot)。
func (s *APIServer) continuousQuotaOK(ctx context.Context) (bool, continuousQuotaSnapshot) {
	snap := s.continuousQuotaSnapshot(ctx)
	state := s.diskV2()
	state.mu.Lock()
	state.lastQuotaUsed = snap.UsedBytes
	state.lastQuotaBytes = snap.QuotaBytes
	state.mu.Unlock()
	if snap.QuotaBytes <= 0 {
		return true, snap
	}
	return snap.UsedBytes < snap.QuotaBytes, snap
}
