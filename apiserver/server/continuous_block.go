// ============================================================
// continuous_block.go — 阶段三：内建持续剖析块存储（compactor）
// ============================================================
// 仅借鉴 Parca/Pyroscope 的块、压缩、索引、合并和替换机制；不部署、不依赖、
// 不调用任何开源 profiling 后端。把"每分钟一个 JSON 对象"的持续采集存储
// 改为"每个 session 每小时一个 gzip 压缩块"：
//
//   - 块对象：continuous-blocks/{session}/{YYYY}/{MM}/{DD}/{HH}/{block-id}.json.gz
//     gzip 压缩 JSON，块头记录 schema/session/小时范围/版本/checksum，
//     batches 保留每个原始 batch 的完整 payload（CPU profile、io/sched 延迟
//     直方图、db_snapshot 均保持原结构，现有解析逻辑可直接复用）。
//   - compactor：每 CONTINUOUS_BLOCK_COMPACTION_INTERVAL_SEC 扫描已结束至少
//     COMPACTION_DELAY_SEC 的 UTC 小时桶；同一 session+桶通过 PostgreSQL
//     advisory lock 单飞；生成块 → 上传 → 校验 → 事务登记（block 行 +
//     batch/window 映射）→ 提交后删除源分钟对象（失败保留 source_object_key
//     由 sweep 重试）。
//   - 迟到 batch：已封存桶出现新 batch 时生成包含旧成员+新成员的新版本块，
//     旧块标记 superseded，15 分钟宽限后删除旧对象。
//   - 24h 保留：到期 batch 从块中移除（仍有未到期成员则重写替换版本，无成员
//     则删除块行+对象）；compaction 前检查根盘余量（≥1GiB 且容纳 2× 输入）。
//   - 旧 continuous/{session}/{batch}.json 保持兼容读取（热数据未压缩时仍走
//     原路径，无查询空窗）。
// ============================================================

package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/mini-drop/apiserver/model"
	"github.com/mini-drop/apiserver/util"
)

const (
	continuousBlockSchemaV1           = "continuous-block-v1"
	continuousBlockCompressionGzip    = "gzip"
	continuousBlockSupersededGrace    = 15 * time.Minute
	continuousBlockOrphanGrace        = 15 * time.Minute
	continuousBlockMaxObjectBytes     = 512 * 1024 * 1024
	continuousBlockLockPrefix         = "cblk"
	continuousBlockPrefix             = "continuous-blocks/"
	continuousBlockVersioned1         = 1
	continuousBlockMaxCandidates      = 5000
	continuousBlockMaxActiveBlocks    = 200
	continuousBlockMaxSweepBatch      = 1000
	continuousBlockMaxSweepSuperseded = 200
)

// continuousBlockBatch 块内单个原始 batch 的完整快照：payload 保持原始 JSON
// 结构（continuousStoredBatch），checksum 是 payload 原始 JSON 字节的 sha256。
type continuousBlockBatch struct {
	BatchID   string                `json:"batch_id"`
	StartTime time.Time             `json:"start_time"`
	EndTime   time.Time             `json:"end_time"`
	Checksum  string                `json:"checksum"`
	Payload   continuousStoredBatch `json:"payload"`
}

// continuousBlock 块头 + 成员列表。
type continuousBlock struct {
	Schema      string                 `json:"schema"`
	SessionSID  string                 `json:"session_sid"`
	BucketStart time.Time              `json:"bucket_start"`
	BucketEnd   time.Time              `json:"bucket_end"`
	CreatedAt   time.Time              `json:"created_at"`
	Version     int                    `json:"version"`
	Compression string                 `json:"compression"`
	Checksum    string                 `json:"checksum"`
	Batches     []continuousBlockBatch `json:"batches"`
}

type continuousBucketKey struct {
	sessionSID  string
	bucketStart time.Time
}

type continuousBucketWork struct {
	bucketStart time.Time
	bucketEnd   time.Time
	// activeBlock 非 nil 表示该桶已存在 active 块（迟到 batch / 保留重写场景）
	activeBlock *model.ContinuousProfileBlock
	// newBatches 是尚未压缩（block_id IS NULL）且未过期的候选 batch
	newBatches []model.ProfileBatch
	// preExpiredBIDs 是预检出的"已过期且无剩余 window"的块内成员 bid 集合，
	// 用于决定该桶是否值得进入锁内精确处理（避免每轮把全部块对象读一遍）。
	preExpiredBIDs map[string]bool
	// force 表示块没有任何成员行（异常/崩溃残留的孤儿块），需要进入锁内
	// 处理以便回收。
	force bool
}

// ---------------------------------------------------------------------------
// 块编解码
// ---------------------------------------------------------------------------

func continuousBlockID() string {
	return "cblk-" + util.GenTID()[4:]
}

