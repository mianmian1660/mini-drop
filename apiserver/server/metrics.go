package server

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/mini-drop/apiserver/model"
	"github.com/mini-drop/apiserver/util"
)

var (
	metricTasksCreatedTotal         int64
	metricTaskNotifyFailedTotal     int64
	metricArtifactUploadFailedTotal int64
	metricAnalysisQueuedTotal       int64
	metricSSEActiveConnections      int64
	// 阶段三：内建持续剖析块存储指标
	metricContinuousBlocksCreatedTotal     int64
	metricContinuousBlocksReplacedTotal    int64
	metricContinuousBlocksReadTotal        int64
	metricContinuousSourceDeleteRetryTotal int64
	metricContinuousCompactionSkipTotal    int64
	metricContinuousReclaimedBytesTotal    int64
	// 阶段一：持续采集正确性指标
	metricContinuousConflictTotal       int64 // counter：内容冲突（409 不可重试）次数
	metricContinuousDuplicateBatchTotal int64 // counter：批次级重复 ACK 次数
	// 阶段 0：磁盘止血指标
	metricStorageTotalBytes     int64 // gauge：受监控文件系统总字节数
	metricStorageAvailableBytes int64 // gauge：受监控文件系统剩余字节数
	metricStoragePressureLevel  int64 // gauge：0=normal 1=warning 2=critical 3=emergency 4=unknown
	// 存储阶段一：Artifact 生命周期指标
	metricArtifactCleanupDeletedTotal      int64 // counter：清理成功删除的对象数
	metricArtifactCleanupDeletedBytesTotal int64 // counter：清理回收的字节数
	metricArtifactCleanupFailuresTotal     int64 // counter：对象删除失败次数
	metricArtifactReadyBytes               int64 // gauge：ready 状态字节数
	metricArtifactPinnedBytes              int64 // gauge：固定任务下非 deleted 字节数
	metricArtifactSupersededBytes          int64 // gauge：被替换旧代（result_superseded）字节数
	metricArtifactExpirationBacklog        int64 // gauge：到期候选（due）数量
	metricArtifactPolicyReconcileBacklog   int64 // gauge：策略版本不一致待重算数量
	// 按 status 计数的 gauge（label: status）
	metricArtifactsByStatusMu sync.Mutex
	metricArtifactsByStatus   = map[string]int64{}
	// 存储阶段二：物理 Blob 指标
	metricBlobPhysicalBytes      int64 // gauge：ready blob 物理字节
	metricBlobLogicalBytes       int64 // gauge：ready blob 逻辑字节
	metricBlobDeduplicatedBytes  int64 // gauge：去重+压缩收益
	metricBlobMigrationBacklog   int64 // gauge：迁移剩余候选
	metricBlobBackfillBacklog    int64 // gauge：待回填引用数
	metricBlobBackfillCreated    int64 // counter：回填创建的 blob 数
	metricBlobBackfillLinked     int64 // counter：回填关联的引用数
	metricBlobBackfillStatFail   int64 // counter：回填 Stat 失败次数
	metricBlobBackfillConflicts  int64 // counter：同 key 多大小冲突次数
	metricBlobMigrationObjects   int64 // counter：迁移完成对象数
	metricBlobMigrationFailures  int64 // counter：迁移失败对象数
	metricBlobMigrationReclaimed int64 // counter：迁移回收字节（逻辑-存储）
	metricBlobGCDeleted          int64 // counter：GC 删除对象数
	metricBlobGCDeletedBytes     int64 // counter：GC 回收字节
	metricBlobGCFailures         int64 // counter：GC 删除失败次数
	metricBlobOrphanReconciled   int64 // counter：孤儿回填 blob 墓碑化数
	metricMaintenanceSkipTotal   int64 // counter：maintenance 跳过（低磁盘/unknown）
	metricBlobByStatusMu         sync.Mutex
	metricBlobByStatus           = map[string]int64{}
	metricBlobByStatusBytesMu    sync.Mutex
	metricBlobByStatusBytes      = map[string]int64{}
	// 阶段五：Parquet v2 指标
	metricParquetBuildSkipTotal         int64 // counter：v2 块构建跳过（低磁盘/配额）
	metricParquetMetricUnknownKindTotal int64 // counter：未登记 metric 类型告警
	// 阶段六：细粒度 GC / 迁移失败 / 查询指标
	metricFineGCCandidatesTotal    int64 // counter：细粒度 GC 候选 batch 数
	metricFineGCDeletedTotal       int64 // counter：细粒度 GC 已清理 batch 数
	metricFineGCFailuresTotal      int64 // counter：细粒度 GC 失败次数
	metricMigrationFailureTotal    int64 // counter：迁移失败记录新增数
	metricMigrationQuarantineTotal int64 // counter：隔离数
	metricParquetV1FallbackTotal   int64 // counter：v1 fallback 次数
	metricParquetQueryErrorsTotal  int64 // counter：Parquet 查询错误次数
	metricParquetQueryLatencyMs    int64 // gauge：最近一次 Parquet 查询耗时（ms）
	// 按原因计数的细粒度 GC 阻塞（label: reason）
	metricFineGCBlockedMu sync.Mutex
	metricFineGCBlocked   = map[string]int64{}
	// 按错误类型计数的迁移失败（label: error_type）
	metricMigrationFailureByTypeMu sync.Mutex
	metricMigrationFailureByType   = map[string]int64{}
	// 阶段六：热表/对账/覆盖率 gauge（由 pqRefreshPhase6Metrics 刷新）
	metricHotWindowCount          int64 // gauge：热 window 数（< 2h）
	metricHotBatchCount           int64 // gauge：热 batch 数
	metricHotWindowOldestMs       int64 // gauge：最老热 window 时间戳（ms）
	metricHotBatchOldestMs        int64 // gauge：最老热 batch 时间戳（ms）
	metricOrphanWindowCount       int64 // gauge：orphan window 数
	metricCoverageSegments        int64 // gauge：coverage segment 数
	metricReconcileFailed         int64 // gauge：对账失败 raw 块数
	metricReconcileQuarantined    int64 // gauge：对账隔离 raw 块数
	metricFineGCEnforceCandidates int64 // gauge：enforce 可清理候选数（observe 统计）
	// 阶段六：migration receipt 回收指标
	metricMigrationReceiptCount           int64 // gauge：receipt 总数
	metricMigrationReceiptGCEligible      int64 // gauge：可回收 receipt 数（batch 已删 + 超保留期）
	metricMigrationReceiptGCDeletedTotal  int64 // counter：已回收 receipt 数
	metricMigrationReceiptGCFailuresTotal int64 // counter：receipt 回收失败次数
)

