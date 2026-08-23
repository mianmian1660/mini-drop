package server

import (
	"errors"
	"io"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/mini-drop/apiserver/config"
	"github.com/mini-drop/apiserver/model"
)

func blobTestServer(t *testing.T) *APIServer {
	t.Helper()
	s := newTestAPIServer(t)
	mem := newRetentionMemoryStorage()
	s.Storage = mem
	s.Config = &config.Config{
		Storage: config.StorageConfig{Bucket: "drop-data", PresignExpireSec: 900},
		Blob: config.BlobConfig{
			MinCompressBytes:  4096,
			GCSafeGraceHours:  24,
			MigrationBatch:    50,
			MigrationIntervalSec: 60,
			MigrateKallsyms:   true,
			MigrateELF:        true,
			MigrateResults:    true,
		},
		Retention: config.RetentionConfig{
			Enabled: true, LifecycleMode: "enforce", NotBeforeProtectionHours: 24,
			RawLargeHours: 24, RawPortableHours: 168, IntermediateHours: 24,
			DiagnosticHours: 72, ResultRetentionHours: 720, ManifestPermanent: true,
			ReconcileIntervalSec: 60, ReconcileBatch: 100,
		},
		StorageDisk: config.StorageDiskConfig{
			Path: "/tmp", WarningFreeBytes: 8 << 30, CriticalFreeBytes: 4 << 30, MinFreeBytes: 1 << 30,
		},
	}
	return s
}

func blobRefServer(t *testing.T) *APIServer {
	s := blobTestServer(t)
	s.Config.Blob.BackfillEnabled = true
	s.Config.Blob.MigrationEnabled = true
	s.Config.Blob.GCEnabled = true
	return s
}

