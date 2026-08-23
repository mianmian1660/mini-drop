// ============================================================
// server/blob_workers.go — 存储阶段二：回填 / 压缩迁移 / 延迟 GC
// ============================================================
// Release B 的三类后台 worker：
//   1. 回填（backfill）：按有效引用 distinct object_key 创建 storage_blobs 并
//      回填三张表的 blob_id。历史对象不搬迁、不重新计算内容哈希；用 MinIO
//      Stat 校准物理大小；同 key 多种大小记错误并跳过自动关联。分批幂等可恢复。
//   2. 压缩迁移（migration）：旧未压缩 kallsyms → 用户态 ELF → 历史 SVG/folded
//      ≥4KiB 文本。影子写入 CAS key（流式、不落大临时文件）→ 回读校验 →
//      事务内切换引用 → 旧 key 入 GC 队列（24h 宽限）。并发固定 1；
//      emergency/unknown 暂停。任一步失败保留旧引用，无查询空窗。
//   3. GC：storage_object_gc 到期待删对象；删除前二次确认无任何引用仍解析到
//      旧物理 key；失败沿用阶段 1 的 1m→5m→30m→2h→6h 退避。
// ============================================================

package server

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/mini-drop/apiserver/model"
)

// ------------------------------------------------------------
// Worker 启动
// ------------------------------------------------------------

// startBlobWorkers 启动阶段二后台 worker（按配置开关）。
// 回填/迁移/GC 共用同一个周期循环，按 回填 → 迁移 → GC 顺序执行，
// 迁移并发天然为 1（单 goroutine 串行处理）。
func (s *APIServer) startBlobWorkers() {
	if s == nil || s.Config == nil || s.DB == nil {
		return
	}
	if !s.Config.Blob.BackfillEnabled && !s.Config.Blob.MigrationEnabled && !s.Config.Blob.GCEnabled {
		return
	}
	interval := time.Duration(s.Config.Blob.MigrationIntervalSec) * time.Second
	if interval <= 0 {
		interval = 60 * time.Second
	}
	time.Sleep(20 * time.Second) // 等服务启动、migration 完成
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
		s.runBlobCycle(ctx)
		cancel()
	}()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
		s.runBlobCycle(ctx)
		cancel()
	}
}

// runBlobCycle 单轮：迁移（磁盘收益优先）→ 回填 → GC → 统计。
func (s *APIServer) runBlobCycle(ctx context.Context) {
	if s == nil || s.DB == nil || s.Config == nil {
		return
	}
	now := time.Now()
	s.setBlobLastRun(&now)
	if s.Config.Blob.MigrationEnabled {
		if err := s.runMigrationOnce(ctx); err != nil {
			s.setBlobError("migration: " + err.Error())
			s.logBlobWarn("migration failed", zap.Error(err))
		}
	}
	if s.Config.Blob.BackfillEnabled {
		if err := s.runBackfillOnce(ctx); err != nil {
			s.setBlobError("backfill: " + err.Error())
			s.logBlobWarn("backfill failed", zap.Error(err))
		}
	}
	if s.Config.Blob.GCEnabled {
		if err := s.runGCOnce(ctx); err != nil {
			s.setBlobError("gc: " + err.Error())
			s.logBlobWarn("gc failed", zap.Error(err))
		}
	}
	s.setBlobError("")
	s.refreshBlobMetrics(ctx)
}

// ------------------------------------------------------------
// 1) 回填
// ------------------------------------------------------------

type backfillKeyState struct {
	size     int64
	conflict bool
	statErr  string
}

