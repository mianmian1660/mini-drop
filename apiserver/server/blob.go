// ============================================================
// server/blob.go — 存储阶段二：物理 Blob 核心
// ============================================================
// 实现：
//   - 内容寻址 CAS key 构造（与 analysis 侧 pprof_builder/descriptor 保持一致）
//   - 逻辑 key → 物理 key 解析（Blob resolver，Release A 兼容 Reader）
//   - 多表引用计数（artifacts / symbol_files / kernel_symbol_files）
//   - 统计与指标（物理/逻辑容量、去重收益、各状态计数）
//
// 读写兼容规则：
//   - blob_id != NULL：物理对象在 storage_blobs.object_key。
//   - blob_id == NULL：兼容读取原 object_key。
//   - 根据 blob.compression 解压内部消费内容；SVG/folded 等浏览器资源
//     通过 content_encoding=gzip 透明展示；pprof 作为文件格式保持 .gz 原样。
// ============================================================

package server

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/mini-drop/apiserver/model"
	"github.com/mini-drop/apiserver/util"
)

// ------------------------------------------------------------
// CAS key
// ------------------------------------------------------------

// blobCASKey 构造内容寻址物理对象 key。
// 格式：blobs/sha256/<ab>/<logical_sha256>/<format>-v<schema><ext>
//   - ext：pprof→".pb.gz"（格式自带扩展名）；gzip→".gz"；zstd→".zst"；未压缩→""。
// 该格式与 analysis/artifact_descriptor.py 的 blob_cas_key 保持一致（跨端契约）。
func blobCASKey(logicalSHA256, format, schemaVersion, compression string) string {
	if len(logicalSHA256) < 2 {
		logicalSHA256 = "00"
	}
	ext := ""
	switch {
	case format == model.BlobFormatPprof:
		ext = ".pb.gz"
	case compression == model.CompressionGzip:
		ext = ".gz"
	case compression == model.CompressionZstd:
		ext = ".zst"
	}
	schema := schemaVersion
	if schema == "" {
		schema = model.BlobSchemaV1
	}
	// format 未提供时用 unknown 兜底，保证 key 仍是确定性的。
	if format == "" {
		format = "unknown"
	}
	return fmt.Sprintf("blobs/sha256/%s/%s/%s-v%s%s",
		logicalSHA256[:2], logicalSHA256, format, schema, ext)
}

// blobFormatFromKey 从对象/逻辑 key 推断内容格式（best effort）。
func blobFormatFromKey(key string) string {
	lower := strings.ToLower(key)
	switch {
	case strings.Contains(lower, "kallsyms"):
		return model.BlobFormatKallsyms
	case strings.HasSuffix(lower, ".svg"):
		return model.BlobFormatSVG
	case strings.HasSuffix(lower, "folded.txt") || strings.HasSuffix(lower, ".collapsed"):
		return model.BlobFormatFolded
	case strings.HasSuffix(lower, ".pb.gz"):
		return model.BlobFormatPprof
	case strings.HasPrefix(lower, "symbols/"):
		return model.BlobFormatELF
	case strings.HasSuffix(lower, "perf.data"):
		return model.BlobFormatPerfData
	case strings.HasSuffix(lower, ".json"):
		return model.BlobFormatJSON
	case strings.HasSuffix(lower, ".md"):
		return model.BlobFormatMarkdown
	default:
		return model.BlobFormatUnknown
	}
}

// blobCompressionFromKey 按 key 后缀推断压缩（历史对象不重新计算内容时用）。
func blobCompressionFromKey(key string) string {
	lower := strings.ToLower(key)
	switch {
	case strings.HasSuffix(lower, ".gz"):
		return model.CompressionGzip
	case strings.HasSuffix(lower, ".zst"):
		return model.CompressionZstd
	default:
		return model.CompressionNone
	}
}

// transparentContentEncoding 浏览器资源是否需要透明 gzip 解码。
// SVG/folded 返回 "gzip"；pprof 作为文件格式本身保持 gzip，不透明解压。
func transparentContentEncoding(format, compression string) string {
	if compression != model.CompressionGzip {
		return ""
	}
	switch format {
	case model.BlobFormatSVG, model.BlobFormatFolded, model.BlobFormatJSON, model.BlobFormatMarkdown:
		return "gzip"
	default:
		return ""
	}
}