// continuousBlockObjectKey 生成块对象 key（独立前缀，与旧分钟对象并存兼容读取）。
func continuousBlockObjectKey(sessionSID, blockID string, bucketStart time.Time) string {
	utc := bucketStart.UTC()
	return fmt.Sprintf("continuous-blocks/%s/%04d/%02d/%02d/%02d/%s.json.gz",
		sessionSID, utc.Year(), int(utc.Month()), utc.Day(), utc.Hour(), blockID)
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// buildContinuousBlock 构造 gzip 压缩的块对象字节流。每个成员 batch 先单独
// 计算 payload 校验和，块头再对整个规范化 JSON（checksum 字段置空）计算
// 整体校验和，gzip 压缩后返回。返回值第二个为压缩前 JSON 字节数
// （bytes_before / 压缩率统计用）。
func buildContinuousBlock(sessionSID string, bucketStart, bucketEnd, createdAt time.Time, version int, batches []continuousBlockBatch) ([]byte, int64, error) {
	for i := range batches {
		raw, err := json.Marshal(batches[i].Payload)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal block batch %s: %w", batches[i].BatchID, err)
		}
		batches[i].Checksum = sha256Hex(raw)
	}
	block := continuousBlock{
		Schema:      continuousBlockSchemaV1,
		SessionSID:  sessionSID,
		BucketStart: bucketStart,
		BucketEnd:   bucketEnd,
		CreatedAt:   createdAt,
		Version:     version,
		Compression: continuousBlockCompressionGzip,
		Batches:     batches,
	}
	body, err := json.Marshal(block)
	if err != nil {
		return nil, 0, err
	}
	block.Checksum = sha256Hex(body)
	body, err = json.Marshal(block)
	if err != nil {
		return nil, 0, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		return nil, 0, err
	}
	if err := zw.Close(); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), int64(len(body)), nil
}

// parseContinuousBlock 解析并校验块 JSON（schema/compression/整体 checksum）。
func parseContinuousBlock(body []byte) (*continuousBlock, error) {
	if len(body) > continuousBlockMaxObjectBytes {
		return nil, fmt.Errorf("block 超过 %d bytes 上限", continuousBlockMaxObjectBytes)
	}
	var block continuousBlock
	if err := json.Unmarshal(body, &block); err != nil {
		return nil, err
	}
	if block.Schema != continuousBlockSchemaV1 {
		return nil, fmt.Errorf("未知块 schema %q", block.Schema)
	}
	if block.Compression != "" && block.Compression != continuousBlockCompressionGzip {
		return nil, fmt.Errorf("未知块压缩格式 %q", block.Compression)
	}
	expected := block.Checksum
	block.Checksum = ""
	canonical, err := json.Marshal(block)
	if err != nil {
		return nil, err
	}
	if expected != "" && sha256Hex(canonical) != expected {
		return nil, errors.New("块 checksum 校验失败")
	}
	block.Checksum = expected
	return &block, nil
}

func gunzipBytes(body []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(io.LimitReader(zr, continuousBlockMaxObjectBytes))
}

func looksLikeContinuousBlockKey(objectKey string) bool {
	return strings.HasPrefix(objectKey, continuousBlockPrefix) ||
		strings.HasSuffix(strings.ToLower(objectKey), ".json.gz")
}

// loadContinuousBatches 统一加载器：一个对象要么是旧分钟 JSON（返回单个
// batch），要么是 gzip 块（解压一次后返回全部成员 batch）。查询侧按
// object_key 去重调用，保证一个块只解压一次。
func (s *APIServer) loadContinuousBatches(ctx context.Context, objectKey string) ([]continuousStoredBatch, error) {
	if !s.StorageConnected() {
		return nil, errProfileUnavailable
	}
	rc, err := s.Storage.GetObject(ctx, s.Config.Storage.Bucket, objectKey)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, continuousBlockMaxObjectBytes))
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("对象 %s 为空", objectKey)
	}
	if looksLikeContinuousBlockKey(objectKey) {
		raw, err := gunzipBytes(body)
		if err != nil {
			return nil, fmt.Errorf("块 %s gunzip 失败: %w", objectKey, err)
		}
		block, err := parseContinuousBlock(raw)
		if err != nil {
			return nil, fmt.Errorf("块 %s 解析失败: %w", objectKey, err)
		}
		out := make([]continuousStoredBatch, 0, len(block.Batches))
		for _, member := range block.Batches {
			out = append(out, member.Payload)
		}
		incContinuousBlocksRead()
		return out, nil
	}
	var batch continuousStoredBatch
	if err := json.Unmarshal(body, &batch); err != nil {
		return nil, err
	}
	return []continuousStoredBatch{batch}, nil
}

// loadContinuousBlockObject 直接读取并校验一个块对象（compactor 校验/重写用）。
func (s *APIServer) loadContinuousBlockObject(ctx context.Context, objectKey string) (*continuousBlock, error) {
	if !s.StorageConnected() {
		return nil, errProfileUnavailable
	}
	rc, err := s.Storage.GetObject(ctx, s.Config.Storage.Bucket, objectKey)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, continuousBlockMaxObjectBytes))
	if err != nil {
		return nil, err
	}
	raw, err := gunzipBytes(body)
	if err != nil {
		return nil, fmt.Errorf("块 %s gunzip 失败: %w", objectKey, err)
	}
	return parseContinuousBlock(raw)
}

// ---------------------------------------------------------------------------
// compactor 主循环
// ---------------------------------------------------------------------------

func (s *APIServer) startContinuousBlockCompactor() {
	if s == nil || s.DB == nil || s.Config == nil || !s.Config.ContinuousBlock.Enabled {
		return
	}
	interval := time.Duration(s.Config.ContinuousBlock.CompactionIntervalSec) * time.Second
	if interval <= 0 {
		interval = 300 * time.Second
	}
	time.Sleep(15 * time.Second)
	s.Logger.Info("continuous block compactor 已启动",
		zap.Int("window_sec", s.Config.ContinuousBlock.WindowSec),
		zap.Int("delay_sec", s.Config.ContinuousBlock.CompactionDelaySec),
		zap.Int("interval_sec", s.Config.ContinuousBlock.CompactionIntervalSec),
	)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		s.runContinuousBlockCompaction(ctx)
		s.sweepContinuousBlockCleanup(ctx)
		cancel()
	}
}