// runBackfillOnce 单轮回填。分批、幂等、可中断恢复：
//   - 只处理 blob_id IS NULL 的有效引用（回填过的行不会重复处理）。
//   - 每个 distinct object_key 只 Stat 一次；Stat 失败记错误跳过。
//   - 同一 key 在本轮内出现多种 stat 大小 → conflict，跳过自动关联。
func (s *APIServer) runBackfillOnce(ctx context.Context) error {
	batch := s.Config.Blob.MigrationBatch * 4 // 回填每轮可处理更多（仍串行）
	if batch <= 0 {
		batch = 200
	}
	keyStates := map[string]*backfillKeyState{}

	// 候选来源：artifacts（有效引用）→ symbol_files → kernel_symbol_files。
	keys, err := s.backfillCandidateKeys(ctx, batch)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}
	linked := int64(0)
	created := 0
	for _, key := range keys {
		if ctx.Err() != nil {
			break
		}
		state := keyStates[key]
		if state == nil {
			state = &backfillKeyState{}
			size, statErr := s.Storage.StatObject(ctx, s.Config.Storage.Bucket, key)
			if statErr != nil {
				state.statErr = statErr.Error()
				s.logBlobWarn("backfill stat failed", zap.String("object_key", redactBlobKey(key)), zap.Error(statErr))
				incBlobBackfillStatFailures()
			} else {
				state.size = size
			}
			keyStates[key] = state
		} else if state.statErr == "" && !state.conflict && state.size != 0 {
			// 同一个 key 出现多次：再次 Stat 校准；与首次不一致视为冲突。
			size, statErr := s.Storage.StatObject(ctx, s.Config.Storage.Bucket, key)
			if statErr == nil && size != state.size {
				state.conflict = true
				s.logBlobWarn("backfill size conflict, skip auto-association",
					zap.String("object_key", redactBlobKey(key)), zap.Int64("first_size", state.size), zap.Int64("second_size", size))
				incBlobBackfillConflicts()
			}
		}
		if state.statErr != "" || state.conflict {
			continue
		}
		blobID, isNew, err := s.ensureBackfillBlob(ctx, key, state.size)
		if err != nil {
			s.logBlobWarn("backfill blob ensure failed", zap.String("object_key", redactBlobKey(key)), zap.Error(err))
			continue
		}
		if isNew {
			created++
		}
		if n := s.linkBackfillRefs(ctx, key, blobID); n > 0 {
			linked += n
		}
	}
	if linked > 0 || created > 0 {
		s.logBlobState("backfill progress",
			zap.Int("candidates", len(keys)),
			zap.Int("created", created),
			zap.Int64("linked", linked))
	}
	return nil
}

// backfillCandidateKeys 收集一轮中待回填的 distinct key（优先 artifacts）。
func (s *APIServer) backfillCandidateKeys(ctx context.Context, limit int) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(keys []string) {
		for _, k := range keys {
			k = strings.TrimSpace(k)
			if k != "" && !seen[k] {
				seen[k] = true
				out = append(out, k)
				if len(out) >= limit {
					return
				}
			}
		}
	}
	var artifactKeys []string
	if err := s.DB.WithContext(ctx).Model(&model.Artifact{}).
		Distinct("object_key").
		Where("blob_id IS NULL AND deleted_at IS NULL AND status NOT IN ?",
			[]string{model.ArtifactStatusDeleting, model.ArtifactStatusDeleted}).
		Pluck("object_key", &artifactKeys).Error; err != nil {
		return nil, err
	}
	add(artifactKeys)
	if len(out) >= limit {
		return out, nil
	}
	var symbolKeys []string
	if err := s.DB.WithContext(ctx).Model(&model.SymbolFile{}).
		Distinct("object_key").
		Where("blob_id IS NULL OR blob_id = 0").
		Pluck("object_key", &symbolKeys).Error; err != nil {
		return nil, err
	}
	add(symbolKeys)
	if len(out) >= limit {
		return out, nil
	}
	var kernelKeys []string
	if err := s.DB.WithContext(ctx).Model(&model.KernelSymbolFile{}).
		Distinct("object_key").
		Where("blob_id IS NULL OR blob_id = 0").
		Pluck("object_key", &kernelKeys).Error; err != nil {
		return nil, err
	}
	add(kernelKeys)
	return out, nil
}

// ensureBackfillBlob 为历史 key 创建/复用 Blob（object_key 唯一）。
// 历史对象不重新计算内容哈希：logical_sha256/stored_sha256 留空，
// 物理大小用 Stat 校准。
func (s *APIServer) ensureBackfillBlob(ctx context.Context, key string, size int64) (uint, bool, error) {
	var existing model.StorageBlob
	err := s.DB.WithContext(ctx).Where("object_key = ?", key).First(&existing).Error
	if err == nil {
		if existing.Status == model.BlobStatusDeleted {
			// 对象已删但 key 仍被引用（异常）：复活行，下一轮迁移会处理。
			now := time.Now()
			_ = s.DB.WithContext(ctx).Model(&model.StorageBlob{}).
				Where("id = ?", existing.ID).
				Updates(map[string]interface{}{"status": model.BlobStatusReady, "deleted_at": nil, "updated_at": now}).Error
		}
		return existing.ID, false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, false, err
	}
	format := blobFormatFromKey(key)
	compression := blobCompressionFromKey(key)
	row := model.StorageBlob{
		ObjectKey:       key,
		StoredSize:      size,
		LogicalSize:     size,
		Format:          format,
		Compression:     compression,
		ContentType:     mimeType(key),
		ContentEncoding: transparentContentEncoding(format, compression),
		// LogicalSHA256 保持 nil（NULL）：历史对象不参与内容寻址唯一索引。
		Status:    model.BlobStatusReady,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := s.DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "object_key"}},
		DoNothing: true,
	}).Create(&row).Error; err != nil {
		return 0, false, err
	}
	if row.ID == 0 {
		// 并发下被别人创建
		_ = s.DB.WithContext(ctx).Where("object_key = ?", key).First(&row).Error
	}
	incBlobBackfillBlobsCreated()
	return row.ID, true, nil
}