// 按来源计数的低磁盘拒收计数（label: source）。
var (
	metricCollectionRejectedLowDiskMu sync.Mutex
	metricCollectionRejectedLowDisk   = map[string]int64{}
)

// maintenance 跳过计数（label: kind：continuous_compaction / format_migration）。
var (
	metricMaintenanceSkipMu sync.Mutex
	metricMaintenanceSkip   = map[string]int64{}
)

// 阶段五：shadow 对账失败计数与指标辅助。
var (
	parquetShadowFailures atomic.Int64
)

func incParquetBuildSkip(reason string) {
	atomic.AddInt64(&metricParquetBuildSkipTotal, 1)
	incMaintenanceSkip("parquet_build")
}
func incParquetMetricUnknownKind(metric string) {
	atomic.AddInt64(&metricParquetMetricUnknownKindTotal, 1)
}
func incParquetShadowFailure() { parquetShadowFailures.Add(1) }

// 阶段六指标辅助。
func incFineGCCandidate(bid string) { atomic.AddInt64(&metricFineGCCandidatesTotal, 1) }
func incFineGCDeleted(bid string)   { atomic.AddInt64(&metricFineGCDeletedTotal, 1) }
func incFineGCFailure(reason, bid string) {
	atomic.AddInt64(&metricFineGCFailuresTotal, 1)
}
func incFineGCBlocked(reason, bid string) {
	metricFineGCBlockedMu.Lock()
	metricFineGCBlocked[reason]++
	metricFineGCBlockedMu.Unlock()
}
func incMigrationReceiptGCDeleted(n int64) {
	if n > 0 {
		atomic.AddInt64(&metricMigrationReceiptGCDeletedTotal, n)
	}
}
func incMigrationReceiptGCFailure() { atomic.AddInt64(&metricMigrationReceiptGCFailuresTotal, 1) }
func incContinuousMigrationFailure(errorType string) {
	atomic.AddInt64(&metricMigrationFailureTotal, 1)
	metricMigrationFailureByTypeMu.Lock()
	metricMigrationFailureByType[errorType]++
	metricMigrationFailureByTypeMu.Unlock()
}
func incContinuousMigrationQuarantine() { atomic.AddInt64(&metricMigrationQuarantineTotal, 1) }
func incParquetV1Fallback()             { atomic.AddInt64(&metricParquetV1FallbackTotal, 1) }
func incParquetQueryError()             { atomic.AddInt64(&metricParquetQueryErrorsTotal, 1) }
func observeParquetQueryLatency(ms int64) {
	if ms >= 0 {
		atomic.StoreInt64(&metricParquetQueryLatencyMs, ms)
	}
}

func incTasksCreated()         { atomic.AddInt64(&metricTasksCreatedTotal, 1) }
func incTaskNotifyFailed()     { atomic.AddInt64(&metricTaskNotifyFailedTotal, 1) }
func incArtifactUploadFailed() { atomic.AddInt64(&metricArtifactUploadFailedTotal, 1) }
func incAnalysisQueued()       { atomic.AddInt64(&metricAnalysisQueuedTotal, 1) }
func incSSEActive()            { atomic.AddInt64(&metricSSEActiveConnections, 1) }
func decSSEActive()            { atomic.AddInt64(&metricSSEActiveConnections, -1) }

func incContinuousBlocksCreated(replaced bool) {
	atomic.AddInt64(&metricContinuousBlocksCreatedTotal, 1)
	if replaced {
		atomic.AddInt64(&metricContinuousBlocksReplacedTotal, 1)
	}
}

// incContinuousConflictTotal 阶段一：内容冲突（409 不可重试）计数。
func incContinuousConflictTotal() {
	atomic.AddInt64(&metricContinuousConflictTotal, 1)
}

// incContinuousDuplicateBatchTotal 阶段一：批次级重复 ACK 计数。
func incContinuousDuplicateBatchTotal() {
	atomic.AddInt64(&metricContinuousDuplicateBatchTotal, 1)
}
func incContinuousBlocksRead()        { atomic.AddInt64(&metricContinuousBlocksReadTotal, 1) }
func incContinuousSourceDeleteRetry() { atomic.AddInt64(&metricContinuousSourceDeleteRetryTotal, 1) }
func incContinuousCompactionSkip()    { atomic.AddInt64(&metricContinuousCompactionSkipTotal, 1) }
func incContinuousReclaimedBytes(bytes int64) {
	atomic.AddInt64(&metricContinuousReclaimedBytesTotal, bytes)
}

func incArtifactCleanupDeleted(_ string) {
	atomic.AddInt64(&metricArtifactCleanupDeletedTotal, 1)
}
func incArtifactCleanupDeletedBytes(bytes int64) {
	atomic.AddInt64(&metricArtifactCleanupDeletedBytesTotal, bytes)
}
func incArtifactCleanupFailures() {
	atomic.AddInt64(&metricArtifactCleanupFailuresTotal, 1)
}

// updateArtifactLifecycleGauges 用最新生命周期统计刷新 gauge 指标。
func updateArtifactLifecycleGauges(stats artifactLifecycleStats) {
	atomic.StoreInt64(&metricArtifactReadyBytes, stats.ReadyBytes)
	atomic.StoreInt64(&metricArtifactPinnedBytes, stats.PinnedBytes)
	atomic.StoreInt64(&metricArtifactSupersededBytes, stats.SupersededBytes)
	atomic.StoreInt64(&metricArtifactExpirationBacklog, stats.DueCount)
	atomic.StoreInt64(&metricArtifactPolicyReconcileBacklog, stats.ReconcileBacklog)
}

// refreshArtifactsByStatusGauge 从 DB 刷新按状态计数的 gauge（在指标输出时兜底）。
func (s *APIServer) refreshArtifactsByStatusGauge() {
	if s == nil || s.DB == nil {
		return
	}
	rows := []struct {
		Status string
		Count  int64
	}{}
	_ = s.DB.Model(&model.Artifact{}).Select("status, count(*) as count").Group("status").Scan(&rows).Error
	metricArtifactsByStatusMu.Lock()
	metricArtifactsByStatus = map[string]int64{}
	for _, r := range rows {
		metricArtifactsByStatus[r.Status] = r.Count
	}
	metricArtifactsByStatusMu.Unlock()
}