// continuousRetentionCutoff 计算 session 的原始数据保留截止时间（24h 严格，
// 历史 session 可能带 30 天上限，按阶段二约定统一收敛到 24h）。
func continuousRetentionCutoff(session model.ContinuousSession, now time.Time) time.Time {
	hours := session.RetentionHours
	if hours == 0 || hours > 24 {
		hours = 24
	}
	return now.Add(-time.Duration(hours) * time.Hour)
}

// runContinuousBlockCompaction 一轮 compaction：扫描候选桶（未压缩热数据桶 +
// 已封存桶的 active 块），逐桶单飞处理。
func (s *APIServer) runContinuousBlockCompaction(ctx context.Context) {
	cfg := s.Config.ContinuousBlock
	if s.DB == nil || !cfg.Enabled {
		return
	}
	if !s.StorageConnected() {
		s.recordContinuousBlockSkip("storage_disconnected", "")
		return
	}
	window := time.Duration(cfg.WindowSec) * time.Second
	delay := time.Duration(cfg.CompactionDelaySec) * time.Second
	now := time.Now()
	sealedCutoff := now.Add(-delay)

	// 全局磁盘底线（≥1GiB），不满足直接整轮跳过，不影响 ingest。
	if ok, reason := s.blockCompactionDiskOK(0); !ok {
		s.recordContinuousBlockSkip("low_disk:"+reason, "")
		return
	}

	sessionCutoff := map[string]time.Time{}
	{
		var sessions []model.ContinuousSession
		if err := s.DB.WithContext(ctx).Find(&sessions).Error; err != nil {
			s.Logger.Warn("block compactor: 加载 session 失败", zap.Error(err))
			return
		}
		for _, session := range sessions {
			sessionCutoff[session.SID] = continuousRetentionCutoff(session, now)
		}
	}

	buckets := map[continuousBucketKey]*continuousBucketWork{}

	// 1) 未压缩（热数据）batch 候选桶：桶已封存 且 batch 未过期。
	//    block_id 用 (IS NULL OR = '') 匹配：GORM 把 Go 空字符串存成 ''，
	//    旧行/新行可能为 NULL 或 ''，两种都算未压缩。
	var unblocked []model.ProfileBatch
	if err := s.DB.WithContext(ctx).
		Where("(block_id IS NULL OR block_id = '') AND start_time < ? AND end_time < ?", sealedCutoff, sealedCutoff).
		Order("start_time ASC").
		Limit(continuousBlockMaxCandidates).
		Find(&unblocked).Error; err != nil {
		s.Logger.Warn("block compactor: 查询未压缩 batch 失败", zap.Error(err))
		return
	}
	for i := range unblocked {
		b := &unblocked[i]
		bucketStart := b.StartTime.UTC().Truncate(window)
		if bucketStart.Add(window).After(sealedCutoff) {
			continue // 桶尚未封存
		}
		if cutoff, ok := sessionCutoff[b.SessionSID]; ok && b.EndTime.Before(cutoff) {
			continue // 已过期，交给 retention 删除，不压缩
		}
		key := continuousBucketKey{sessionSID: b.SessionSID, bucketStart: bucketStart}
		work := buckets[key]
		if work == nil {
			work = &continuousBucketWork{bucketStart: bucketStart, bucketEnd: bucketStart.Add(window), preExpiredBIDs: map[string]bool{}}
			buckets[key] = work
		}
		work.newBatches = append(work.newBatches, *b)
	}

	// 2) 只查询确实需要重写的 active 块：无 window 引用的过期
	//    成员，或完全没有成员行的孤儿登记。这样 Limit 每轮都消耗真实候选，
	//    不会被最旧的 200 个健康块永久挡住。迟到 batch 的桶已在上面
	//    建立 work，锁内会自行重读对应 active 块。
	var expirationCandidates []model.ProfileBatch
	if err := s.DB.WithContext(ctx).
		Where("block_id IS NOT NULL AND block_id <> ''").
		Where("NOT EXISTS (SELECT 1 FROM profile_windows WHERE profile_windows.batch_bid = profile_batches.bid)").
		Order("end_time ASC").
		Limit(continuousBlockMaxCandidates).
		Find(&expirationCandidates).Error; err != nil {
		s.Logger.Warn("block compactor: 查询过期块成员候选失败", zap.Error(err))
		return
	}
	expiredByBlock := map[string]map[string]bool{}
	for _, member := range expirationCandidates {
		cutoff, ok := sessionCutoff[member.SessionSID]
		if !ok || !member.EndTime.Before(cutoff) {
			continue
		}
		set := expiredByBlock[member.BlockID]
		if set == nil {
			set = map[string]bool{}
			expiredByBlock[member.BlockID] = set
		}
		set[member.BID] = true
	}
	candidateBlockIDs := make([]string, 0, len(expiredByBlock))
	for blockID := range expiredByBlock {
		candidateBlockIDs = append(candidateBlockIDs, blockID)
	}
	activeQuery := s.DB.WithContext(ctx).
		Where("status = ? AND bucket_end <= ?", model.ContinuousBlockStatusActive, sealedCutoff)
	orphanPredicate := "NOT EXISTS (SELECT 1 FROM profile_batches WHERE profile_batches.block_id = continuous_profile_blocks.block_id)"
	if len(candidateBlockIDs) > 0 {
		activeQuery = activeQuery.Where("block_id IN ? OR "+orphanPredicate, candidateBlockIDs)
	} else {
		activeQuery = activeQuery.Where(orphanPredicate)
	}
	var activeBlocks []model.ContinuousProfileBlock
	if err := activeQuery.Order("bucket_start ASC").Limit(continuousBlockMaxActiveBlocks).Find(&activeBlocks).Error; err != nil {
		s.Logger.Warn("block compactor: 查询 active 重写候选失败", zap.Error(err))
		return
	}
	for i := range activeBlocks {
		blk := &activeBlocks[i]
		key := continuousBucketKey{sessionSID: blk.SessionSID, bucketStart: blk.BucketStart}
		work := buckets[key]
		if work == nil {
			work = &continuousBucketWork{bucketStart: blk.BucketStart, bucketEnd: blk.BucketEnd, preExpiredBIDs: map[string]bool{}}
			buckets[key] = work
		}
		work.activeBlock = blk
		for bid := range expiredByBlock[blk.BlockID] {
			work.preExpiredBIDs[bid] = true
		}
		if len(expiredByBlock[blk.BlockID]) == 0 {
			work.force = true
		}
	}

	// 3) 逐桶处理（确定性顺序）
	keys := make([]continuousBucketKey, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].sessionSID == keys[j].sessionSID {
			return keys[i].bucketStart.Before(keys[j].bucketStart)
		}
		return keys[i].sessionSID < keys[j].sessionSID
	})
	for _, key := range keys {
		work := buckets[key]
		if work.activeBlock == nil && len(work.newBatches) == 0 {
			continue
		}
		if work.activeBlock != nil && len(work.newBatches) == 0 && len(work.preExpiredBIDs) == 0 && !work.force {
			continue
		}
		s.compactContinuousBucket(ctx, key, work)
	}
}