// linkBackfillRefs 把 key 对应的有效引用关联到 blob（幂等：只处理 blob_id IS NULL）。
func (s *APIServer) linkBackfillRefs(ctx context.Context, key string, blobID uint) int64 {
	var linked int64
	now := time.Now()
	res := s.DB.WithContext(ctx).Model(&model.Artifact{}).
		Where("object_key = ? AND blob_id IS NULL AND deleted_at IS NULL AND status NOT IN ?", key,
			[]string{model.ArtifactStatusDeleting, model.ArtifactStatusDeleted}).
		Updates(map[string]interface{}{"blob_id": blobID, "updated_at": now})
	if res.Error == nil {
		linked += res.RowsAffected
	}
	res = s.DB.WithContext(ctx).Model(&model.SymbolFile{}).
		Where("object_key = ? AND (blob_id IS NULL OR blob_id = 0)", key).
		Updates(map[string]interface{}{"blob_id": blobID})
	if res.Error == nil {
		linked += res.RowsAffected
	}
	res = s.DB.WithContext(ctx).Model(&model.KernelSymbolFile{}).
		Where("object_key = ? AND (blob_id IS NULL OR blob_id = 0)", key).
		Updates(map[string]interface{}{"blob_id": blobID})
	if res.Error == nil {
		linked += res.RowsAffected
	}
	incBlobBackfillRefsLinked(linked)
	return linked
}

// ------------------------------------------------------------
// 2) 压缩迁移
// ------------------------------------------------------------

// runMigrationOnce 单轮迁移。候选顺序：kallsyms → ELF → 历史结果。
// 并发固定 1（单 goroutine 串行），但每轮可连续处理最多 MigrationBatch 个
// 对象（在周期上下文中带 ctx 超时，不会无限阻塞）。失败保留旧引用。
func (s *APIServer) runMigrationOnce(ctx context.Context) error {
	if !s.blobMaintenanceAllowed() {
		incMaintenanceSkip("format_migration")
		s.logBlobWarn("migration paused (low disk / unknown)",
			zap.String("reason", "maintenance_allowed=false"))
		return nil
	}
	if s.Storage == nil {
		return errors.New("object storage not connected")
	}
	budget := s.Config.Blob.MigrationBatch
	if budget <= 0 {
		budget = 10
	}
	processed := 0
	for processed < budget {
		if ctx.Err() != nil {
			break
		}
		var done bool
		var err error
		// 2.1 旧未压缩 kallsyms（优先，磁盘收益最大）
		if s.Config.Blob.MigrateKallsyms {
			done, err = s.migrateNextKallsyms(ctx)
			if err != nil {
				return err
			}
			if done {
				processed++
				continue
			}
		}
		// 2.2 用户态 ELF 符号
		if s.Config.Blob.MigrateELF {
			done, err = s.migrateNextELF(ctx)
			if err != nil {
				return err
			}
			if done {
				processed++
				continue
			}
		}
		// 2.3 历史 SVG/folded/≥4KiB 文本结果
		if s.Config.Blob.MigrateResults {
			done, err = s.migrateNextResult(ctx)
			if err != nil {
				return err
			}
			if done {
				processed++
				continue
			}
		}
		break // 无候选
	}
	return nil
}

// migrationCommon 迁移公共流程：读取旧对象 → gzip 影子写入 CAS key →
// 回读校验 → 事务切换引用 → 入 GC 队列。
// 返回 (newBlobID, reclaimedBytes, error)。任何失败都不切换引用。
type migrationCommonInput struct {
	OldKey        string // 旧物理 key（逻辑 key == 旧 key，历史对象未分离）
	Format        string
	SchemaVersion string
	ContentType   string
	// SwitchKernelLedger 切换 kernel_symbol_files 引用（kallsyms 用）。
	SwitchKernelLedger bool
	// SwitchSymbolLedger 切换 symbol_files 引用（ELF 用）。
	SwitchSymbolLedger bool
	// SwitchArtifacts 切换 artifacts 引用（结果用）。
	SwitchArtifacts bool
}