// storagePressureLevelNumeric 把等级映射为数值（供 gauge 使用）。
func storagePressureLevelNumeric(level StoragePressureLevel) int64 {
	switch level {
	case StoragePressureNormal:
		return 0
	case StoragePressureWarning:
		return 1
	case StoragePressureCritical:
		return 2
	case StoragePressureEmergency:
		return 3
	default:
		return 4 // unknown
	}
}

// updateStorageMetrics 用最新磁盘快照刷新 gauge 指标。
func updateStorageMetrics(snap StorageDiskSnapshot) {
	atomic.StoreInt64(&metricStorageTotalBytes, int64(snap.TotalBytes))
	atomic.StoreInt64(&metricStorageAvailableBytes, int64(snap.AvailableBytes))
	atomic.StoreInt64(&metricStoragePressureLevel, storagePressureLevelNumeric(snap.Level))
}

// incCollectionRejectedLowDisk 累加"低磁盘拒收"计数（按采集入口来源）。
func incCollectionRejectedLowDisk(source string) {
	metricCollectionRejectedLowDiskMu.Lock()
	metricCollectionRejectedLowDisk[source]++
	metricCollectionRejectedLowDiskMu.Unlock()
}

// ---- 存储阶段二：Blob 指标 ----

func incBlobBackfillBlobsCreated() { atomic.AddInt64(&metricBlobBackfillCreated, 1) }
func incBlobBackfillRefsLinked(n int64) {
	if n > 0 {
		atomic.AddInt64(&metricBlobBackfillLinked, n)
	}
}
func incBlobBackfillStatFailures() { atomic.AddInt64(&metricBlobBackfillStatFail, 1) }
func incBlobBackfillConflicts()    { atomic.AddInt64(&metricBlobBackfillConflicts, 1) }
func incBlobMigrationObjects(format string) {
	atomic.AddInt64(&metricBlobMigrationObjects, 1)
	_ = format // 日志已带 format，计数器不细分避免 label 膨胀
}
func incBlobMigrationFailures() { atomic.AddInt64(&metricBlobMigrationFailures, 1) }
func incBlobMigrationReclaimedBytes(n int64) {
	if n > 0 {
		atomic.AddInt64(&metricBlobMigrationReclaimed, n)
	}
}
func incBlobGCDeleted() { atomic.AddInt64(&metricBlobGCDeleted, 1) }
func incBlobGCDeletedBytes(n int64) {
	if n > 0 {
		atomic.AddInt64(&metricBlobGCDeletedBytes, n)
	}
}
func incBlobGCFailures() { atomic.AddInt64(&metricBlobGCFailures, 1) }

func incBlobOrphanReconciled() { atomic.AddInt64(&metricBlobOrphanReconciled, 1) }

// incMaintenanceSkip 累加 maintenance 跳过计数（compactor / 格式迁移）。
// 与采集拒收口径分离：maintenance 跳过不是"采集拒绝"。
func incMaintenanceSkip(kind string) {
	metricMaintenanceSkipMu.Lock()
	metricMaintenanceSkip[kind]++
	metricMaintenanceSkipMu.Unlock()
}

// updateBlobGauges 用最新 Blob 统计刷新 gauge 指标。
func updateBlobGauges(stats blobStats) {
	atomic.StoreInt64(&metricBlobPhysicalBytes, stats.BlobPhysicalBytes)
	atomic.StoreInt64(&metricBlobLogicalBytes, stats.BlobLogicalBytes)
	atomic.StoreInt64(&metricBlobDeduplicatedBytes, stats.BlobDedupBytes)
	atomic.StoreInt64(&metricBlobMigrationBacklog, stats.Backlog)
	atomic.StoreInt64(&metricBlobBackfillBacklog, stats.BackfillBacklog)
	metricBlobByStatusMu.Lock()
	metricBlobByStatus = map[string]int64{}
	for k, v := range stats.ByStatusCount {
		metricBlobByStatus[k] = v
	}
	metricBlobByStatusMu.Unlock()
	metricBlobByStatusBytesMu.Lock()
	metricBlobByStatusBytes = map[string]int64{}
	for k, v := range stats.ByStatusBytes {
		metricBlobByStatusBytes[k] = v
	}
	metricBlobByStatusBytesMu.Unlock()
}

// pprofArtifactStats 从 DB 统计 pprof 转换结果（成功=ready、失败=failed）。
func (s *APIServer) pprofArtifactStats() (ready int64, failed int64, logicalBytes int64, storedBytes int64) {
	if s == nil || s.DB == nil {
		return 0, 0, 0, 0
	}
	type cnt struct {
		Status string
		Cnt    int64
	}
	var rows []cnt
	_ = s.DB.Model(&model.Artifact{}).
		Select("status, count(*) as cnt").
		Where("format = ?", model.BlobFormatPprof).
		Group("status").Scan(&rows).Error
	for _, r := range rows {
		switch r.Status {
		case model.ArtifactStatusReady:
			ready = r.Cnt
		case model.ArtifactStatusFailed:
			failed = r.Cnt
		}
	}
	_ = s.DB.Model(&model.StorageBlob{}).
		Where("format = ? AND status = ? AND deleted_at IS NULL", model.BlobFormatPprof, model.BlobStatusReady).
		Select("COALESCE(SUM(logical_size),0), COALESCE(SUM(stored_size),0)").
		Row().Scan(&logicalBytes, &storedBytes)
	return ready, failed, logicalBytes, storedBytes
}

func snapshotRejectedLowDiskCounts() map[string]int64 {
	metricCollectionRejectedLowDiskMu.Lock()
	defer metricCollectionRejectedLowDiskMu.Unlock()
	out := make(map[string]int64, len(metricCollectionRejectedLowDisk))
	for k, v := range metricCollectionRejectedLowDisk {
		out[k] = v
	}
	return out
}

func currentSSEActive() int64 { return atomic.LoadInt64(&metricSSEActiveConnections) }

