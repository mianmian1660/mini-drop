package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mini-drop/apiserver/config"
	"github.com/mini-drop/apiserver/model"
	"github.com/mini-drop/apiserver/pkg/storage"
)

type retentionMemoryStorage struct {
	objects map[string][]byte
	deleted map[string]bool
}

func newRetentionMemoryStorage() *retentionMemoryStorage {
	return &retentionMemoryStorage{objects: map[string][]byte{}, deleted: map[string]bool{}}
}

func (m *retentionMemoryStorage) EnsureBucket(context.Context, string) error { return nil }

func (m *retentionMemoryStorage) PutObject(_ context.Context, _, key string, reader io.Reader, _ int64, _ string) error {
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	m.objects[key] = data
	return nil
}

func (m *retentionMemoryStorage) GetObject(_ context.Context, _, key string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(m.objects[key])), nil
}

func (m *retentionMemoryStorage) PresignedGetURL(context.Context, string, string, time.Duration) (string, error) {
	return "http://example.test/signed", nil
}

func (m *retentionMemoryStorage) ListObjects(_ context.Context, _, prefix string) ([]storage.FileInfo, error) {
	files := []storage.FileInfo{}
	for key, data := range m.objects {
		if strings.HasPrefix(key, prefix) {
			files = append(files, storage.FileInfo{Name: key, Size: int64(len(data)), LastModified: time.Now()})
		}
	}
	return files, nil
}

func (m *retentionMemoryStorage) DeleteObject(_ context.Context, _, key string) error {
	delete(m.objects, key)
	m.deleted[key] = true
	return nil
}

func (m *retentionMemoryStorage) ObjectExists(_ context.Context, _, key string) (bool, error) {
	_, ok := m.objects[key]
	return ok, nil
}

func (m *retentionMemoryStorage) StatObject(_ context.Context, _, key string) (int64, error) {
	data, ok := m.objects[key]
	if !ok {
		return 0, fmt.Errorf("对象不存在: %s", key)
	}
	return int64(len(data)), nil
}

func testKallsymsBody() []byte {
	return []byte("ffffffff81000000 T startup_64\nffffffff81000100 T start_kernel\n")
}

func TestKernelSymbolCheckUploadDeduplicatesAndRecordsArtifacts(t *testing.T) {
	s := newTestAPIServer(t)
	mem := newRetentionMemoryStorage()
	s.Storage = mem
	sumBytes := sha256.Sum256(testKallsymsBody())
	sum := hex.EncodeToString(sumBytes[:])

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/kernel-symbols/check", s.CheckKernelSymbol)
	router.PUT("/api/v1/kernel-symbols/:sha256", s.UploadKernelSymbol)

	checkBody := `{"tid":"tid-one","sha256":"` + sum + `","size_bytes":64,"kernel_release":"5.15","hostname":"h","target_ip":"127.0.0.1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/kernel-symbols/check", strings.NewReader(checkBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"upload_required":true`) {
		t.Fatalf("check before upload status=%d body=%s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodPut, "/api/v1/kernel-symbols/"+sum+"?tid=tid-one&kernel_release=5.15&hostname=h&target_ip=127.0.0.1", bytes.NewReader(testKallsymsBody()))
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("upload status=%d body=%s", w.Code, w.Body.String())
	}
	key := kernelSymbolObjectKey(sum)
	if _, ok := mem.objects[key]; !ok {
		t.Fatalf("kernel symbol object %s not uploaded", key)
	}
	compressed := mem.objects[key]
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("kallsyms must be stored gzip-compressed: %v", err)
	}
	plain, _ := io.ReadAll(zr)
	_ = zr.Close()
	if !bytes.Equal(plain, testKallsymsBody()) {
		t.Fatal("compressed kallsyms content differs from raw hash input")
	}

	checkBody = `{"tid":"tid-two","sha256":"` + sum + `","size_bytes":64,"kernel_release":"5.15","hostname":"h","target_ip":"127.0.0.1"}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/kernel-symbols/check", strings.NewReader(checkBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"upload_required":false`) {
		t.Fatalf("check after upload status=%d body=%s", w.Code, w.Body.String())
	}

	var rows int64
	s.DB.Model(&model.KernelSymbolFile{}).Count(&rows)
	if rows != 1 {
		t.Fatalf("kernel_symbol_files count=%d, want 1", rows)
	}
	s.DB.Model(&model.Artifact{}).Where("object_key = ?", key).Count(&rows)
	if rows != 2 {
		t.Fatalf("kallsyms artifact refs=%d, want 2", rows)
	}
}

func TestDiskGuardRejectsBelowOneGiB(t *testing.T) {
	s := newTestAPIServer(t)
	s.Config = &config.Config{StorageDisk: config.StorageDiskConfig{
		Path: "/tmp", WarningFreeBytes: 8 << 30, CriticalFreeBytes: 4 << 30, MinFreeBytes: 1 << 30,
	}}
	original := readStorageDiskSnapshot
	t.Cleanup(func() { readStorageDiskSnapshot = original })
	readStorageDiskSnapshot = func(string) (uint64, uint64, uint64, error) { return 0, (1 << 30) - 1, 0, nil }
	ok, message, snap := s.canStartCollection(CollectionSourceOneShot)
	if ok || snap.Level != StoragePressureEmergency || snap.CollectionAllowed || !strings.Contains(message, "采集被拒绝") {
		t.Fatalf("low disk guard = ok:%v level:%s allowed:%v message:%q", ok, snap.Level, snap.CollectionAllowed, message)
	}
}