// blockCompactionDiskOK 根盘余量检查：至少保留 1GiB，且必须容纳本次输入
// 大小的两倍。inputBytes<=0 时仅检查保护线。
func (s *APIServer) blockCompactionDiskOK(inputBytes int64) (bool, string) {
	path, minFree := diskGuardConfig(s.Config)
	free, err := storageFreeBytes(path)
	if err != nil {
		return false, fmt.Sprintf("无法检查根盘 %s: %v", path, err)
	}
	if free < minFree {
		return false, fmt.Sprintf("根盘可用 %d bytes 低于保护线 %d bytes", free, minFree)
	}
	if inputBytes > 0 {
		remaining := uint64(0)
		if free > minFree {
			remaining = free - minFree
		}
		if uint64(inputBytes) > remaining/2 {
			return false, fmt.Sprintf("根盘剩余空间 %d bytes 无法容纳本次输入 %d bytes 的两倍", remaining, inputBytes)
		}
	}
	return true, ""
}

// recordContinuousBlockSkip 记录 compaction 跳过（原因 + 指标 + 日志）。
func (s *APIServer) recordContinuousBlockSkip(reason, sessionSID string) {
	incContinuousCompactionSkip()
	s.Logger.Warn("continuous block compaction 跳过",
		zap.String("reason", reason), zap.String("session_sid", sessionSID))
}

// acquireContinuousBucketLock 对 (session, 小时桶) 加 PostgreSQL 会话级
// advisory lock 实现跨实例单飞；SQLite（单测）退化为 noop。返回释放函数。
func (s *APIServer) acquireContinuousBucketLock(ctx context.Context, lockKey string) (func(), error) {
	if s.DB == nil || s.DB.Dialector.Name() != "postgres" {
		return func() {}, nil
	}
	sqlDB, err := s.DB.DB()
	if err != nil {
		return nil, err
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock(hashtext($1))", lockKey); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return func() {
		_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock(hashtext($1))", lockKey)
		_ = conn.Close()
	}, nil
}

func (s *APIServer) continuousBlockWindow() time.Duration {
	window := time.Duration(s.Config.ContinuousBlock.WindowSec) * time.Second
	if window <= 0 {
		window = time.Hour
	}
	return window
}