func resetMetricsForTest() {
	atomic.StoreInt64(&metricTasksCreatedTotal, 0)
	atomic.StoreInt64(&metricTaskNotifyFailedTotal, 0)
	atomic.StoreInt64(&metricArtifactUploadFailedTotal, 0)
	atomic.StoreInt64(&metricAnalysisQueuedTotal, 0)
	atomic.StoreInt64(&metricSSEActiveConnections, 0)
	atomic.StoreInt64(&metricContinuousBlocksCreatedTotal, 0)
	atomic.StoreInt64(&metricContinuousBlocksReplacedTotal, 0)
	atomic.StoreInt64(&metricContinuousBlocksReadTotal, 0)
	atomic.StoreInt64(&metricContinuousSourceDeleteRetryTotal, 0)
	atomic.StoreInt64(&metricContinuousCompactionSkipTotal, 0)
	atomic.StoreInt64(&metricContinuousReclaimedBytesTotal, 0)
	atomic.StoreInt64(&metricContinuousConflictTotal, 0)
	atomic.StoreInt64(&metricContinuousDuplicateBatchTotal, 0)
	atomic.StoreInt64(&metricStorageTotalBytes, 0)
	atomic.StoreInt64(&metricStorageAvailableBytes, 0)
	atomic.StoreInt64(&metricStoragePressureLevel, 4) // unknown 兜底
	atomic.StoreInt64(&metricArtifactCleanupDeletedTotal, 0)
	atomic.StoreInt64(&metricArtifactCleanupDeletedBytesTotal, 0)
	atomic.StoreInt64(&metricArtifactCleanupFailuresTotal, 0)
	atomic.StoreInt64(&metricArtifactReadyBytes, 0)
	atomic.StoreInt64(&metricArtifactPinnedBytes, 0)
	atomic.StoreInt64(&metricArtifactSupersededBytes, 0)
	atomic.StoreInt64(&metricArtifactExpirationBacklog, 0)
	atomic.StoreInt64(&metricArtifactPolicyReconcileBacklog, 0)
	atomic.StoreInt64(&metricBlobPhysicalBytes, 0)
	atomic.StoreInt64(&metricBlobLogicalBytes, 0)
	atomic.StoreInt64(&metricBlobDeduplicatedBytes, 0)
	atomic.StoreInt64(&metricBlobMigrationBacklog, 0)
	atomic.StoreInt64(&metricBlobBackfillBacklog, 0)
	atomic.StoreInt64(&metricBlobBackfillCreated, 0)
	atomic.StoreInt64(&metricBlobBackfillLinked, 0)
	atomic.StoreInt64(&metricBlobBackfillStatFail, 0)
	atomic.StoreInt64(&metricBlobBackfillConflicts, 0)
	atomic.StoreInt64(&metricBlobMigrationObjects, 0)
	atomic.StoreInt64(&metricBlobMigrationFailures, 0)
	atomic.StoreInt64(&metricBlobMigrationReclaimed, 0)
	atomic.StoreInt64(&metricBlobGCDeleted, 0)
	atomic.StoreInt64(&metricBlobGCDeletedBytes, 0)
	atomic.StoreInt64(&metricBlobGCFailures, 0)
	atomic.StoreInt64(&metricBlobOrphanReconciled, 0)
	atomic.StoreInt64(&metricMaintenanceSkipTotal, 0)
	metricArtifactsByStatusMu.Lock()
	metricArtifactsByStatus = map[string]int64{}
	metricArtifactsByStatusMu.Unlock()
	metricBlobByStatusMu.Lock()
	metricBlobByStatus = map[string]int64{}
	metricBlobByStatusMu.Unlock()
	metricBlobByStatusBytesMu.Lock()
	metricBlobByStatusBytes = map[string]int64{}
	metricBlobByStatusBytesMu.Unlock()
	metricCollectionRejectedLowDiskMu.Lock()
	metricCollectionRejectedLowDisk = map[string]int64{}
	metricCollectionRejectedLowDiskMu.Unlock()
	metricMaintenanceSkipMu.Lock()
	metricMaintenanceSkip = map[string]int64{}
	metricMaintenanceSkipMu.Unlock()
}