// ------------------------------------------------------------
// Blob resolver：逻辑 key → 物理 key
// ------------------------------------------------------------

// resolvedBlob 一次解析结果。
type resolvedBlob struct {
	// PhysicalKey 实际读取的 MinIO 对象 key。
	PhysicalKey string
	// Blob 非 nil 表示命中 storage_blobs（含压缩/格式信息）。
	Blob *model.StorageBlob
	// Logical 表示调用方传入的 key 是逻辑名称（已解析到物理 key）。
	Logical bool
	// viaTable 解析来源表（仅调试）。
	viaTable string
}

// resolveBlobForKey 解析读取 key：
//  1. key 本身就是物理对象 key（storage_blobs.object_key 命中）→ 原样返回。
//  2. key 是逻辑名称：在 artifacts/symbol_files/kernel_symbol_files 中按
//     object_key 找 blob_id → 解析到物理 key。
//  3. 都未命中 → 把 key 当物理 key 使用（兼容历史/直接 MinIO key 调用）。
//
// 说明：解析优先非 deleted 引用；墓碑不允许复活，故不会返回 deleted blob。
func (s *APIServer) resolveBlobForKey(ctx context.Context, key string) resolvedBlob {
	if s == nil || s.DB == nil || key == "" {
		return resolvedBlob{PhysicalKey: key}
	}
	// 1) 物理 key 直命中
	var direct model.StorageBlob
	if err := s.DB.WithContext(ctx).
		Where("object_key = ? AND deleted_at IS NULL", key).
		First(&direct).Error; err == nil {
		return resolvedBlob{PhysicalKey: key, Blob: &direct}
	}

	// 2) 逻辑 key → blob_id
	var blobID *uint
	var viaTable string
	var a model.Artifact
	if err := s.DB.WithContext(ctx).
		Where("object_key = ? AND blob_id IS NOT NULL AND deleted_at IS NULL AND status <> ?", key, model.ArtifactStatusDeleted).
		Order("id ASC").Limit(1).First(&a).Error; err == nil {
		blobID, viaTable = a.BlobID, "artifacts"
	}
	if blobID == nil {
		var sf model.SymbolFile
		if err := s.DB.WithContext(ctx).
			Where("object_key = ? AND blob_id IS NOT NULL", key).
			Limit(1).First(&sf).Error; err == nil {
			blobID, viaTable = sf.BlobID, "symbol_files"
		}
	}
	if blobID == nil {
		var kf model.KernelSymbolFile
		if err := s.DB.WithContext(ctx).
			Where("object_key = ? AND blob_id IS NOT NULL", key).
			Limit(1).First(&kf).Error; err == nil {
			blobID, viaTable = kf.BlobID, "kernel_symbol_files"
		}
	}
	if blobID == nil {
		return resolvedBlob{PhysicalKey: key}
	}
	var blob model.StorageBlob
	if err := s.DB.WithContext(ctx).
		Where("id = ? AND deleted_at IS NULL", *blobID).
		First(&blob).Error; err != nil {
		return resolvedBlob{PhysicalKey: key}
	}
	return resolvedBlob{PhysicalKey: blob.ObjectKey, Blob: &blob, Logical: true, viaTable: viaTable}
}

// viaTable 记录解析来源（仅调试用）。
func (r resolvedBlob) String() string {
	if r.Blob == nil {
		return r.PhysicalKey
	}
	return r.PhysicalKey + " (blob=" + fmt.Sprint(r.Blob.ID) + ")"
}

// ------------------------------------------------------------
// 引用计数（不持久化 ref_count，始终实时计算）
// ------------------------------------------------------------

// countBlobRefs 统计一个 Blob 的全部有效引用：
//   - artifacts：blob_id 指向且未删除（deleting/deleted 不算有效引用）
//   - symbol_files / kernel_symbol_files：blob_id 指向
func (s *APIServer) countBlobRefs(ctx context.Context, blobID uint) (int64, error) {
	if s == nil || s.DB == nil || blobID == 0 {
		return 0, nil
	}
	var total int64
	q := s.DB.WithContext(ctx)
	if err := q.Model(&model.Artifact{}).
		Where("blob_id = ? AND deleted_at IS NULL AND status NOT IN ?", blobID,
			[]string{model.ArtifactStatusDeleting, model.ArtifactStatusDeleted}).
		Count(&total).Error; err != nil {
		return 0, err
	}
	var n2, n3 int64
	if err := q.Model(&model.SymbolFile{}).Where("blob_id = ?", blobID).Count(&n2).Error; err != nil {
		return 0, err
	}
	if err := q.Model(&model.KernelSymbolFile{}).Where("blob_id = ?", blobID).Count(&n3).Error; err != nil {
		return 0, err
	}
	return total + n2 + n3, nil
}