// compactContinuousBucket 处理单个 (session, 小时桶)：advisory lock 单飞 →
// 锁内重读状态 → 组装成员 → 磁盘检查 → 生成/上传/校验块 → 事务登记 →
// 提交后删除源分钟对象。
func (s *APIServer) compactContinuousBucket(ctx context.Context, key continuousBucketKey, work *continuousBucketWork) {
	lockKey := continuousBlockLockPrefix + "|" + key.sessionSID + "|" + key.bucketStart.UTC().Format(time.RFC3339)
	release, err := s.acquireContinuousBucketLock(ctx, lockKey)
	if err != nil {
		s.Logger.Warn("block compactor: 获取 advisory lock 失败",
			zap.String("session_sid", key.sessionSID), zap.Time("bucket_start", key.bucketStart), zap.Error(err))
		return
	}
	defer release()

	bucketEnd := key.bucketStart.Add(s.continuousBlockWindow())
	now := time.Now()
	var session model.ContinuousSession
	if err := s.DB.WithContext(ctx).Where("sid = ?", key.sessionSID).First(&session).Error; err != nil {
		s.Logger.Warn("block compactor: 桶对应 session 不存在，跳过",
			zap.String("session_sid", key.sessionSID), zap.Error(err))
		return
	}
	cutoff := continuousRetentionCutoff(session, now)

	// 锁内重读 active 块（可能已被其他实例处理）
	var active model.ContinuousProfileBlock
	activeFound := false
	if err := s.DB.WithContext(ctx).
		Where("session_sid = ? AND bucket_start = ? AND status = ?", key.sessionSID, key.bucketStart, model.ContinuousBlockStatusActive).
		First(&active).Error; err == nil {
		activeFound = true
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		s.Logger.Warn("block compactor: 查询 active 块失败",
			zap.String("session_sid", key.sessionSID), zap.Time("bucket_start", key.bucketStart), zap.Error(err))
		return
	}

	// 锁内重读未压缩 batch
	var newBatches []model.ProfileBatch
	if err := s.DB.WithContext(ctx).
		Where("session_sid = ? AND (block_id IS NULL OR block_id = '') AND start_time >= ? AND start_time < ?",
			key.sessionSID, key.bucketStart, bucketEnd).
		Order("start_time ASC").
		Find(&newBatches).Error; err != nil {
		s.Logger.Warn("block compactor: 查询未压缩 batch 失败",
			zap.String("session_sid", key.sessionSID), zap.Time("bucket_start", key.bucketStart), zap.Error(err))
		return
	}
	fresh := make([]model.ProfileBatch, 0, len(newBatches))
	for _, b := range newBatches {
		if b.EndTime.Before(cutoff) {
			continue // 已过期，交给 retention 删除
		}
		fresh = append(fresh, b)
	}

	members := []continuousBlockBatch{}
	batchIDsToUpdate := []string{}
	removedBIDs := []string{}
	type sourceDelete struct {
		key   string
		bytes uint64
	}
	sourceDeletes := []sourceDelete{}
	loadedFreshCount := 0
	var sampleCount uint64
	version := continuousBlockVersioned1
	var oldBlock *model.ContinuousProfileBlock

	if activeFound {
		oldBlock = &active
		version = active.Version + 1
		blockObj, err := s.loadContinuousBlockObject(ctx, active.ObjectKey)
		if err != nil {
			s.Logger.Warn("block compactor: 读取旧块失败，跳过该桶",
				zap.String("session_sid", key.sessionSID), zap.String("block_id", active.BlockID), zap.Error(err))
			return
		}
		// 块内成员对应的 DB 行 + window 数（精确判定过期移除）
		memberRows := map[string]model.ProfileBatch{}
		var rows []model.ProfileBatch
		if err := s.DB.WithContext(ctx).Where("block_id = ?", active.BlockID).Find(&rows).Error; err != nil {
			s.Logger.Warn("block compactor: 查询块内成员行失败",
				zap.String("block_id", active.BlockID), zap.Error(err))
			return
		}
		bids := make([]string, 0, len(rows))
		for _, r := range rows {
			memberRows[r.BID] = r
			bids = append(bids, r.BID)
		}
		windowCounts := map[string]int64{}
		if len(bids) > 0 {
			type bidCount struct {
				BatchBID string
				C        int64
			}
			var counts []bidCount
			if err := s.DB.WithContext(ctx).Model(&model.ProfileWindow{}).
				Select("batch_bid, count(*) AS c").
				Where("batch_bid IN ?", bids).
				Group("batch_bid").Scan(&counts).Error; err != nil {
				s.Logger.Warn("block compactor: 统计成员 window 数失败",
					zap.String("block_id", active.BlockID), zap.Error(err))
				return
			}
			for _, row := range counts {
				windowCounts[row.BatchBID] = row.C
			}
		}
		for _, mb := range blockObj.Batches {
			row, exists := memberRows[mb.BatchID]
			expired := !exists || (row.EndTime.Before(cutoff) && windowCounts[mb.BatchID] == 0)
			if expired {
				// 行不存在说明此前已被清理，只需从块中移除；
				// 行存在则删除行（无剩余 window，冷层摘要已就绪）。
				removedBIDs = append(removedBIDs, mb.BatchID)
				continue
			}
			members = append(members, mb)
			batchIDsToUpdate = append(batchIDsToUpdate, mb.BatchID)
			if row, exists := memberRows[mb.BatchID]; exists {
				sampleCount = addContinuousCount(sampleCount, row.SampleCount)
			}
		}
	}
	for _, b := range fresh {
		payload, err := s.loadContinuousStoredBatch(ctx, b.ObjectKey)
		if err != nil {
			s.Logger.Warn("block compactor: 读取新 batch 源对象失败，跳过该 batch",
				zap.String("session_sid", key.sessionSID), zap.String("batch_id", b.BID), zap.String("object_key", b.ObjectKey), zap.Error(err))
			continue
		}
		members = append(members, continuousBlockBatch{
			BatchID: b.BID, StartTime: b.StartTime, EndTime: b.EndTime, Payload: payload,
		})
		batchIDsToUpdate = append(batchIDsToUpdate, b.BID)
		sourceDeletes = append(sourceDeletes, sourceDelete{key: b.ObjectKey, bytes: b.PayloadBytes})
		loadedFreshCount++
		sampleCount = addContinuousCount(sampleCount, b.SampleCount)
	}

	// 无事可做
	if len(members) == 0 && oldBlock == nil {
		return
	}
	// 旧块存在且所有成员均已过期 → 删除块
	if len(members) == 0 && oldBlock != nil {
		s.deleteContinuousBlock(ctx, oldBlock, removedBIDs)
		return
	}
	// 无新 batch 且无成员移除 → 无需重写
	if loadedFreshCount == 0 && len(removedBIDs) == 0 {
		return
	}

	// 磁盘检查：至少保留 1GiB 且容纳本次输入两倍
	inputBytes := int64(0)
	for _, src := range sourceDeletes {
		inputBytes += int64(src.bytes)
	}
	if oldBlock != nil {
		inputBytes += oldBlock.BytesAfter
	}
	if ok, reason := s.blockCompactionDiskOK(inputBytes); !ok {
		s.recordContinuousBlockSkip("low_disk:"+reason, key.sessionSID)
		return
	}

	// 生成、上传、校验
	blockID := continuousBlockID()
	blockKey := continuousBlockObjectKey(key.sessionSID, blockID, key.bucketStart)
	compressed, rawSize, err := buildContinuousBlock(key.sessionSID, key.bucketStart, bucketEnd, now, version, members)
	if err != nil {
		s.Logger.Warn("block compactor: 生成块失败",
			zap.String("session_sid", key.sessionSID), zap.String("block_id", blockID), zap.Error(err))
		return
	}
	if err := s.Storage.PutObject(ctx, s.Config.Storage.Bucket, blockKey,
		bytes.NewReader(compressed), int64(len(compressed)), "application/gzip"); err != nil {
		s.Logger.Warn("block compactor: 上传块失败",
			zap.String("session_sid", key.sessionSID), zap.String("block_id", blockID), zap.Error(err))
		return
	}
	verified, err := s.loadContinuousBlockObject(ctx, blockKey)
	if err != nil || verified == nil || len(verified.Batches) != len(members) {
		_ = s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, blockKey)
		s.Logger.Warn("block compactor: 块校验失败，已清理半成品",
			zap.String("session_sid", key.sessionSID), zap.String("block_id", blockID),
			zap.Int("verified", func() int {
				if verified == nil {
					return -1
				}
				return len(verified.Batches)
			}()), zap.Int("expected", len(members)), zap.Error(err))
		return
	}

	// 压缩前大小用块 JSON 的实际字节数（旧 batch 没有 payload_bytes 也能
	// 得到准确压缩率）；磁盘余量检查仍用上面的估算（构建前无法预知精确值）。
	bytesBefore := rawSize
	bytesAfter := int64(len(compressed))

	// 事务登记：block 行 + batch/window 映射 + 移除过期成员 + 旧块 superseded
	err = s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		block := model.ContinuousProfileBlock{
			BlockID:       blockID,
			SessionSID:    key.sessionSID,
			BucketStart:   key.bucketStart,
			BucketEnd:     bucketEnd,
			ObjectKey:     blockKey,
			Compression:   continuousBlockCompressionGzip,
			SchemaVersion: 1,
			Version:       version,
			Status:        model.ContinuousBlockStatusActive,
			BatchCount:    len(members),
			SampleCount:   sampleCount,
			BytesBefore:   bytesBefore,
			BytesAfter:    bytesAfter,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		// 唯一索引要求每个桶最多一个 active 块：同一事务内
		// 先退役旧块再插入新块；任一步失败都会整体回滚。
		if oldBlock != nil {
			result := tx.Model(&model.ContinuousProfileBlock{}).
				Where("block_id = ? AND status = ?", oldBlock.BlockID, model.ContinuousBlockStatusActive).
				Updates(map[string]interface{}{
					"status": model.ContinuousBlockStatusSuperseded, "superseded_at": now,
					"replaced_by": blockID, "updated_at": now,
				})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return fmt.Errorf("旧 active 块 %s 状态已变更", oldBlock.BlockID)
			}
		}
		if err := tx.Create(&block).Error; err != nil {
			return err
		}
		if len(batchIDsToUpdate) > 0 {
			var rows []model.ProfileBatch
			if err := tx.Where("bid IN ?", batchIDsToUpdate).Find(&rows).Error; err != nil {
				return err
			}
			for i := range rows {
				updates := map[string]interface{}{
					"block_id":   blockID,
					"object_key": blockKey,
				}
				if rows[i].SourceObjectKey == "" && rows[i].CompactedAt == nil {
					updates["source_object_key"] = rows[i].ObjectKey
					updates["compacted_at"] = now
				}
				if err := tx.Model(&rows[i]).Updates(updates).Error; err != nil {
					return err
				}
			}
			if err := tx.Model(&model.ProfileWindow{}).
				Where("batch_bid IN ?", batchIDsToUpdate).
				Update("object_key", blockKey).Error; err != nil {
				return err
			}
		}
		if len(removedBIDs) > 0 {
			if err := tx.Where("bid IN ?", removedBIDs).Delete(&model.ProfileBatch{}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		cleanupErr := s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, blockKey)
		s.Logger.Warn("block compactor: 登记块事务失败，已尝试清理未登记对象",
			zap.String("session_sid", key.sessionSID), zap.String("block_id", blockID),
			zap.Error(err), zap.NamedError("cleanup_error", cleanupErr))
		return
	}

	// 提交后：删除新压缩 batch 的源分钟对象；失败保留 source_object_key 由 sweep 重试
	for _, src := range sourceDeletes {
		if err := s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, src.key); err != nil {
			incContinuousSourceDeleteRetry()
			s.Logger.Warn("block compactor: 删除源分钟对象失败，sweep 将重试",
				zap.String("session_sid", key.sessionSID), zap.String("object_key", src.key), zap.Error(err))
			continue
		}
		s.DB.WithContext(ctx).Model(&model.ProfileBatch{}).
			Where("source_object_key = ? AND object_key = ?", src.key, blockKey).
			Update("source_object_key", "")
		incContinuousReclaimedBytes(int64(src.bytes))
	}

	incContinuousBlocksCreated(version > continuousBlockVersioned1)
	ratio := 0.0
	if bytesBefore > 0 {
		ratio = float64(bytesAfter) / float64(bytesBefore)
	}
	s.Logger.Info("continuous block compacted",
		zap.String("session_sid", key.sessionSID),
		zap.String("block_id", blockID),
		zap.Time("bucket_start", key.bucketStart),
		zap.Int("version", version),
		zap.Int("batches", len(members)),
		zap.Int64("bytes_before", bytesBefore),
		zap.Int64("bytes_after", bytesAfter),
		zap.Float64("compression_ratio", ratio),
		zap.Int("removed_batches", len(removedBIDs)),
		zap.Int("new_batches", loadedFreshCount),
	)
}