func (s *APIServer) Metrics(c *gin.Context) {
	var b strings.Builder
	writeMetricHeader(&b, "mini_drop_tasks_created_total", "counter", "Tasks created through the API.")
	fmt.Fprintf(&b, "mini_drop_tasks_created_total %d\n", atomic.LoadInt64(&metricTasksCreatedTotal))
	writeMetricHeader(&b, "mini_drop_task_notify_failed_total", "counter", "Failed task result notifications.")
	fmt.Fprintf(&b, "mini_drop_task_notify_failed_total %d\n", atomic.LoadInt64(&metricTaskNotifyFailedTotal))
	writeMetricHeader(&b, "mini_drop_artifact_upload_failed_total", "counter", "Artifact upload or notify failures.")
	fmt.Fprintf(&b, "mini_drop_artifact_upload_failed_total %d\n", atomic.LoadInt64(&metricArtifactUploadFailedTotal))
	writeMetricHeader(&b, "mini_drop_analysis_queued_total", "counter", "Analysis jobs queued by apiserver.")
	fmt.Fprintf(&b, "mini_drop_analysis_queued_total %d\n", atomic.LoadInt64(&metricAnalysisQueuedTotal))
	writeMetricHeader(&b, "mini_drop_sse_active_connections", "gauge", "Current active SSE connections.")
	fmt.Fprintf(&b, "mini_drop_sse_active_connections %d\n", currentSSEActive())
	writeMetricHeader(&b, "mini_drop_continuous_blocks_created_total", "counter", "Continuous profile blocks created by compactor.")
	fmt.Fprintf(&b, "mini_drop_continuous_blocks_created_total %d\n", atomic.LoadInt64(&metricContinuousBlocksCreatedTotal))
	writeMetricHeader(&b, "mini_drop_continuous_blocks_replaced_total", "counter", "Continuous profile blocks replaced by a new version (late batch or retention rewrite).")
	fmt.Fprintf(&b, "mini_drop_continuous_blocks_replaced_total %d\n", atomic.LoadInt64(&metricContinuousBlocksReplacedTotal))
	writeMetricHeader(&b, "mini_drop_continuous_blocks_read_total", "counter", "Continuous profile block objects decompressed by queries.")
	fmt.Fprintf(&b, "mini_drop_continuous_blocks_read_total %d\n", atomic.LoadInt64(&metricContinuousBlocksReadTotal))
	writeMetricHeader(&b, "mini_drop_continuous_source_delete_retry_total", "counter", "Failed source minute-object deletions retried by sweep.")
	fmt.Fprintf(&b, "mini_drop_continuous_source_delete_retry_total %d\n", atomic.LoadInt64(&metricContinuousSourceDeleteRetryTotal))
	writeMetricHeader(&b, "mini_drop_continuous_compaction_skip_total", "counter", "Compaction runs skipped (low disk or dependency unavailable).")
	fmt.Fprintf(&b, "mini_drop_continuous_compaction_skip_total %d\n", atomic.LoadInt64(&metricContinuousCompactionSkipTotal))
	writeMetricHeader(&b, "mini_drop_continuous_reclaimed_bytes_total", "counter", "Bytes reclaimed by block/source-object garbage collection.")
	fmt.Fprintf(&b, "mini_drop_continuous_reclaimed_bytes_total %d\n", atomic.LoadInt64(&metricContinuousReclaimedBytesTotal))
	writeMetricHeader(&b, "mini_drop_continuous_conflict_total", "counter", "Continuous batch/window content conflicts rejected as non-retryable 409.")
	fmt.Fprintf(&b, "mini_drop_continuous_conflict_total %d\n", atomic.LoadInt64(&metricContinuousConflictTotal))
	writeMetricHeader(&b, "mini_drop_continuous_duplicate_batch_total", "counter", "Continuous batch-level duplicate ACKs (same batch_id retransmit).")
	fmt.Fprintf(&b, "mini_drop_continuous_duplicate_batch_total %d\n", atomic.LoadInt64(&metricContinuousDuplicateBatchTotal))
	writeMetricHeader(&b, "mini_drop_parquet_build_skip_total", "counter", "Parquet v2 block builds skipped (low disk or quota exceeded).")
	fmt.Fprintf(&b, "mini_drop_parquet_build_skip_total %d\n", atomic.LoadInt64(&metricParquetBuildSkipTotal))
	writeMetricHeader(&b, "mini_drop_parquet_metric_unknown_kind_total", "counter", "Unknown metric kinds treated as gauge (alert for counter misclassification).")
	fmt.Fprintf(&b, "mini_drop_parquet_metric_unknown_kind_total %d\n", atomic.LoadInt64(&metricParquetMetricUnknownKindTotal))
	writeMetricHeader(&b, "mini_drop_parquet_shadow_failures_total", "counter", "Parquet v2 shadow reconciliation failures (v1 remains query source).")
	fmt.Fprintf(&b, "mini_drop_parquet_shadow_failures_total %d\n", parquetShadowFailures.Load())
	// 阶段六：细粒度 GC / 迁移失败 / 查询指标
	writeMetricHeader(&b, "mini_drop_fine_gc_candidates_total", "counter", "Fine-grained GC candidate batches observed.")
	fmt.Fprintf(&b, "mini_drop_fine_gc_candidates_total %d\n", atomic.LoadInt64(&metricFineGCCandidatesTotal))
	writeMetricHeader(&b, "mini_drop_fine_gc_deleted_total", "counter", "Fine-grained GC batches cleaned.")
	fmt.Fprintf(&b, "mini_drop_fine_gc_deleted_total %d\n", atomic.LoadInt64(&metricFineGCDeletedTotal))
	writeMetricHeader(&b, "mini_drop_fine_gc_failures_total", "counter", "Fine-grained GC failures.")
	fmt.Fprintf(&b, "mini_drop_fine_gc_failures_total %d\n", atomic.LoadInt64(&metricFineGCFailuresTotal))
	writeMetricHeader(&b, "mini_drop_fine_gc_blocked_total", "counter", "Fine-grained GC candidates blocked, by reason.")
	blockedReasons := make([]string, 0, len(metricFineGCBlocked))
	metricFineGCBlockedMu.Lock()
	for reason := range metricFineGCBlocked {
		blockedReasons = append(blockedReasons, reason)
	}
	metricFineGCBlockedMu.Unlock()
	sort.Strings(blockedReasons)
	for _, reason := range blockedReasons {
		count := func() int64 {
			metricFineGCBlockedMu.Lock()
			defer metricFineGCBlockedMu.Unlock()
			return metricFineGCBlocked[reason]
		}()
		fmt.Fprintf(&b, "mini_drop_fine_gc_blocked_total{reason=%q} %d\n", prometheusLabel(reason), count)
	}
	writeMetricHeader(&b, "mini_drop_migration_failure_total", "counter", "Continuous migration failure records created, by error type.")
	failureTypes := make([]string, 0, len(metricMigrationFailureByType))
	metricMigrationFailureByTypeMu.Lock()
	for errorType := range metricMigrationFailureByType {
		failureTypes = append(failureTypes, errorType)
	}
	metricMigrationFailureByTypeMu.Unlock()
	sort.Strings(failureTypes)
	for _, errorType := range failureTypes {
		count := func() int64 {
			metricMigrationFailureByTypeMu.Lock()
			defer metricMigrationFailureByTypeMu.Unlock()
			return metricMigrationFailureByType[errorType]
		}()
		fmt.Fprintf(&b, "mini_drop_migration_failure_total{error_type=%q} %d\n", prometheusLabel(errorType), count)
	}
	writeMetricHeader(&b, "mini_drop_migration_quarantine_total", "counter", "Continuous migration failures quarantined.")
	fmt.Fprintf(&b, "mini_drop_migration_quarantine_total %d\n", atomic.LoadInt64(&metricMigrationQuarantineTotal))
	writeMetricHeader(&b, "mini_drop_parquet_v1_fallback_total", "counter", "Query hours falling back to v1 storage (missing/blocked parquet coverage).")
	fmt.Fprintf(&b, "mini_drop_parquet_v1_fallback_total %d\n", atomic.LoadInt64(&metricParquetV1FallbackTotal))
	writeMetricHeader(&b, "mini_drop_parquet_query_errors_total", "counter", "Parquet query errors (read/decode/dependency).")
	fmt.Fprintf(&b, "mini_drop_parquet_query_errors_total %d\n", atomic.LoadInt64(&metricParquetQueryErrorsTotal))
	writeMetricHeader(&b, "mini_drop_parquet_query_latency_ms", "gauge", "Last parquet query latency in milliseconds.")
	fmt.Fprintf(&b, "mini_drop_parquet_query_latency_ms %d\n", atomic.LoadInt64(&metricParquetQueryLatencyMs))
	writeMetricHeader(&b, "mini_drop_fine_gc_hot_windows", "gauge", "Hot profile window count (within hot metadata retention).")
	fmt.Fprintf(&b, "mini_drop_fine_gc_hot_windows %d\n", atomic.LoadInt64(&metricHotWindowCount))
	writeMetricHeader(&b, "mini_drop_fine_gc_hot_batches", "gauge", "Hot profile batch count (within hot metadata retention).")
	fmt.Fprintf(&b, "mini_drop_fine_gc_hot_batches %d\n", atomic.LoadInt64(&metricHotBatchCount))
	writeMetricHeader(&b, "mini_drop_fine_gc_hot_window_oldest_ms", "gauge", "Oldest hot window start time (unix ms).")
	fmt.Fprintf(&b, "mini_drop_fine_gc_hot_window_oldest_ms %d\n", atomic.LoadInt64(&metricHotWindowOldestMs))
	writeMetricHeader(&b, "mini_drop_fine_gc_hot_batch_oldest_ms", "gauge", "Oldest hot batch start time (unix ms).")
	fmt.Fprintf(&b, "mini_drop_fine_gc_hot_batch_oldest_ms %d\n", atomic.LoadInt64(&metricHotBatchOldestMs))
	writeMetricHeader(&b, "mini_drop_fine_gc_orphan_windows", "gauge", "Orphan profile windows (missing batch reference).")
	fmt.Fprintf(&b, "mini_drop_fine_gc_orphan_windows %d\n", atomic.LoadInt64(&metricOrphanWindowCount))
	writeMetricHeader(&b, "mini_drop_fine_gc_coverage_segments", "gauge", "Coverage segment count.")
	fmt.Fprintf(&b, "mini_drop_fine_gc_coverage_segments %d\n", atomic.LoadInt64(&metricCoverageSegments))
	writeMetricHeader(&b, "mini_drop_fine_gc_reconcile_failed", "gauge", "Raw parquet blocks with failed reconcile status.")
	fmt.Fprintf(&b, "mini_drop_fine_gc_reconcile_failed %d\n", atomic.LoadInt64(&metricReconcileFailed))
	writeMetricHeader(&b, "mini_drop_fine_gc_reconcile_quarantined", "gauge", "Raw parquet blocks quarantined from reconcile failures.")
	fmt.Fprintf(&b, "mini_drop_fine_gc_reconcile_quarantined %d\n", atomic.LoadInt64(&metricReconcileQuarantined))
	writeMetricHeader(&b, "mini_drop_fine_gc_enforce_candidates", "gauge", "Fine-grained GC candidates eligible for enforce cleanup.")
	fmt.Fprintf(&b, "mini_drop_fine_gc_enforce_candidates %d\n", atomic.LoadInt64(&metricFineGCEnforceCandidates))
	writeMetricHeader(&b, "mini_drop_migration_receipt_count", "gauge", "Continuous migration receipt total count.")
	fmt.Fprintf(&b, "mini_drop_migration_receipt_count %d\n", atomic.LoadInt64(&metricMigrationReceiptCount))
	writeMetricHeader(&b, "mini_drop_migration_receipt_gc_eligible", "gauge", "Migration receipts eligible for GC (batch deleted and past retention).")
	fmt.Fprintf(&b, "mini_drop_migration_receipt_gc_eligible %d\n", atomic.LoadInt64(&metricMigrationReceiptGCEligible))
	writeMetricHeader(&b, "mini_drop_migration_receipt_gc_deleted_total", "counter", "Migration receipts recycled by GC.")
	fmt.Fprintf(&b, "mini_drop_migration_receipt_gc_deleted_total %d\n", atomic.LoadInt64(&metricMigrationReceiptGCDeletedTotal))
	writeMetricHeader(&b, "mini_drop_migration_receipt_gc_failures_total", "counter", "Migration receipt GC failures.")
	fmt.Fprintf(&b, "mini_drop_migration_receipt_gc_failures_total %d\n", atomic.LoadInt64(&metricMigrationReceiptGCFailuresTotal))
	writeMetricHeader(&b, "mini_drop_storage_total_bytes", "gauge", "Total bytes of the monitored storage filesystem.")
	fmt.Fprintf(&b, "mini_drop_storage_total_bytes %d\n", atomic.LoadInt64(&metricStorageTotalBytes))
	writeMetricHeader(&b, "mini_drop_storage_available_bytes", "gauge", "Available bytes of the monitored storage filesystem.")
	fmt.Fprintf(&b, "mini_drop_storage_available_bytes %d\n", atomic.LoadInt64(&metricStorageAvailableBytes))
	writeMetricHeader(&b, "mini_drop_storage_pressure_level", "gauge", "Storage pressure level: 0=normal 1=warning 2=critical 3=emergency 4=unknown.")
	fmt.Fprintf(&b, "mini_drop_storage_pressure_level %d\n", atomic.LoadInt64(&metricStoragePressureLevel))
	writeMetricHeader(&b, "mini_drop_collection_rejected_low_disk_total", "counter", "Collection attempts rejected because of low disk, by entry source.")
	sources := make([]string, 0, len(metricCollectionRejectedLowDisk))
	metricCollectionRejectedLowDiskMu.Lock()
	for source := range metricCollectionRejectedLowDisk {
		sources = append(sources, source)
	}
	metricCollectionRejectedLowDiskMu.Unlock()
	sort.Strings(sources)
	for _, source := range sources {
		count := func() int64 {
			metricCollectionRejectedLowDiskMu.Lock()
			defer metricCollectionRejectedLowDiskMu.Unlock()
			return metricCollectionRejectedLowDisk[source]
		}()
		fmt.Fprintf(&b, "mini_drop_collection_rejected_low_disk_total{source=%q} %d\n", prometheusLabel(source), count)
	}

	if s != nil && s.DB != nil {
		s.refreshArtifactsByStatusGauge()
		s.writeDBMetrics(&b)
	}
	// 存储阶段一：Artifact 生命周期指标
	writeMetricHeader(&b, "mini_drop_artifacts_by_status", "gauge", "Current artifacts grouped by status.")
	metricArtifactsByStatusMu.Lock()
	statuses := make([]string, 0, len(metricArtifactsByStatus))
	for status := range metricArtifactsByStatus {
		statuses = append(statuses, status)
	}
	metricArtifactsByStatusMu.Unlock()
	sort.Strings(statuses)
	for _, status := range statuses {
		metricArtifactsByStatusMu.Lock()
		count := metricArtifactsByStatus[status]
		metricArtifactsByStatusMu.Unlock()
		fmt.Fprintf(&b, "mini_drop_artifacts_by_status{status=%q} %d\n", prometheusLabel(status), count)
	}
	writeMetricHeader(&b, "mini_drop_artifact_ready_bytes", "gauge", "Bytes of artifacts in ready status.")
	fmt.Fprintf(&b, "mini_drop_artifact_ready_bytes %d\n", atomic.LoadInt64(&metricArtifactReadyBytes))
	writeMetricHeader(&b, "mini_drop_artifact_pinned_bytes", "gauge", "Bytes of non-deleted artifacts under pinned tasks.")
	fmt.Fprintf(&b, "mini_drop_artifact_pinned_bytes %d\n", atomic.LoadInt64(&metricArtifactPinnedBytes))
	writeMetricHeader(&b, "mini_drop_artifact_superseded_bytes", "gauge", "Bytes of superseded generation results (result_superseded, phase 4).")
	fmt.Fprintf(&b, "mini_drop_artifact_superseded_bytes %d\n", atomic.LoadInt64(&metricArtifactSupersededBytes))
	writeMetricHeader(&b, "mini_drop_artifact_expiration_backlog", "gauge", "Artifacts due for expiration cleanup.")
	fmt.Fprintf(&b, "mini_drop_artifact_expiration_backlog %d\n", atomic.LoadInt64(&metricArtifactExpirationBacklog))
	writeMetricHeader(&b, "mini_drop_artifact_cleanup_deleted_total", "counter", "Artifacts deleted by lifecycle cleanup.")
	fmt.Fprintf(&b, "mini_drop_artifact_cleanup_deleted_total %d\n", atomic.LoadInt64(&metricArtifactCleanupDeletedTotal))
	writeMetricHeader(&b, "mini_drop_artifact_cleanup_deleted_bytes_total", "counter", "Object bytes reclaimed by lifecycle cleanup.")
	fmt.Fprintf(&b, "mini_drop_artifact_cleanup_deleted_bytes_total %d\n", atomic.LoadInt64(&metricArtifactCleanupDeletedBytesTotal))
	writeMetricHeader(&b, "mini_drop_artifact_cleanup_failures_total", "counter", "Artifact object deletion failures.")
	fmt.Fprintf(&b, "mini_drop_artifact_cleanup_failures_total %d\n", atomic.LoadInt64(&metricArtifactCleanupFailuresTotal))
	writeMetricHeader(&b, "mini_drop_artifact_policy_reconcile_backlog", "gauge", "Artifacts awaiting retention policy reconciliation.")
	fmt.Fprintf(&b, "mini_drop_artifact_policy_reconcile_backlog %d\n", atomic.LoadInt64(&metricArtifactPolicyReconcileBacklog))
	// 存储阶段二：物理 Blob 指标
	writeMetricHeader(&b, "mini_drop_blob_physical_bytes", "gauge", "Physical bytes of ready storage blobs (deduplicated).")
	fmt.Fprintf(&b, "mini_drop_blob_physical_bytes %d\n", atomic.LoadInt64(&metricBlobPhysicalBytes))
	writeMetricHeader(&b, "mini_drop_blob_logical_bytes", "gauge", "Logical (uncompressed) bytes of ready storage blobs.")
	fmt.Fprintf(&b, "mini_drop_blob_logical_bytes %d\n", atomic.LoadInt64(&metricBlobLogicalBytes))
	writeMetricHeader(&b, "mini_drop_blob_deduplicated_bytes", "gauge", "Bytes saved by content deduplication and compression.")
	fmt.Fprintf(&b, "mini_drop_blob_deduplicated_bytes %d\n", atomic.LoadInt64(&metricBlobDeduplicatedBytes))
	writeMetricHeader(&b, "mini_drop_blob_by_status", "gauge", "Storage blobs grouped by status.")
	metricBlobByStatusMu.Lock()
	blobStatuses := make([]string, 0, len(metricBlobByStatus))
	for status := range metricBlobByStatus {
		blobStatuses = append(blobStatuses, status)
	}
	metricBlobByStatusMu.Unlock()
	sort.Strings(blobStatuses)
	for _, status := range blobStatuses {
		metricBlobByStatusMu.Lock()
		count := metricBlobByStatus[status]
		metricBlobByStatusMu.Unlock()
		fmt.Fprintf(&b, "mini_drop_blob_by_status{status=%q} %d\n", prometheusLabel(status), count)
	}
	writeMetricHeader(&b, "mini_drop_blob_backfill_created_total", "counter", "Storage blobs created by backfill.")
	fmt.Fprintf(&b, "mini_drop_blob_backfill_created_total %d\n", atomic.LoadInt64(&metricBlobBackfillCreated))
	writeMetricHeader(&b, "mini_drop_blob_backfill_linked_total", "counter", "References linked to blobs by backfill.")
	fmt.Fprintf(&b, "mini_drop_blob_backfill_linked_total %d\n", atomic.LoadInt64(&metricBlobBackfillLinked))
	writeMetricHeader(&b, "mini_drop_blob_backfill_stat_failures_total", "counter", "Backfill StatObject failures.")
	fmt.Fprintf(&b, "mini_drop_blob_backfill_stat_failures_total %d\n", atomic.LoadInt64(&metricBlobBackfillStatFail))
	writeMetricHeader(&b, "mini_drop_blob_backfill_conflicts_total", "counter", "Backfill keys skipped due to conflicting sizes.")
	fmt.Fprintf(&b, "mini_drop_blob_backfill_conflicts_total %d\n", atomic.LoadInt64(&metricBlobBackfillConflicts))
	writeMetricHeader(&b, "mini_drop_blob_migration_objects_total", "counter", "Objects migrated to compressed CAS blobs.")
	fmt.Fprintf(&b, "mini_drop_blob_migration_objects_total %d\n", atomic.LoadInt64(&metricBlobMigrationObjects))
	writeMetricHeader(&b, "mini_drop_blob_migration_failures_total", "counter", "Migration failures (old references kept).")
	fmt.Fprintf(&b, "mini_drop_blob_migration_failures_total %d\n", atomic.LoadInt64(&metricBlobMigrationFailures))
	writeMetricHeader(&b, "mini_drop_blob_migration_reclaimed_bytes_total", "counter", "Bytes reclaimed by compression migration (logical - stored).")
	fmt.Fprintf(&b, "mini_drop_blob_migration_reclaimed_bytes_total %d\n", atomic.LoadInt64(&metricBlobMigrationReclaimed))
	writeMetricHeader(&b, "mini_drop_blob_migration_backlog", "gauge", "Objects remaining for migration.")
	fmt.Fprintf(&b, "mini_drop_blob_migration_backlog %d\n", atomic.LoadInt64(&metricBlobMigrationBacklog))
	writeMetricHeader(&b, "mini_drop_blob_backfill_backlog", "gauge", "References awaiting blob backfill.")
	fmt.Fprintf(&b, "mini_drop_blob_backfill_backlog %d\n", atomic.LoadInt64(&metricBlobBackfillBacklog))
	writeMetricHeader(&b, "mini_drop_blob_gc_deleted_total", "counter", "Legacy objects deleted by delayed GC.")
	fmt.Fprintf(&b, "mini_drop_blob_gc_deleted_total %d\n", atomic.LoadInt64(&metricBlobGCDeleted))
	writeMetricHeader(&b, "mini_drop_blob_gc_deleted_bytes_total", "counter", "Bytes reclaimed by delayed GC.")
	fmt.Fprintf(&b, "mini_drop_blob_gc_deleted_bytes_total %d\n", atomic.LoadInt64(&metricBlobGCDeletedBytes))
	writeMetricHeader(&b, "mini_drop_blob_gc_failures_total", "counter", "Delayed GC deletion failures.")
	fmt.Fprintf(&b, "mini_drop_blob_gc_failures_total %d\n", atomic.LoadInt64(&metricBlobGCFailures))
	writeMetricHeader(&b, "mini_drop_blob_orphan_reconciled_total", "counter", "Orphan backfill blobs tombstoned after migration.")
	fmt.Fprintf(&b, "mini_drop_blob_orphan_reconciled_total %d\n", atomic.LoadInt64(&metricBlobOrphanReconciled))
	writeMetricHeader(&b, "mini_drop_maintenance_skip_total", "counter", "Maintenance operations skipped (low disk / unknown), by kind.")
	kinds := make([]string, 0, len(metricMaintenanceSkip))
	metricMaintenanceSkipMu.Lock()
	for kind := range metricMaintenanceSkip {
		kinds = append(kinds, kind)
	}
	metricMaintenanceSkipMu.Unlock()
	sort.Strings(kinds)
	for _, kind := range kinds {
		metricMaintenanceSkipMu.Lock()
		count := metricMaintenanceSkip[kind]
		metricMaintenanceSkipMu.Unlock()
		fmt.Fprintf(&b, "mini_drop_maintenance_skip_total{kind=%q} %d\n", prometheusLabel(kind), count)
	}
	// pprof 转换结果（由 artifacts.format=pprof 状态统计）
	if s != nil && s.DB != nil {
		pprofReady, pprofFailed, pprofLogical, pprofStored := s.pprofArtifactStats()
		writeMetricHeader(&b, "mini_drop_pprof_artifacts_ready_total", "gauge", "Ready pprof artifacts (conversion success).")
		fmt.Fprintf(&b, "mini_drop_pprof_artifacts_ready_total %d\n", pprofReady)
		writeMetricHeader(&b, "mini_drop_pprof_artifacts_failed_total", "gauge", "Failed pprof artifacts (conversion failure).")
		fmt.Fprintf(&b, "mini_drop_pprof_artifacts_failed_total %d\n", pprofFailed)
		if pprofStored > 0 {
			writeMetricHeader(&b, "mini_drop_pprof_compression_ratio", "gauge", "Average pprof compression ratio (logical/stored).")
			fmt.Fprintf(&b, "mini_drop_pprof_compression_ratio %.3f\n", float64(pprofLogical)/float64(pprofStored))
		}
	}
	c.Data(http.StatusOK, "text/plain; version=0.0.4; charset=utf-8", []byte(b.String()))
}