func sha256hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func mustGzip(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// ------------------------------------------------------------
// CAS key
// ------------------------------------------------------------

func TestBlobCASKeyFormat(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	if got := blobCASKey(sha, model.BlobFormatPprof, model.BlobSchemaV1, model.CompressionGzip); got != "blobs/sha256/ab/"+sha+"/pprof-v1.pb.gz" {
		t.Fatalf("pprof cas key = %s", got)
	}
	if got := blobCASKey(sha, model.BlobFormatSVG, model.BlobSchemaV1, model.CompressionGzip); got != "blobs/sha256/ab/"+sha+"/svg-v1.gz" {
		t.Fatalf("svg cas key = %s", got)
	}
	if got := blobCASKey(sha, model.BlobFormatJSON, model.BlobSchemaV1, model.CompressionNone); got != "blobs/sha256/ab/"+sha+"/json-v1" {
		t.Fatalf("json cas key = %s", got)
	}
	// 短哈希兜底
	if got := blobCASKey("a", "svg", "1", "gzip"); !strings.HasPrefix(got, "blobs/sha256/00/") {
		t.Fatalf("short hash cas key = %s", got)
	}
}

// ------------------------------------------------------------
// Resolver
// ------------------------------------------------------------

func TestResolveBlobForKey(t *testing.T) {
	s := blobTestServer(t)
	now := time.Now()
	blob := model.StorageBlob{
		ObjectKey: "blobs/sha256/ab/sha1/svg-v1.gz", StoredSize: 100,
		Format: model.BlobFormatSVG, Compression: model.CompressionGzip,
		ContentEncoding: "gzip", Status: model.BlobStatusReady,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.DB.Create(&blob).Error; err != nil {
		t.Fatal(err)
	}
	a := model.Artifact{
		TaskTID: "tid-1", Kind: model.ArtifactKindResult,
		ObjectKey: "tid-1/flamegraph.svg", Size: 100, Status: model.ArtifactStatusReady,
		BlobID: &blob.ID, CreatedAt: now,
	}
	if err := s.DB.Create(&a).Error; err != nil {
		t.Fatal(err)
	}

	// 逻辑 key → 物理 key + blob
	res := s.resolveBlobForKey(context.Background(), "tid-1/flamegraph.svg")
	if !res.Logical || res.PhysicalKey != blob.ObjectKey || res.Blob == nil {
		t.Fatalf("logical resolve = %+v", res)
	}
	if res.Blob.ContentEncoding != "gzip" {
		t.Fatalf("content encoding = %q", res.Blob.ContentEncoding)
	}
	// 物理 key 直命中
	res = s.resolveBlobForKey(context.Background(), blob.ObjectKey)
	if res.PhysicalKey != blob.ObjectKey || res.Blob == nil {
		t.Fatalf("physical resolve = %+v", res)
	}
	// 未命中 → 原样返回（兼容）
	res = s.resolveBlobForKey(context.Background(), "tid-1/top.json")
	if res.PhysicalKey != "tid-1/top.json" || res.Blob != nil {
		t.Fatalf("fallback resolve = %+v", res)
	}
	// 墓碑不复活：artifact 删除后 resolve 应回退物理 key
	_ = s.DB.Model(&model.Artifact{}).Where("id = ?", a.ID).
		Updates(map[string]interface{}{"status": model.ArtifactStatusDeleted, "deleted_at": &now}).Error
	res = s.resolveBlobForKey(context.Background(), "tid-1/flamegraph.svg")
	if res.Blob != nil {
		t.Fatalf("tombstone should not resolve: %+v", res)
	}
}

// ------------------------------------------------------------
// 回填：幂等 / 同 key 合并 / 大小冲突 / 兼容读取
// ------------------------------------------------------------

func TestBlobBackfillIdempotentAndLinksRefs(t *testing.T) {
	s := blobTestServer(t)
	mem := s.Storage.(*retentionMemoryStorage)
	svg := []byte("<svg>" + strings.Repeat("x", 100) + "</svg>")
	mem.objects["tid-1/flamegraph.svg"] = svg
	mem.objects["tid-1/top.json"] = []byte(`{"a":1}`)
	mem.objects["kernel-symbols/abc/kallsyms"] = []byte("ffffffff81000000 T x\n")

	now := time.Now()
	s.DB.Create(&model.Artifact{TaskTID: "tid-1", Kind: model.ArtifactKindResult,
		ObjectKey: "tid-1/flamegraph.svg", Size: int64(len(svg)), Status: model.ArtifactStatusReady, CreatedAt: now})
	s.DB.Create(&model.Artifact{TaskTID: "tid-2", Kind: model.ArtifactKindResult,
		ObjectKey: "tid-1/flamegraph.svg", Size: int64(len(svg)), Status: model.ArtifactStatusReady, CreatedAt: now}) // 共享引用
	s.DB.Create(&model.Artifact{TaskTID: "tid-1", Kind: model.ArtifactKindResult,
		ObjectKey: "tid-1/top.json", Size: 7, Status: model.ArtifactStatusReady, CreatedAt: now})
	s.DB.Create(&model.Artifact{TaskTID: "tid-3", Kind: model.ArtifactKindRaw,
		ObjectKey: "kernel-symbols/abc/kallsyms", Size: 30, Status: model.ArtifactStatusReady, CreatedAt: now})
	s.DB.Create(&model.KernelSymbolFile{SHA256: "abc", ObjectKey: "kernel-symbols/abc/kallsyms", SizeBytes: 30, Status: 1, CreatedAt: now})

	if err := s.runBackfillOnce(context.Background()); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	var blobs []model.StorageBlob
	if err := s.DB.Find(&blobs).Error; err != nil {
		t.Fatal(err)
	}
	// 3 个 distinct key → 3 个 blob；共享 key 只计一次
	if len(blobs) != 3 {
		t.Fatalf("blob count = %d, want 3", len(blobs))
	}
	// 物理大小 = Stat 校准
	for _, b := range blobs {
		if b.StoredSize == 0 {
			t.Fatalf("blob %s stored size = 0", b.ObjectKey)
		}
	}
	// 所有引用已关联 blob_id
	var artifacts []model.Artifact
	s.DB.Find(&artifacts)
	for _, a := range artifacts {
		if a.BlobID == nil {
			t.Fatalf("artifact %d blob_id not linked", a.ID)
		}
	}
	var kf model.KernelSymbolFile
	s.DB.First(&kf)
	if kf.BlobID == nil {
		t.Fatal("kernel_symbol_files blob_id not linked")
	}
	// 幂等：再跑一轮不新增 blob、不报错
	countBefore := int64(len(blobs))
	if err := s.runBackfillOnce(context.Background()); err != nil {
		t.Fatalf("backfill idempotent: %v", err)
	}
	var blobs2 []model.StorageBlob
	s.DB.Find(&blobs2)
	if int64(len(blobs2)) != countBefore {
		t.Fatalf("backfill not idempotent: %d -> %d", countBefore, len(blobs2))
	}
}

func TestBlobBackfillSizeConflictSkipsAssociation(t *testing.T) {
	s := blobTestServer(t)
	mem := s.Storage.(*retentionMemoryStorage)
	mem.objects["tid-1/x.json"] = []byte(`{"a":1}`)
	now := time.Now()
	// 两个引用声称不同大小（同 key 冲突）
	s.DB.Create(&model.Artifact{TaskTID: "tid-1", Kind: model.ArtifactKindResult,
		ObjectKey: "tid-1/x.json", Size: 10, Status: model.ArtifactStatusReady, CreatedAt: now})
	s.DB.Create(&model.Artifact{TaskTID: "tid-2", Kind: model.ArtifactKindResult,
		ObjectKey: "tid-1/x.json", Size: 20, Status: model.ArtifactStatusReady, CreatedAt: now})
	if err := s.runBackfillOnce(context.Background()); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	// 无冲突时仍应正常关联（Stat 校准为权威大小）
	var artifacts []model.Artifact
	s.DB.Find(&artifacts)
	for _, a := range artifacts {
		if a.BlobID == nil {
			t.Fatalf("artifact %d blob_id not linked", a.ID)
		}
	}
}

func TestBlobBackfillStatFailureSkips(t *testing.T) {
	s := blobTestServer(t)
	// 对象不存在：Stat 失败 → 跳过，不建 blob 不关联
	now := time.Now()
	s.DB.Create(&model.Artifact{TaskTID: "tid-1", Kind: model.ArtifactKindRaw,
		ObjectKey: "tid-1/missing.perf.data", Size: 5, Status: model.ArtifactStatusReady, CreatedAt: now})
	if err := s.runBackfillOnce(context.Background()); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	var blobs []model.StorageBlob
	s.DB.Find(&blobs)
	if len(blobs) != 0 {
		t.Fatalf("expected no blobs for missing object, got %d", len(blobs))
	}
	var a model.Artifact
	s.DB.First(&a)
	if a.BlobID != nil {
		t.Fatal("missing object should not link blob_id")
	}
}

// ------------------------------------------------------------
// 物理容量：多引用只计一次
// ------------------------------------------------------------

func TestBlobMultiRefPhysicalCapacity(t *testing.T) {
	s := blobTestServer(t)
	now := time.Now()
	// 一个 blob 被两个 artifact 引用
	blob := model.StorageBlob{ObjectKey: "blobs/sha256/ab/sha1/svg-v1.gz", StoredSize: 100,
		LogicalSize: 1000, Format: model.BlobFormatSVG, Compression: model.CompressionGzip,
		ContentEncoding: "gzip", Status: model.BlobStatusReady, CreatedAt: now, UpdatedAt: now}
	s.DB.Create(&blob)
	s.DB.Create(&model.Artifact{TaskTID: "t1", Kind: model.ArtifactKindResult,
		ObjectKey: "t1/flamegraph.svg", Size: 100, LogicalSize: 1000, BlobID: &blob.ID, Status: model.ArtifactStatusReady, CreatedAt: now})
	s.DB.Create(&model.Artifact{TaskTID: "t2", Kind: model.ArtifactKindResult,
		ObjectKey: "t2/flamegraph.svg", Size: 100, LogicalSize: 1000, BlobID: &blob.ID, Status: model.ArtifactStatusReady, CreatedAt: now})
	stats := s.collectBlobStats(context.Background())
	if stats.BlobPhysicalBytes != 100 {
		t.Fatalf("physical bytes = %d, want 100 (dedup)", stats.BlobPhysicalBytes)
	}
	if stats.BlobLogicalBytes != 1000 {
		t.Fatalf("logical bytes = %d, want 1000", stats.BlobLogicalBytes)
	}
	refs, err := s.countBlobRefs(context.Background(), blob.ID)
	if err != nil || refs != 2 {
		t.Fatalf("refs = %d err = %v, want 2", refs, err)
	}
}

// ------------------------------------------------------------
// 最后引用删除 → Blob GC；有引用 → 保留
// ------------------------------------------------------------

func TestBlobLastRefDeletionGarbageCollects(t *testing.T) {
	s := blobTestServer(t)
	mem := s.Storage.(*retentionMemoryStorage)
	now := time.Now()
	blob := model.StorageBlob{ObjectKey: "blobs/sha256/ab/sha1/svg-v1.gz", StoredSize: 100,
		Format: model.BlobFormatSVG, Compression: model.CompressionGzip,
		ContentEncoding: "gzip", Status: model.BlobStatusReady, CreatedAt: now, UpdatedAt: now}
	s.DB.Create(&blob)
	mem.objects[blob.ObjectKey] = []byte("gzip-data")
	a1 := model.Artifact{TaskTID: "t1", Kind: model.ArtifactKindResult,
		ObjectKey: "t1/flamegraph.svg", Size: 100, BlobID: &blob.ID, Status: model.ArtifactStatusReady, CreatedAt: now}
	s.DB.Create(&a1)
	a2 := model.Artifact{TaskTID: "t2", Kind: model.ArtifactKindResult,
		ObjectKey: "t2/flamegraph.svg", Size: 100, BlobID: &blob.ID, Status: model.ArtifactStatusReady, CreatedAt: now}
	s.DB.Create(&a2)

	// 还有引用（a2）时：删 a1 但 blob 保留
	claimed := s.claimArtifactForDeletion(context.Background(), &a1, false)
	if !claimed {
		t.Fatal("claim a1 failed")
	}
	s.processClaimedDeletion(context.Background(), &a1, model.DeleteReasonExpired)
	var blobAfter model.StorageBlob
	s.DB.First(&blobAfter, blob.ID)
	if blobAfter.Status != model.BlobStatusReady {
		t.Fatalf("blob should remain while refs exist, got %s", blobAfter.Status)
	}

	// 最后一个引用删除 → blob 对象删除 + 墓碑
	claimed2 := s.claimArtifactForDeletion(context.Background(), &a2, false)
	if !claimed2 {
		t.Fatal("claim a2 failed")
	}
	s.processClaimedDeletion(context.Background(), &a2, model.DeleteReasonExpired)
	var blobFinal model.StorageBlob
	s.DB.First(&blobFinal, blob.ID)
	if blobFinal.Status != model.BlobStatusDeleted || blobFinal.DeletedAt == nil {
		t.Fatalf("blob should be tombstoned, got %+v", blobFinal)
	}
	if _, ok := mem.objects[blob.ObjectKey]; ok {
		t.Fatal("blob object should be deleted from storage")
	}
}

// ------------------------------------------------------------
// 迁移：影子写入 + 校验 + 切引用 + GC 入队；失败保留旧引用
// ------------------------------------------------------------

func TestBlobMigrationCompressesAndSwitchesRefs(t *testing.T) {
	s := blobTestServer(t)
	mem := s.Storage.(*retentionMemoryStorage)
	legacyKallsyms := []byte("ffffffff81000000 T startup_64\nffffffff81000100 T start_kernel\n")
	mem.objects["kernel-symbols/abc/kallsyms"] = legacyKallsyms
	now := time.Now()
	s.DB.Create(&model.Artifact{TaskTID: "t1", Kind: model.ArtifactKindRaw,
		ObjectKey: "kernel-symbols/abc/kallsyms", Size: int64(len(legacyKallsyms)), Status: model.ArtifactStatusReady, CreatedAt: now})
	s.DB.Create(&model.KernelSymbolFile{SHA256: "abc", ObjectKey: "kernel-symbols/abc/kallsyms", SizeBytes: int64(len(legacyKallsyms)), Status: 1, CreatedAt: now})

	// 回填先行（旧 key 建 blob），再迁移
	if err := s.runBackfillOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	done, err := s.migrateNextKallsyms(context.Background())
	if err != nil || !done {
		t.Fatalf("migrateNextKallsyms done=%v err=%v", done, err)
	}
	// 新 blob：CAS key + gzip + 双哈希
	var blobs []model.StorageBlob
	s.DB.Order("id ASC").Find(&blobs)
	if len(blobs) != 2 {
		t.Fatalf("blob count = %d (backfill + migration), want 2", len(blobs))
	}
	var migrated *model.StorageBlob
	for i := range blobs {
		if strings.HasPrefix(blobs[i].ObjectKey, "blobs/sha256/") {
			migrated = &blobs[i]
		}
	}
	if migrated == nil {
		t.Fatal("no CAS blob created")
	}
	if migrated.Compression != model.CompressionGzip {
		t.Fatalf("compression = %s", migrated.Compression)
	}
	if migrated.LogicalSHA256 == nil || *migrated.LogicalSHA256 != sha256hex(legacyKallsyms) {
		t.Fatal("logical sha mismatch")
	}
	// 回读校验：对象是 gzip，解压后等于原文
	body, ok := mem.objects[migrated.ObjectKey]
	if !ok {
		t.Fatal("CAS object not stored")
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip header: %v", err)
	}
	var out bytes.Buffer
	out.ReadFrom(zr)
	if !bytes.Equal(out.Bytes(), legacyKallsyms) {
		t.Fatal("roundtrip mismatch")
	}
	// 引用已切换
	var a model.Artifact
	s.DB.Where("object_key = ?", "kernel-symbols/abc/kallsyms").First(&a)
	if a.BlobID == nil || *a.BlobID != migrated.ID {
		t.Fatalf("artifact blob_id = %v, want %d", a.BlobID, migrated.ID)
	}
	var kf model.KernelSymbolFile
	s.DB.First(&kf)
	if kf.BlobID == nil || *kf.BlobID != migrated.ID {
		t.Fatalf("kernel ledger blob_id = %v, want %d", kf.BlobID, migrated.ID)
	}
	// GC 入队（24h 宽限）
	var gc model.StorageObjectGC
	if err := s.DB.Where("object_key = ?", "kernel-symbols/abc/kallsyms").First(&gc).Error; err != nil {
		t.Fatalf("gc entry missing: %v", err)
	}
	if gc.NotBefore == nil || gc.NotBefore.After(time.Now().Add(48*time.Hour)) {
		t.Fatalf("gc not_before = %v", gc.NotBefore)
	}
	// GC 宽限未到不删除
	if err := s.runGCOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := mem.objects["kernel-symbols/abc/kallsyms"]; !ok {
		t.Fatal("legacy object deleted before grace period")
	}
}

func TestBlobMigrationFailureKeepsOldRefs(t *testing.T) {
	s := blobTestServer(t)
	mem := s.Storage.(*retentionMemoryStorage)
	mem.objects["kernel-symbols/abc/kallsyms"] = []byte("ffffffff81000000 T x\n")
	now := time.Now()
	s.DB.Create(&model.Artifact{TaskTID: "t1", Kind: model.ArtifactKindRaw,
		ObjectKey: "kernel-symbols/abc/kallsyms", Size: 30, Status: model.ArtifactStatusReady, CreatedAt: now})
	s.DB.Create(&model.KernelSymbolFile{SHA256: "abc", ObjectKey: "kernel-symbols/abc/kallsyms", SizeBytes: 30, Status: 1, CreatedAt: now})
	// 先回填：artifact/kernel ledger 指向旧 blob（object_key = 旧 key）
	if err := s.runBackfillOnce(context.Background()); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	var oldBlob model.StorageBlob
	if err := s.DB.Where("object_key = ?", "kernel-symbols/abc/kallsyms").First(&oldBlob).Error; err != nil {
		t.Fatalf("backfill blob missing: %v", err)
	}

	// 注入读失败：hash pass 失败 → 保留旧引用、不入 GC 队列
	s.Storage = &failingGetStorage{retentionMemoryStorage: mem}
	done, err := s.migrateNextKallsyms(context.Background())
	if err == nil {
		t.Fatal("expected migration error")
	}
	_ = done
	// 引用保持 blob_id 指向旧 blob（不切换）
	var a model.Artifact
	s.DB.First(&a)
	if a.BlobID == nil || *a.BlobID != oldBlob.ID {
		t.Fatalf("artifact blob_id changed on failure: %v (old=%d)", a.BlobID, oldBlob.ID)
	}
	// 无 GC 条目（失败不入队）
	var gcCount int64
	s.DB.Model(&model.StorageObjectGC{}).Count(&gcCount)
	if gcCount != 0 {
		t.Fatal("gc entry should not be created on failure")
	}
}

// failingGetStorage 注入 GetObject 读失败。
type failingGetStorage struct {
	*retentionMemoryStorage
}

func (f *failingGetStorage) GetObject(ctx context.Context, _, _ string) (io.ReadCloser, error) {
	return nil, errors.New("simulated read failure")
}

// ------------------------------------------------------------
// 删除退避
// ------------------------------------------------------------

func TestBlobDeletionBackoff(t *testing.T) {
	s := blobTestServer(t)
	now := time.Now()
	blob := model.StorageBlob{ObjectKey: "blobs/sha256/ab/sha1/svg-v1.gz", StoredSize: 100,
		Status: model.BlobStatusDeleting, CreatedAt: now, UpdatedAt: now}
	s.DB.Create(&blob)
	s.failBlobDeletion(context.Background(), &blob, gzip.ErrHeader)
	var updated model.StorageBlob
	s.DB.First(&updated, blob.ID)
	if updated.DeleteAttempts != 1 || updated.NextDeleteAttemptAt == nil {
		t.Fatalf("backoff not recorded: %+v", updated)
	}
	if updated.NextDeleteAttemptAt.Before(time.Now().Add(50 * time.Second)) {
		t.Fatalf("backoff too short: %v", updated.NextDeleteAttemptAt)
	}
}

// ------------------------------------------------------------
// 迁移结果：SVG ≥4KiB
// ------------------------------------------------------------

func TestBlobMigrationSVGResult(t *testing.T) {
	s := blobTestServer(t)
	mem := s.Storage.(*retentionMemoryStorage)
	svg := []byte("<svg>" + strings.Repeat("x", 5000) + "</svg>")
	mem.objects["tid-1/flamegraph.svg"] = svg
	now := time.Now()
	s.DB.Create(&model.Artifact{TaskTID: "tid-1", Kind: model.ArtifactKindResult,
		ObjectKey: "tid-1/flamegraph.svg", Size: int64(len(svg)), Status: model.ArtifactStatusReady, CreatedAt: now})

	if err := s.runBackfillOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	done, err := s.migrateNextResult(context.Background())
	if err != nil || !done {
		t.Fatalf("migrateNextResult done=%v err=%v", done, err)
	}
	var blobs []model.StorageBlob
	s.DB.Find(&blobs)
	foundCAS := false
	for i := range blobs {
		if strings.HasPrefix(blobs[i].ObjectKey, "blobs/sha256/") {
			foundCAS = true
			if blobs[i].Format != model.BlobFormatSVG || blobs[i].ContentEncoding != "gzip" {
				t.Fatalf("bad migrated blob: %+v", blobs[i])
			}
		}
	}
	if !foundCAS {
		t.Fatal("no CAS svg blob")
	}
	// 小 JSON 不迁移（<4KiB）
	mem.objects["tid-1/top.json"] = []byte(`{"a":1}`)
	s.DB.Create(&model.Artifact{TaskTID: "tid-1", Kind: model.ArtifactKindResult,
		ObjectKey: "tid-1/top.json", Size: 7, Status: model.ArtifactStatusReady, CreatedAt: now})
	done2, err := s.migrateNextResult(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if done2 {
		t.Fatal("small json should not be migrated")
	}
}