func (s *APIServer) migrationCommon(ctx context.Context, in migrationCommonInput) (uint, int64, error) {
	// 1) 流式读旧对象算逻辑哈希（不落大临时文件）。
	logicalHash, logicalSize, err := s.streamHash(ctx, in.OldKey)
	if err != nil {
		return 0, 0, fmt.Errorf("hash pass: %w", err)
	}
	schema := in.SchemaVersion
	if schema == "" {
		schema = model.BlobSchemaV1
	}
	casKey := blobCASKey(logicalHash, in.Format, schema, model.CompressionGzip)

	// 2) 已有同内容 blob（并发/去重）→ 复用，直接切引用。
	var existing model.StorageBlob
	if err := s.DB.WithContext(ctx).
		Where("logical_sha256 = ? AND format = ? AND compression = ?", logicalHash, in.Format, model.CompressionGzip).
		First(&existing).Error; err == nil && existing.DeletedAt == nil {
		return existing.ID, 0, s.switchMigrationRefs(ctx, in.OldKey, existing.ID, in)
	}

	// 3) 影子上传：gzip(mtime=0) 写 CAS key，同时算存储哈希与大小。
	storedHash, storedSize, err := s.streamGzipUpload(ctx, in.OldKey, casKey, in.ContentType)
	if err != nil {
		return 0, 0, fmt.Errorf("gzip upload: %w", err)
	}

	// 4) 回读校验：解压后哈希/大小与逻辑哈希一致。
	if err := s.verifyBlob(ctx, casKey, logicalHash, logicalSize); err != nil {
		// 校验失败：尽力删除影子对象，保留旧引用。
		_ = s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, casKey)
		return 0, 0, fmt.Errorf("verify: %w", err)
	}

	// 5) 事务内创建 ready Blob + 切换引用 + 入 GC 队列。
	var blobID uint
	now := time.Now()
	err = s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row := model.StorageBlob{
			ObjectKey:       casKey,
			LogicalSHA256:   &logicalHash,
			StoredSHA256:    storedHash,
			StoredSize:      storedSize,
			LogicalSize:     logicalSize,
			Format:          in.Format,
			SchemaVersion:   schema,
			Compression:     model.CompressionGzip,
			ContentEncoding: transparentContentEncoding(in.Format, model.CompressionGzip),
			ContentType:     in.ContentType,
			Status:          model.BlobStatusReady,
			VerifiedAt:      &now,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		if err := upsertBlobByContent(tx, &row, logicalHash, in.Format); err != nil {
			return err
		}
		blobID = row.ID
		return s.switchMigrationRefsTx(tx, in.OldKey, blobID, in)
	})
	if err != nil {
		return 0, 0, err
	}
	// 6) 旧 key 入 GC 队列（24h 宽限）。
	s.enqueueGC(ctx, in.OldKey, model.GCMigrationReason)
	reclaimed := logicalSize - storedSize
	if reclaimed < 0 {
		reclaimed = 0
	}
	s.incBlobReclaimedBytes(reclaimed)
	incBlobMigrationObjects(in.Format)
	incBlobMigrationReclaimedBytes(reclaimed)
	s.logBlobState("migration done",
		zap.String("format", in.Format),
		zap.String("old_key", redactBlobKey(in.OldKey)),
		zap.String("new_key", redactBlobKey(casKey)),
		zap.Int64("logical_size", logicalSize),
		zap.Int64("stored_size", storedSize),
		zap.Int64("reclaimed", reclaimed))
	return blobID, reclaimed, nil
}