func TestKernelSymbolUploadRejectsHashMismatch(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = newRetentionMemoryStorage()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.PUT("/api/v1/kernel-symbols/:sha256", s.UploadKernelSymbol)

	bad := strings.Repeat("0", 64)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/kernel-symbols/"+bad+"?tid=tid-one", bytes.NewReader(testKallsymsBody()))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "sha256") {
		t.Fatalf("mismatch status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestRetentionCleanerHandlesNormalAndSharedArtifacts(t *testing.T) {
	s := newTestAPIServer(t)
	mem := newRetentionMemoryStorage()
	s.Storage = mem
	s.Config = &config.Config{
		Storage:   config.StorageConfig{Bucket: "drop-data"},
		Retention: config.RetentionConfig{Enabled: true, RawRetentionHours: 168, ResultRetentionHours: 720, BatchLimit: 100},
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	fresh := time.Now()
	kernelKey := kernelSymbolObjectKey(strings.Repeat("a", 64))
	mem.objects["tid-old/perf.data"] = []byte("perf")
	mem.objects[kernelKey] = testKallsymsBody()
	if err := s.DB.Create(&model.KernelSymbolFile{SHA256: strings.Repeat("a", 64), ObjectKey: kernelKey, SizeBytes: int64(len(testKallsymsBody())), Status: model.SymbolFileStatusReady, CreatedAt: old}).Error; err != nil {
		t.Fatalf("create kernel row: %v", err)
	}
	artifacts := []model.Artifact{
		{TaskTID: "tid-old", Kind: model.ArtifactKindRaw, ObjectKey: "tid-old/perf.data", Status: model.ArtifactStatusReady, CreatedAt: old},
		{TaskTID: "tid-one", Kind: model.ArtifactKindRaw, ObjectKey: kernelKey, Status: model.ArtifactStatusReady, CreatedAt: old},
		{TaskTID: "tid-two", Kind: model.ArtifactKindRaw, ObjectKey: kernelKey, Status: model.ArtifactStatusReady, CreatedAt: fresh},
	}
	if err := s.DB.Create(&artifacts).Error; err != nil {
		t.Fatalf("create artifacts: %v", err)
	}

	s.cleanupExpiredArtifacts(context.Background(), []string{model.ArtifactKindRaw}, 7*24*time.Hour, 100)
	if _, ok := mem.objects["tid-old/perf.data"]; ok {
		t.Fatal("expired normal raw object should be deleted")
	}
	if _, ok := mem.objects[kernelKey]; !ok {
		t.Fatal("shared kallsyms object should remain while referenced")
	}
	var refs int64
	s.DB.Model(&model.Artifact{}).Where("object_key = ?", kernelKey).Count(&refs)
	if refs != 1 {
		t.Fatalf("kernel refs=%d, want 1 after first cleanup", refs)
	}

	if err := s.DB.Model(&model.Artifact{}).Where("object_key = ?", kernelKey).Update("created_at", old).Error; err != nil {
		t.Fatalf("age remaining ref: %v", err)
	}
	s.cleanupExpiredArtifacts(context.Background(), []string{model.ArtifactKindRaw}, 7*24*time.Hour, 100)
	if _, ok := mem.objects[kernelKey]; ok {
		t.Fatal("orphaned shared kallsyms object should be deleted")
	}
	var rows int64
	s.DB.Model(&model.KernelSymbolFile{}).Where("object_key = ?", kernelKey).Count(&rows)
	if rows != 0 {
		t.Fatalf("kernel symbol row count=%d, want 0", rows)
	}
}

func TestDeleteTaskCleansArtifactsAndUntrackedTaskObjects(t *testing.T) {
	s := newTestAPIServer(t)
	mem := newRetentionMemoryStorage()
	s.Storage = mem
	task := model.HotmethodTask{TID: "tid-delete-artifacts", Name: "delete", UID: "owner", CreateTime: time.Now()}
	if err := s.DB.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	mem.objects["tid-delete-artifacts/perf.data"] = []byte("perf")
	mem.objects["tid-delete-artifacts/kallsyms"] = []byte("legacy")
	if err := s.DB.Create(&model.Artifact{TaskTID: task.TID, Kind: model.ArtifactKindRaw, ObjectKey: task.TID + "/perf.data", Status: model.ArtifactStatusReady, CreatedAt: time.Now()}).Error; err != nil {
		t.Fatalf("create artifact: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.DELETE("/api/v1/tasks/:tid", s.DeleteTask)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/tasks/"+task.TID, nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", w.Code, w.Body.String())
	}
	if _, ok := mem.objects["tid-delete-artifacts/perf.data"]; ok {
		t.Fatal("registered object should be deleted")
	}
	if _, ok := mem.objects["tid-delete-artifacts/kallsyms"]; ok {
		t.Fatal("untracked legacy task object should be deleted")
	}
}