// deleteContinuousBlock 块内已无成员时安全删除：先在事务内将块标记
// deleting 并删除无 window 引用的 batch 行，再删对象，最后删块登记。
// 任一后续步骤失败，sweep 都能根据 deleting 行幂等重试，不会留下
// active 数据库引用指向已删除的对象。
func (s *APIServer) deleteContinuousBlock(ctx context.Context, blk *model.ContinuousProfileBlock, removedBIDs []string) {
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if len(removedBIDs) > 0 {
			if err := tx.Where("bid IN ?", removedBIDs).Delete(&model.ProfileBatch{}).Error; err != nil {
				return err
			}
		}
		result := tx.Model(&model.ContinuousProfileBlock{}).
			Where("block_id = ? AND status = ?", blk.BlockID, model.ContinuousBlockStatusActive).
			Updates(map[string]interface{}{
				"status": model.ContinuousBlockStatusDeleting, "updated_at": time.Now(),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("active 块 %s 状态已变更", blk.BlockID)
		}
		return nil
	})
	if err != nil {
		s.Logger.Warn("block compactor: 标记空块 deleting 失败，对象保持不变",
			zap.String("session_sid", blk.SessionSID), zap.String("block_id", blk.BlockID), zap.Error(err))
		return
	}
	if err := s.deleteContinuousBlockObjectAndRow(ctx, blk); err != nil {
		s.Logger.Warn("block compactor: 回收 deleting 空块失败，sweep 将重试",
			zap.String("session_sid", blk.SessionSID), zap.String("block_id", blk.BlockID), zap.Error(err))
		return
	}
	incContinuousReclaimedBytes(blk.BytesAfter)
	s.Logger.Info("continuous block 已清空删除",
		zap.String("session_sid", blk.SessionSID),
		zap.String("block_id", blk.BlockID),
		zap.Int("version", blk.Version),
		zap.Int64("bytes_after", blk.BytesAfter),
		zap.Int("removed_batches", len(removedBIDs)),
	)
}