// upsertBlobByContent 按内容唯一键 (logical_sha256, format, compression) upsert。
// PostgreSQL：部分唯一索引带 WHERE 谓词，ON CONFLICT 必须显式带同样谓词
// 才能匹配索引（用 raw SQL）；SQLite（单测）：GORM 创建的是全量唯一索引，
// 用 GORM OnConflict 即可。两者都会把墓碑行复活（deleted_at=NULL, status=ready）。
func upsertBlobByContent(tx *gorm.DB, row *model.StorageBlob, logicalHash, format string) error {
	now := row.UpdatedAt
	if tx.Dialector.Name() == "postgres" {
		if err := tx.Exec(`
			INSERT INTO storage_blobs
				(object_key, logical_sha256, stored_sha256, stored_size, logical_size,
				 format, schema_version, compression, content_encoding, content_type,
				 status, delete_reason, delete_attempts, next_delete_attempt_at,
				 last_delete_error, verified_at, deleted_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (logical_sha256, format, compression)
			  WHERE logical_sha256 IS NOT NULL
			DO UPDATE SET
				object_key = EXCLUDED.object_key,
				stored_sha256 = EXCLUDED.stored_sha256,
				stored_size = EXCLUDED.stored_size,
				logical_size = EXCLUDED.logical_size,
				content_encoding = EXCLUDED.content_encoding,
				content_type = EXCLUDED.content_type,
				status = 'ready',
				delete_reason = NULL,
				delete_attempts = 0,
				verified_at = EXCLUDED.verified_at,
				deleted_at = NULL,
				updated_at = NOW()`,
			row.ObjectKey, row.LogicalSHA256, row.StoredSHA256, row.StoredSize, row.LogicalSize,
			row.Format, row.SchemaVersion, row.Compression, row.ContentEncoding, row.ContentType,
			row.Status, row.DeleteReason, row.DeleteAttempts, row.NextDeleteAttemptAt, row.LastDeleteError,
			row.VerifiedAt, row.DeletedAt, row.CreatedAt, row.UpdatedAt).Error; err != nil {
			return err
		}
		var id uint
		if err := tx.Raw(`
			SELECT id FROM storage_blobs
			WHERE logical_sha256 = ? AND format = ? AND compression = ?
			LIMIT 1`, logicalHash, format, model.CompressionGzip).Scan(&id).Error; err != nil {
			return err
		}
		row.ID = id
		return nil
	}
	if err := tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "logical_sha256"}, {Name: "format"}, {Name: "compression"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"object_key":       row.ObjectKey,
			"stored_sha256":    row.StoredSHA256,
			"stored_size":      row.StoredSize,
			"logical_size":     row.LogicalSize,
			"content_encoding": row.ContentEncoding,
			"content_type":     row.ContentType,
			"status":           model.BlobStatusReady,
			"verified_at":      row.VerifiedAt,
			"deleted_at":       nil,
			"updated_at":       now,
		}),
	}).Create(row).Error; err != nil {
		return err
	}
	if row.ID == 0 {
		if err := tx.Where("logical_sha256 = ? AND format = ? AND compression = ?", logicalHash, format, model.CompressionGzip).First(row).Error; err != nil {
			return err
		}
	}
	return nil
}

// switchMigrationRefs 事务外切引用（复用已有 blob 的并发场景）。
func (s *APIServer) switchMigrationRefs(ctx context.Context, oldKey string, blobID uint, in migrationCommonInput) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return s.switchMigrationRefsTx(tx, oldKey, blobID, in)
	})
}

func (s *APIServer) switchMigrationRefsTx(tx *gorm.DB, oldKey string, blobID uint, in migrationCommonInput) error {
	now := time.Now()
	if in.SwitchArtifacts {
		if err := tx.Model(&model.Artifact{}).
			Where("object_key = ? AND deleted_at IS NULL AND status NOT IN ?", oldKey,
				[]string{model.ArtifactStatusDeleting, model.ArtifactStatusDeleted}).
			Updates(map[string]interface{}{"blob_id": blobID, "updated_at": now}).Error; err != nil {
			return err
		}
	}
	if in.SwitchKernelLedger {
		if err := tx.Model(&model.KernelSymbolFile{}).
			Where("object_key = ?", oldKey).
			Updates(map[string]interface{}{"blob_id": blobID}).Error; err != nil {
			return err
		}
	}
	if in.SwitchSymbolLedger {
		if err := tx.Model(&model.SymbolFile{}).
			Where("object_key = ?", oldKey).
			Updates(map[string]interface{}{"blob_id": blobID}).Error; err != nil {
			return err
		}
	}
	return nil
}

// blobMigrateReady 判断 blob 引用是否指向"旧物理 key"（非 CAS key），
// 或尚未回填（blob_id 为空）——两种情况都构成迁移候选。
func blobMigrateReady(blob *model.StorageBlob) bool {
	if blob == nil {
		return true
	}
	return !strings.HasPrefix(blob.ObjectKey, "blobs/sha256/")
}