func writeMetricHeader(b *strings.Builder, name string, typ string, help string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func (s *APIServer) writeDBMetrics(b *strings.Builder) {
	writeMetricHeader(b, "mini_drop_tasks_by_status", "gauge", "Current tasks grouped by status.")
	for status, count := range countTasksByStatus(s.DB) {
		fmt.Fprintf(b, "mini_drop_tasks_by_status{status=\"%d\"} %d\n", status, count)
	}

	writeMetricHeader(b, "mini_drop_outbox_by_status", "gauge", "Current outbox records grouped by status.")
	for status, count := range countOutboxByStatus(s.DB) {
		fmt.Fprintf(b, "mini_drop_outbox_by_status{status=\"%s\"} %d\n", prometheusLabel(status), count)
	}

	writeMetricHeader(b, "mini_drop_analysis_jobs_by_status", "gauge", "Current analysis jobs grouped by status.")
	for status, count := range countAnalysisJobsByStatus(s.DB) {
		fmt.Fprintf(b, "mini_drop_analysis_jobs_by_status{status=\"%s\"} %d\n", prometheusLabel(status), count)
	}

	writeMetricHeader(b, "mini_drop_agents_online", "gauge", "Current online agents.")
	fmt.Fprintf(b, "mini_drop_agents_online %d\n", countOnlineAgents(s.DB))
}

func prometheusLabel(raw string) string {
	raw = util.RedactSecret(raw)
	raw = strings.ReplaceAll(raw, "\\", "\\\\")
	raw = strings.ReplaceAll(raw, "\n", "")
	return strings.ReplaceAll(raw, "\"", "\\\"")
}

func countTasksByStatus(db *gorm.DB) map[int]int64 {
	rows := []struct {
		Status int
		Count  int64
	}{}
	_ = db.Model(&model.HotmethodTask{}).Select("status, count(*) as count").Group("status").Scan(&rows).Error
	result := map[int]int64{}
	for _, row := range rows {
		result[row.Status] = row.Count
	}
	return result
}

func countOutboxByStatus(db *gorm.DB) map[string]int64 {
	rows := []struct {
		Status string
		Count  int64
	}{}
	_ = db.Model(&model.Outbox{}).Select("status, count(*) as count").Group("status").Scan(&rows).Error
	result := map[string]int64{}
	for _, row := range rows {
		result[row.Status] = row.Count
	}
	return result
}

func countAnalysisJobsByStatus(db *gorm.DB) map[string]int64 {
	rows := []struct {
		Status string
		Count  int64
	}{}
	_ = db.Model(&model.AnalysisJob{}).Select("status, count(*) as count").Group("status").Scan(&rows).Error
	result := map[string]int64{}
	for _, row := range rows {
		result[row.Status] = row.Count
	}
	return result
}

func countOnlineAgents(db *gorm.DB) int64 {
	var count int64
	_ = db.Model(&model.AgentInfo{}).Where("online = ?", true).Count(&count).Error
	return count
}