// ------------------------------------------------------------
// Blob 统计与指标
// ------------------------------------------------------------

type blobStats struct {
	// BlobCount/BlobPhysicalBytes/BlobLogicalBytes 只统计 ready 的物理 blob。
	BlobCount          int64             `json:"blob_count"`
	BlobPhysicalBytes  int64             `json:"blob_physical_bytes"`
	BlobLogicalBytes   int64             `json:"blob_logical_bytes"`
	BlobDedupBytes     int64             `json:"deduplicated_bytes"`
	ByStatusCount      map[string]int64  `json:"blob_by_status_count"`
	ByStatusBytes      map[string]int64  `json:"blob_by_status_bytes"`
	Backlog            int64             `json:"migration_backlog"`
	BackfillBacklog    int64             `json:"backfill_backlog"`
	FailedObjects      int64             `json:"migration_failures"`
	ReclaimedBytes     int64             `json:"migration_reclaimed_bytes"`
	LastRunAt          *time.Time        `json:"migration_last_run_at"`
	LastError          string            `json:"migration_last_error"`
	MaintenanceAllowed bool              `json:"maintenance_allowed"`
}

type blobRuntimeState struct {
	mu          sync.Mutex
	stats       blobStats
	lastRunAt   *time.Time
	lastError   string
	failedCount int64
	reclaimed   int64
}

var blobState blobRuntimeState

func (s *APIServer) blobMaintenanceAllowed() bool {
	if s == nil || s.Config == nil {
		return false
	}
	snap := s.currentStorageSnapshot()
	return snap.Level != StoragePressureEmergency && snap.Level != StoragePressureUnknown
}

func (s *APIServer) setBlobLastRun(at *time.Time) {
	blobState.mu.Lock()
	if at != nil {
		blobState.lastRunAt = at
	}
	blobState.mu.Unlock()
}

func (s *APIServer) setBlobError(message string) {
	blobState.mu.Lock()
	blobState.lastError = truncateString(message, 1024)
	blobState.mu.Unlock()
}

func (s *APIServer) incBlobFailedObjects(n int64) {
	blobState.mu.Lock()
	blobState.failedCount += n
	blobState.mu.Unlock()
}

func (s *APIServer) incBlobReclaimedBytes(n int64) {
	blobState.mu.Lock()
	blobState.reclaimed += n
	blobState.mu.Unlock()
}

// collectBlobStats 实时统计（每次状态接口/指标刷新时查询）。
func (s *APIServer) collectBlobStats(ctx context.Context) blobStats {
	stats := blobStats{
		ByStatusCount:      map[string]int64{},
		ByStatusBytes:      map[string]int64{},
		MaintenanceAllowed: s.blobMaintenanceAllowed(),
	}
	if s == nil || s.DB == nil {
		return stats
	}
	type cnt struct {
		Status string
		Cnt    int64
		Bytes  int64
	}
	var rows []cnt
	_ = s.DB.WithContext(ctx).Model(&model.StorageBlob{}).
		Select("status, count(*) as cnt, COALESCE(SUM(stored_size),0) as bytes").
		Group("status").Scan(&rows).Error
	for _, r := range rows {
		stats.ByStatusCount[r.Status] = r.Cnt
		stats.ByStatusBytes[r.Status] = r.Bytes
		if r.Status == model.BlobStatusReady {
			stats.BlobCount += r.Cnt
			stats.BlobPhysicalBytes += r.Bytes
		}
	}
	_ = s.DB.WithContext(ctx).Model(&model.StorageBlob{}).
		Where("status = ? AND deleted_at IS NULL", model.BlobStatusReady).
		Select("COALESCE(SUM(logical_size),0) as bytes").
		Scan(&stats.BlobLogicalBytes).Error
	// 去重收益：逻辑内容总量（各引用视角）减去物理字节。
	var artifactLogical int64
	_ = s.DB.WithContext(ctx).Model(&model.Artifact{}).
		Where("deleted_at IS NULL AND status NOT IN ?", []string{model.ArtifactStatusDeleting, model.ArtifactStatusDeleted}).
		Select("COALESCE(SUM(CASE WHEN logical_size > 0 THEN logical_size ELSE size END),0) as bytes").
		Scan(&artifactLogical).Error
	if delta := artifactLogical - stats.BlobPhysicalBytes; delta > 0 {
		stats.BlobDedupBytes = delta
	}
	// 迁移 backlog：尚未迁移/回填的对象数
	var backfillBacklog int64
	_ = s.DB.WithContext(ctx).Model(&model.Artifact{}).
		Where("blob_id IS NULL AND deleted_at IS NULL AND status NOT IN ?", []string{model.ArtifactStatusDeleting, model.ArtifactStatusDeleted}).
		Count(&backfillBacklog).Error
	stats.BackfillBacklog = backfillBacklog
	stats.Backlog = s.blobMigrationBacklog(ctx)
	stats.FailedObjects = blobState.failedCount
	stats.ReclaimedBytes = blobState.reclaimed
	if blobState.lastRunAt != nil {
		t := *blobState.lastRunAt
		stats.LastRunAt = &t
	}
	stats.LastError = blobState.lastError
	return stats
}