// migrateNextKallsyms 迁移一个旧未压缩 kallsyms（"kernel-symbols/<sha>/kallsyms"，
// 不带 .gz 后缀）。已压缩的 .gz 对象由回填覆盖，不重复压缩。
func (s *APIServer) migrateNextKallsyms(ctx context.Context) (bool, error) {
	var row model.KernelSymbolFile
	err := s.DB.WithContext(ctx).
		Where("object_key LIKE 'kernel-symbols/%/kallsyms' AND object_key NOT LIKE '%.gz'").
		Order("created_at ASC").Limit(1).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// 已回填且 blob 已是 CAS key → 跳过。
	if row.BlobID != nil && *row.BlobID > 0 {
		var blob model.StorageBlob
		if err := s.DB.WithContext(ctx).Where("id = ?", *row.BlobID).First(&blob).Error; err == nil && !blobMigrateReady(&blob) {
			return false, nil
		}
	}
	_, _, err = s.migrationCommon(ctx, migrationCommonInput{
		OldKey:             row.ObjectKey,
		Format:             model.BlobFormatKallsyms,
		ContentType:        "application/octet-stream",
		SwitchKernelLedger: true,
		SwitchArtifacts:    true, // kallsyms 的 artifacts 引用也指向旧 key
	})
	if err != nil {
		s.incBlobFailedObjects(1)
		incBlobMigrationFailures()
		s.logBlobWarn("kallsyms migration failed",
			zap.String("object_key", redactBlobKey(row.ObjectKey)), zap.Error(err))
		return true, err
	}
	return true, nil
}

// migrateNextELF 迁移一个用户态 ELF 符号（"symbols/<build_id>"）。
func (s *APIServer) migrateNextELF(ctx context.Context) (bool, error) {
	var row model.SymbolFile
	err := s.DB.WithContext(ctx).
		Where("object_key LIKE 'symbols/%'").
		Order("created_at ASC").Limit(1).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if row.BlobID != nil && *row.BlobID > 0 {
		var blob model.StorageBlob
		if err := s.DB.WithContext(ctx).Where("id = ?", *row.BlobID).First(&blob).Error; err == nil && !blobMigrateReady(&blob) {
			return false, nil
		}
	}
	_, _, err = s.migrationCommon(ctx, migrationCommonInput{
		OldKey:            row.ObjectKey,
		Format:            model.BlobFormatELF,
		ContentType:       "application/octet-stream",
		SwitchSymbolLedger: true,
	})
	if err != nil {
		s.incBlobFailedObjects(1)
		incBlobMigrationFailures()
		s.logBlobWarn("elf migration failed",
			zap.String("object_key", redactBlobKey(row.ObjectKey)), zap.Error(err))
		return true, err
	}
	return true, nil
}

// migrateNextResult 迁移一个历史文本结果（SVG/folded/JSON/Markdown ≥4KiB）。
func (s *APIServer) migrateNextResult(ctx context.Context) (bool, error) {
	var a model.Artifact
	err := s.DB.WithContext(ctx).
		Where("artifacts.kind IN ? AND artifacts.size >= ? AND artifacts.deleted_at IS NULL AND artifacts.status NOT IN ?",
			[]string{model.ArtifactKindResult, model.ArtifactKindIntermediate},
			s.Config.Blob.MinCompressBytes,
			[]string{model.ArtifactStatusDeleting, model.ArtifactStatusDeleted}).
		Order("artifacts.id ASC").Limit(1).First(&a).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if a.BlobID != nil && *a.BlobID > 0 {
		var blob model.StorageBlob
		if err := s.DB.WithContext(ctx).Where("id = ?", *a.BlobID).First(&blob).Error; err == nil && !blobMigrateReady(&blob) {
			return false, nil
		}
	}
	// 只迁移文本类结果（svg/json/md/txt/collapsed）；二进制/perf.data 跳过。
	format := blobFormatFromKey(a.ObjectKey)
	switch format {
	case model.BlobFormatSVG, model.BlobFormatFolded, model.BlobFormatJSON, model.BlobFormatMarkdown:
	default:
		// 不可迁移格式：标记 blob（占位）避免反复扫描。
		now := time.Now()
		_ = s.DB.WithContext(ctx).Model(&model.Artifact{}).
			Where("id = ?", a.ID).
			Updates(map[string]interface{}{"format": format, "updated_at": now}).Error
		return false, nil
	}
	contentType := mimeType(a.ObjectKey)
	_, _, err = s.migrationCommon(ctx, migrationCommonInput{
		OldKey:          a.ObjectKey,
		Format:          format,
		ContentType:     contentType,
		SwitchArtifacts: true,
	})
	if err != nil {
		s.incBlobFailedObjects(1)
		incBlobMigrationFailures()
		s.logBlobWarn("result migration failed",
			zap.String("object_key", redactBlobKey(a.ObjectKey)), zap.Error(err))
		return true, err
	}
	return true, nil
}

// streamHash 流式读取对象并计算逻辑内容哈希与大小（不落临时文件）。
func (s *APIServer) streamHash(ctx context.Context, key string) (string, int64, error) {
	rc, err := s.Storage.GetObject(ctx, s.Config.Storage.Bucket, key)
	if err != nil {
		return "", 0, err
	}
	defer rc.Close()
	hasher := sha256.New()
	n, err := io.Copy(hasher, rc)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), n, nil
}