func (s *APIServer) deleteContinuousBlockObjectAndRow(ctx context.Context, blk *model.ContinuousProfileBlock) error {
	if err := s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, blk.ObjectKey); err != nil {
		return err
	}
	return s.DB.WithContext(ctx).
		Where("block_id = ? AND status = ?", blk.BlockID, model.ContinuousBlockStatusDeleting).
		Delete(&model.ContinuousProfileBlock{}).Error
}

// ---------------------------------------------------------------------------
// sweep：源对象删除重试 + superseded 块对象宽限删除
// ---------------------------------------------------------------------------

func (s *APIServer) sweepContinuousBlockCleanup(ctx context.Context) {
	if s.DB == nil || !s.StorageConnected() {
		return
	}
	// 1) 源分钟对象删除失败重试：确认删除后清空 source_object_key
	var pending []model.ProfileBatch
	if err := s.DB.WithContext(ctx).
		Where("source_object_key IS NOT NULL AND source_object_key <> '' AND block_id IS NOT NULL").
		Limit(continuousBlockMaxSweepBatch).
		Find(&pending).Error; err != nil {
		s.Logger.Warn("block sweep: 查询待删源对象失败", zap.Error(err))
		return
	}
	for _, b := range pending {
		if err := s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, b.SourceObjectKey); err != nil {
			incContinuousSourceDeleteRetry()
			s.Logger.Warn("block sweep: 源对象删除重试失败",
				zap.String("session_sid", b.SessionSID), zap.String("object_key", b.SourceObjectKey), zap.Error(err))
			continue
		}
		s.DB.WithContext(ctx).Model(&model.ProfileBatch{}).
			Where("bid = ? AND source_object_key = ?", b.BID, b.SourceObjectKey).
			Update("source_object_key", "")
		incContinuousReclaimedBytes(int64(b.PayloadBytes))
	}

	// 2) deleting 块：引用已在事务中解除，幂等重试对象与登记删除。
	var deleting []model.ContinuousProfileBlock
	if err := s.DB.WithContext(ctx).Where("status = ?", model.ContinuousBlockStatusDeleting).
		Limit(continuousBlockMaxSweepSuperseded).Find(&deleting).Error; err != nil {
		s.Logger.Warn("block sweep: 查询 deleting 块失败", zap.Error(err))
		return
	}
	for i := range deleting {
		blk := &deleting[i]
		if err := s.deleteContinuousBlockObjectAndRow(ctx, blk); err != nil {
			s.Logger.Warn("block sweep: 回收 deleting 块失败，下轮重试",
				zap.String("session_sid", blk.SessionSID), zap.String("block_id", blk.BlockID), zap.Error(err))
			continue
		}
		incContinuousReclaimedBytes(blk.BytesAfter)
	}

	// 3) superseded 块对象：15 分钟宽限后删除对象与行
	graceCutoff := time.Now().Add(-continuousBlockSupersededGrace)
	var superseded []model.ContinuousProfileBlock
	if err := s.DB.WithContext(ctx).
		Where("status = ? AND superseded_at IS NOT NULL AND superseded_at < ?",
			model.ContinuousBlockStatusSuperseded, graceCutoff).
		Limit(continuousBlockMaxSweepSuperseded).
		Find(&superseded).Error; err != nil {
		s.Logger.Warn("block sweep: 查询 superseded 块失败", zap.Error(err))
		return
	}
	for _, blk := range superseded {
		if err := s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, blk.ObjectKey); err != nil {
			s.Logger.Warn("block sweep: 删除 superseded 块对象失败，下轮重试",
				zap.String("session_sid", blk.SessionSID), zap.String("block_id", blk.BlockID), zap.Error(err))
			continue
		}
		if err := s.DB.WithContext(ctx).Where("block_id = ?", blk.BlockID).
			Delete(&model.ContinuousProfileBlock{}).Error; err != nil {
			s.Logger.Warn("block sweep: 删除 superseded 块登记失败",
				zap.String("session_sid", blk.SessionSID), zap.String("block_id", blk.BlockID), zap.Error(err))
			continue
		}
		incContinuousReclaimedBytes(blk.BytesAfter)
		s.Logger.Info("superseded continuous block 对象已回收",
			zap.String("session_sid", blk.SessionSID),
			zap.String("block_id", blk.BlockID),
			zap.Int("version", blk.Version),
			zap.String("replaced_by", blk.ReplacedBy),
			zap.Int64("bytes_after", blk.BytesAfter),
		)
	}

	// 4) 未登记块对象：只删除超过宽限期且不在任何块登记中的
	//    continuous-blocks/ 对象，避免其他实例正处于上传与事务提交之间时被误删。
	objects, err := s.Storage.ListObjects(ctx, s.Config.Storage.Bucket, continuousBlockPrefix)
	if err != nil {
		s.Logger.Warn("block sweep: 列出块对象失败", zap.Error(err))
		return
	}
	var registered []string
	if err := s.DB.WithContext(ctx).Model(&model.ContinuousProfileBlock{}).Pluck("object_key", &registered).Error; err != nil {
		s.Logger.Warn("block sweep: 查询已登记块对象失败", zap.Error(err))
		return
	}
	registeredSet := make(map[string]struct{}, len(registered))
	for _, key := range registered {
		registeredSet[key] = struct{}{}
	}
	orphanCutoff := time.Now().Add(-continuousBlockOrphanGrace)
	deleted := 0
	for _, object := range objects {
		if deleted >= continuousBlockMaxSweepSuperseded || object.LastModified.After(orphanCutoff) {
			continue
		}
		if _, ok := registeredSet[object.Name]; ok {
			continue
		}
		if err := s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, object.Name); err != nil {
			s.Logger.Warn("block sweep: 删除未登记块对象失败",
				zap.String("object_key", object.Name), zap.Error(err))
			continue
		}
		deleted++
		s.Logger.Info("未登记 continuous block 对象已回收",
			zap.String("object_key", object.Name), zap.Int64("bytes_after", object.Size))
	}
}