// blobMigrationBacklog 迁移剩余候选数（kallsyms/ELF/SVG/folded 未迁移对象）。
// 候选 = 尚未回填（blob_id 空）或 blob 仍指向旧物理 key（非 CAS key）。
func (s *APIServer) blobMigrationBacklog(ctx context.Context) int64 {
	if s == nil || s.DB == nil {
		return 0
	}
	casPrefix := "blobs/sha256/%"
	var n int64
	_ = s.DB.WithContext(ctx).Model(&model.KernelSymbolFile{}).
		Joins("LEFT JOIN storage_blobs b ON b.id = kernel_symbol_files.blob_id").
		Where("kernel_symbol_files.object_key LIKE '%/kallsyms' AND kernel_symbol_files.object_key NOT LIKE '%.gz'").
		Where("(kernel_symbol_files.blob_id IS NULL OR b.object_key NOT LIKE ?)", casPrefix).
		Count(&n).Error
	var n2 int64
	_ = s.DB.WithContext(ctx).Model(&model.SymbolFile{}).
		Joins("LEFT JOIN storage_blobs b ON b.id = symbol_files.blob_id").
		Where("(symbol_files.blob_id IS NULL OR b.object_key NOT LIKE ?)", casPrefix).
		Count(&n2).Error
	var n3 int64
	_ = s.DB.WithContext(ctx).Model(&model.Artifact{}).
		Joins("LEFT JOIN hotmethod_tasks t ON t.tid = artifacts.task_tid").
		Joins("LEFT JOIN storage_blobs b ON b.id = artifacts.blob_id").
		Where("artifacts.kind IN ? AND artifacts.size >= ? AND artifacts.deleted_at IS NULL AND artifacts.status NOT IN ?",
			[]string{model.ArtifactKindResult, model.ArtifactKindIntermediate},
			s.Config.Blob.MinCompressBytes,
			[]string{model.ArtifactStatusDeleting, model.ArtifactStatusDeleted}).
		Where("(artifacts.blob_id IS NULL OR b.object_key NOT LIKE ?)", casPrefix).
		Count(&n3).Error
	return n + n2 + n3
}

// refreshBlobMetrics 刷新 gauge 指标并缓存统计（生命周期/迁移周期内调用）。
func (s *APIServer) refreshBlobMetrics(ctx context.Context) {
	if s == nil || s.DB == nil {
		return
	}
	stats := s.collectBlobStats(ctx)
	updateBlobGauges(stats)
}

// logBlobState 结构日志（迁移 worker 状态变化用）。
func (s *APIServer) logBlobState(ev string, fields ...zap.Field) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Info("blob "+ev, fields...)
}

// logBlobWarn 迁移/GC 告警日志。
func (s *APIServer) logBlobWarn(ev string, fields ...zap.Field) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Warn("blob "+ev, fields...)
}

// redactBlobKey 日志脱敏（复用 util.RedactObjectKey）。
func redactBlobKey(key string) string {
	return util.RedactObjectKey(key)
}