// streamGzipUpload 流式 gzip（mtime=0）上传到 CAS key，同时计算存储哈希与大小。
// 压缩与哈希在 goroutine 内完成，避免上传主线程与 hasher 的数据竞争；
// 上传完成后用 Stat 校准存储大小。
func (s *APIServer) streamGzipUpload(ctx context.Context, srcKey, dstKey, contentType string) (string, int64, error) {
	rc, err := s.Storage.GetObject(ctx, s.Config.Storage.Bucket, srcKey)
	if err != nil {
		return "", 0, err
	}
	defer rc.Close()
	pr, pw := io.Pipe()
	hashCh := make(chan string, 1)
	doneCh := make(chan error, 1)
	go func() {
		hasher := sha256.New()
		zw, gzErr := gzip.NewWriterLevel(pw, gzip.BestCompression)
		if gzErr != nil {
			pw.CloseWithError(gzErr)
			doneCh <- gzErr
			return
		}
		zw.Header.ModTime = time.Unix(0, 0) // mtime=0，确定性输出
		zw.Header.OS = 0
		mw := io.MultiWriter(zw, hasher)
		_, copyErr := io.Copy(mw, rc)
		closeErr := zw.Close()
		hashCh <- hex.EncodeToString(hasher.Sum(nil))
		if copyErr != nil {
			pw.CloseWithError(copyErr)
			doneCh <- copyErr
			return
		}
		if closeErr != nil {
			pw.CloseWithError(closeErr)
			doneCh <- closeErr
			return
		}
		pw.Close()
		doneCh <- nil
	}()
	putErr := s.Storage.PutObject(ctx, s.Config.Storage.Bucket, dstKey, pr, -1, contentType)
	if putErr != nil {
		// 关闭写端，让压缩 goroutine 立即退出，避免管道阻塞挂死 worker。
		_ = pw.CloseWithError(putErr)
	}
	storedHash := <-hashCh
	copyErr := <-doneCh
	if putErr != nil {
		return "", 0, putErr
	}
	if copyErr != nil {
		return "", 0, copyErr
	}
	size, err := s.Storage.StatObject(ctx, s.Config.Storage.Bucket, dstKey)
	if err != nil {
		return "", 0, err
	}
	return storedHash, size, nil
}

// verifyBlob 回读 CAS 对象，解压后校验逻辑哈希与大小。
func (s *APIServer) verifyBlob(ctx context.Context, key, wantHash string, wantSize int64) error {
	rc, err := s.Storage.GetObject(ctx, s.Config.Storage.Bucket, key)
	if err != nil {
		return err
	}
	defer rc.Close()
	zr, err := gzip.NewReader(rc)
	if err != nil {
		return fmt.Errorf("gzip header: %w", err)
	}
	defer zr.Close()
	hasher := sha256.New()
	n, err := io.Copy(hasher, zr)
	if err != nil {
		return err
	}
	if n != wantSize {
		return fmt.Errorf("size mismatch: got %d want %d", n, wantSize)
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != wantHash {
		return fmt.Errorf("hash mismatch: got %s want %s", got, wantHash)
	}
	return nil
}

// ------------------------------------------------------------
// 3) GC
// ------------------------------------------------------------

// enqueueGC 把旧物理 key 写入 GC 队列（幂等：同 key 已存在则不重复入队）。
// 入队后 24h 宽限期（not_before）满才允许删除。
func (s *APIServer) enqueueGC(ctx context.Context, key, reason string) {
	if s == nil || s.DB == nil || key == "" {
		return
	}
	now := time.Now()
	nb := now.Add(time.Duration(s.Config.Blob.GCSafeGraceHours) * time.Hour)
	row := model.StorageObjectGC{
		ObjectKey: key,
		Reason:    reason,
		NotBefore: &nb,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "object_key"}},
		DoNothing: true,
	}).Create(&row).Error; err != nil {
		s.logBlobWarn("gc enqueue failed", zap.String("object_key", redactBlobKey(key)), zap.Error(err))
	}
}