// ---------------------------------------------------------------------------
// 供其他文件复用的小工具
// ---------------------------------------------------------------------------

// continuousWindowRowsForBatchSet 把一组 DB window 行按 object_key 分组并
// 保序，返回 objectOrder 与 byObject；查询侧按 object_key + batch_bid 去重
// 加载块后，只处理 DB 行选中的 batch。
func continuousGroupWindowsByObject(windows []model.ProfileWindow) ([]string, map[string][]model.ProfileWindow) {
	byObject := map[string][]model.ProfileWindow{}
	objectOrder := []string{}
	for _, window := range windows {
		if window.ObjectKey == "" {
			continue
		}
		if _, ok := byObject[window.ObjectKey]; !ok {
			objectOrder = append(objectOrder, window.ObjectKey)
		}
		byObject[window.ObjectKey] = append(byObject[window.ObjectKey], window)
	}
	return objectOrder, byObject
}

// continuousBatchByID 构造 block 内 batch_id → batch 的索引，供查询侧选择
// 关联 batch。返回值 map 的值是切片元素指针（不拷贝 payload）。
func continuousBatchIndex(batches []continuousStoredBatch) map[string]*continuousStoredBatch {
	out := make(map[string]*continuousStoredBatch, len(batches))
	for i := range batches {
		out[batches[i].BatchID] = &batches[i]
	}
	return out
}

// continuousRowBid 返回 DB window 行应匹配的 batch id：优先 batch_bid，
// 旧数据 batch_bid 为空时若对象只有单个 batch 则退化为该 batch id。
func continuousRowBid(row model.ProfileWindow, batches []continuousStoredBatch) string {
	if row.BatchBID != "" {
		return row.BatchBID
	}
	if len(batches) == 1 {
		return batches[0].BatchID
	}
	return ""
}

// continuousResolveBatch 解析 DB window 行对应的块内 batch：优先按 batch_bid
// 精确匹配；旧数据（DB 行与 payload 的 batch id 均为空、对象只有单个 batch）
// 退化为该唯一 batch。返回 (batch, 去重 key, 是否可用)。
func continuousResolveBatch(row model.ProfileWindow, batches []continuousStoredBatch, batchByID map[string]*continuousStoredBatch) (*continuousStoredBatch, string, bool) {
	if row.BatchBID != "" {
		batch := batchByID[row.BatchBID]
		if batch == nil {
			return nil, "", false
		}
		return batch, row.BatchBID, true
	}
	if len(batches) == 1 {
		return &batches[0], "\x00legacy-single", true
	}
	return nil, "", false
}