// runGCOnce 处理到期的 GC 队列条目（删除前二次确认无引用解析到旧 key）。
func (s *APIServer) runGCOnce(ctx context.Context) error {
	if s == nil || s.DB == nil || s.Storage == nil {
		return errors.New("database or object storage not connected")
	}
	now := time.Now()
	var rows []model.StorageObjectGC
	if err := s.DB.WithContext(ctx).
		Where("deleted_at IS NULL AND not_before IS NOT NULL AND not_before <= ?", now).
		Where("(next_delete_attempt_at IS NULL OR next_delete_attempt_at <= ?)", now).
		Order("not_before ASC, id ASC").
		Limit(50).Find(&rows).Error; err != nil {
		return err
	}
	for i := range rows {
		if ctx.Err() != nil {
			break
		}
		g := &rows[i]
		// 二次确认：没有任何引用仍解析到旧物理 key。
		refs, err := s.legacyKeyRefs(ctx, g.ObjectKey)
		if err != nil {
			s.logBlobWarn("gc ref check failed", zap.String("object_key", redactBlobKey(g.ObjectKey)), zap.Error(err))
			continue
		}
		if refs > 0 {
			// 仍有引用（可能是回滚后旧版本写入）：跳过，等引用消失。
			s.logBlobWarn("gc deferred, legacy refs remain",
				zap.String("object_key", redactBlobKey(g.ObjectKey)), zap.Int64("refs", refs))
			continue
		}
		// 删除前 Stat 一次，用于回收字节统计。
		var deletedBytes int64
		if size, statErr := s.Storage.StatObject(ctx, s.Config.Storage.Bucket, g.ObjectKey); statErr == nil {
			deletedBytes = size
		}
		if err := s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, g.ObjectKey); err != nil {
			s.failGCDeletion(ctx, g, err)
			continue
		}
		now2 := time.Now()
		if err := s.DB.WithContext(ctx).Model(&model.StorageObjectGC{}).
			Where("id = ? AND deleted_at IS NULL", g.ID).
			Updates(map[string]interface{}{"deleted_at": &now2, "updated_at": now2}).Error; err != nil {
			s.logBlobWarn("gc tombstone failed", zap.Uint("id", g.ID), zap.Error(err))
			continue
		}
		incBlobGCDeleted()
		incBlobGCDeletedBytes(deletedBytes)
		s.incBlobReclaimedBytes(deletedBytes)
		s.logBlobState("gc deleted",
			zap.String("object_key", redactBlobKey(g.ObjectKey)),
			zap.String("reason", g.Reason),
			zap.Int64("bytes", deletedBytes))
	}
	return nil
}

// legacyKeyRefs 统计仍有引用解析到旧物理 key 的行数：
// blob_id IS NULL 的 artifacts/symbol_files/kernel_symbol_files + 未删除的 blob 行。
func (s *APIServer) legacyKeyRefs(ctx context.Context, key string) (int64, error) {
	var total int64
	for _, m := range []interface{}{
		&model.Artifact{}, &model.SymbolFile{}, &model.KernelSymbolFile{},
	} {
		q := s.DB.WithContext(ctx).Model(m).Where("object_key = ?", key)
		switch m.(type) {
		case *model.Artifact:
			q = q.Where("deleted_at IS NULL AND status NOT IN ?", []string{model.ArtifactStatusDeleting, model.ArtifactStatusDeleted})
		}
		var n int64
		if err := q.Count(&n).Error; err != nil {
			return 0, err
		}
		total += n
	}
	var blobRefs int64
	_ = s.DB.WithContext(ctx).Model(&model.StorageBlob{}).
		Where("object_key = ? AND deleted_at IS NULL", key).
		Count(&blobRefs).Error
	return total + blobRefs, nil
}

// failGCDeletion 删除失败退避（1m→5m→30m→2h→6h）。
func (s *APIServer) failGCDeletion(ctx context.Context, g *model.StorageObjectGC, delErr error) {
	now := time.Now()
	attempts := g.DeleteAttempts + 1
	next := now.Add(deleteBackoff(attempts))
	if err := s.DB.WithContext(ctx).Model(&model.StorageObjectGC{}).
		Where("id = ? AND deleted_at IS NULL", g.ID).
		Updates(map[string]interface{}{
			"delete_attempts":        attempts,
			"last_delete_error":      truncateString(delErr.Error(), 1024),
			"next_delete_attempt_at": &next,
			"updated_at":             now,
		}).Error; err != nil {
		s.logBlobWarn("gc fail update failed", zap.Uint("id", g.ID), zap.Error(err))
		return
	}
	incBlobGCFailures()
	s.setBlobError(delErr.Error())
	s.logBlobWarn("gc delete failed, will retry",
		zap.String("object_key", redactBlobKey(g.ObjectKey)),
		zap.Int("attempt", attempts),
		zap.String("next_attempt", next.Format(time.RFC3339)),
		zap.Error(delErr))
}
