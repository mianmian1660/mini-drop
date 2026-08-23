package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mini-drop/apiserver/config"
	"github.com/mini-drop/apiserver/model"
)

// failingMemoryStorage 在 DeleteObject 上可注入失败。
type failingMemoryStorage struct {
	*retentionMemoryStorage
	failDelete bool
}

func (m *failingMemoryStorage) DeleteObject(ctx context.Context, bucket, key string) error {
	if m.failDelete {
		return errors.New("simulated storage failure")
	}
	return m.retentionMemoryStorage.DeleteObject(ctx, bucket, key)
}

// newLifecycleTestServer 构建生命周期测试服务（SQLite 内存库 + 内存存储 + 显式保留配置）。
func newLifecycleTestServer(t *testing.T, mode string) *APIServer {
	t.Helper()
	s := newTestAPIServer(t)
	s.Config = &config.Config{
		Storage: config.StorageConfig{Bucket: "drop-data", PresignExpireSec: 900},
		Retention: config.RetentionConfig{
			Enabled:                  true,
			RawRetentionHours:        24,
			ResultRetentionHours:     720,
			CleanupIntervalSec:       300,
			BatchLimit:               100,
			LifecycleMode:            mode,
			ReconcileIntervalSec:     300,
			ReconcileBatch:           500,
			NotBeforeProtectionHours: 24,
			RawLargeHours:            24,
			RawPortableHours:         168,
			IntermediateHours:        24,
			DiagnosticHours:          72,
			ManifestPermanent:        true,
		},
	}
	mem := newRetentionMemoryStorage()
	s.Storage = mem
	return s
}

func mkArtifact(tid, kind, key string, createdAt time.Time, status string) model.Artifact {
	return model.Artifact{
		TaskTID:   tid,
		Kind:      kind,
		ObjectKey: key,
		Status:    status,
		Size:      100,
		CreatedAt: createdAt,
	}
}

// ------------------------------------------------------------
// 分类与期限
// ------------------------------------------------------------

func TestLifecycleClassifyArtifactRetention(t *testing.T) {
	cases := []struct {
		name   string
		kind   string
		key    string
		status int
		want   string
	}{
		{"raw perf", model.ArtifactKindRaw, "t/perf.data", TaskStatusDone, model.RetentionClassRawLarge},
		{"raw bpf", model.ArtifactKindRaw, "t/raw.bpf", TaskStatusDone, model.RetentionClassRawLarge},
		{"raw pb.gz", model.ArtifactKindRaw, "t/profile.pb.gz", TaskStatusDone, model.RetentionClassRawPortable},
		{"raw collapsed", model.ArtifactKindRaw, "t/profile.collapsed", TaskStatusDone, model.RetentionClassRawPortable},
		{"intermediate", model.ArtifactKindIntermediate, "t/folded.txt", TaskStatusDone, model.RetentionClassIntermediate},
		{"result", model.ArtifactKindResult, "t/flamegraph.svg", TaskStatusDone, model.RetentionClassResult},
		{"manifest", model.ArtifactKindManifest, "t/manifest.json", TaskStatusDone, model.RetentionClassManifest},
		{"log", model.ArtifactKindLog, "t/agent.log", TaskStatusDone, model.RetentionClassDiagnostic},
		{"failed raw → diagnostic", model.ArtifactKindRaw, "t/perf.data", TaskStatusFailed, model.RetentionClassDiagnostic},
		{"canceled intermediate → diagnostic", model.ArtifactKindIntermediate, "t/x.txt", TaskStatusCanceled, model.RetentionClassDiagnostic},
		{"running raw stays raw_large", model.ArtifactKindRaw, "t/perf.data", TaskStatusRunning, model.RetentionClassRawLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyArtifactRetentionFull(tc.kind, tc.key, tc.status); got != tc.want {
				t.Fatalf("classify(%s,%s,%d)=%s want %s", tc.kind, tc.key, tc.status, got, tc.want)
			}
		})
	}
}

func TestLifecyclePolicyVersionChangesWithConfig(t *testing.T) {
	s := newLifecycleTestServer(t, "observe")
	v1 := s.lifecyclePolicyVersion()
	s.Config.Retention.RawLargeHours = 48
	v2 := s.lifecyclePolicyVersion()
	if v1 == v2 {
		t.Fatal("policy version must change when retention durations change")
	}
}

func TestLifecycleComputeExpiryNonTerminal(t *testing.T) {
	s := newLifecycleTestServer(t, "observe")
	now := time.Now()
	a := mkArtifact("t", model.ArtifactKindRaw, "t/perf.data", now.Add(-10*time.Minute), model.ArtifactStatusReady)
	task := &model.HotmethodTask{TID: "t", Status: TaskStatusRunning}
	class, exp, nb := s.lifecycleComputeExpiry(&a, task)
	if class != model.RetentionClassRawLarge {
		t.Fatalf("class=%s", class)
	}
	if exp != nil {
		t.Fatalf("non-terminal task must not set expires_at, got %v", exp)
	}
	if nb != nil {
		t.Fatalf("non-terminal task must keep not_before nil, got %v", nb)
	}
}

func TestLifecycleComputeExpiryTerminalBackfill(t *testing.T) {
	s := newLifecycleTestServer(t, "enforce")
	now := time.Now()
	created := now.Add(-3 * 24 * time.Hour)
	end := now.Add(-2 * 24 * time.Hour)
	a := mkArtifact("t", model.ArtifactKindRaw, "t/perf.data", created, model.ArtifactStatusReady)
	task := &model.HotmethodTask{TID: "t", Status: TaskStatusDone, EndTime: &end}
	class, exp, nb := s.lifecycleComputeExpiry(&a, task)
	if class != model.RetentionClassRawLarge {
		t.Fatalf("class=%s", class)
	}
	if exp == nil {
		t.Fatal("terminal task must set expires_at")
	}
	// 起点 = max(created, end) = end；+24h → end+24h
	wantExpiry := end.Add(24 * time.Hour)
	if !exp.Truncate(time.Second).Equal(wantExpiry.Truncate(time.Second)) {
		t.Fatalf("expires_at=%v want %v", *exp, wantExpiry)
	}
	// 首次回填：not_before = now + 24h
	if nb == nil {
		t.Fatal("initial backfill must set retention_not_before")
	}
	wantNB := now.Add(24 * time.Hour)
	if nb.Before(wantNB.Add(-time.Minute)) || nb.After(wantNB.Add(time.Minute)) {
		t.Fatalf("not_before=%v want ~%v", *nb, wantNB)
	}
}

func TestLifecycleComputeExpiryPolicyShortenProtection(t *testing.T) {
	s := newLifecycleTestServer(t, "enforce")
	now := time.Now()
	created := now.Add(-40 * 24 * time.Hour)
	end := now.Add(-40 * 24 * time.Hour)
	a := mkArtifact("t", model.ArtifactKindResult, "t/flamegraph.svg", created, model.ArtifactStatusReady)
	// 旧策略：result 720h
	oldExpiry := end.Add(720 * time.Hour)
	a.ExpiresAt = &oldExpiry
	nb := now.Add(-1 * time.Hour)
	a.RetentionNotBefore = &nb
	a.RetentionPolicyVersion = "v1-old"
	task := &model.HotmethodTask{TID: "t", Status: TaskStatusDone, EndTime: &end}
	// 新策略：result 只有 24h → 到期时间被大幅缩短
	s.Config.Retention.ResultRetentionHours = 24
	class, exp, nbOut := s.lifecycleComputeExpiry(&a, task)
	if class != model.RetentionClassResult {
		t.Fatalf("class=%s", class)
	}
	if exp == nil {
		t.Fatal("expires_at must be set")
	}
	// 24h 保护：expires_at 至少 now+24h
	guard := now.Add(24 * time.Hour)
	if exp.Before(guard.Add(-time.Minute)) {
		t.Fatalf("shortened expiry %v must be protected to >= %v", *exp, guard)
	}
	if nbOut == nil || nbOut.Before(guard.Add(-time.Minute)) {
		t.Fatalf("shortened policy must lift not_before to >= %v, got %v", guard, nbOut)
	}
}

func TestLifecycleComputeExpiryManifestPermanent(t *testing.T) {
	s := newLifecycleTestServer(t, "enforce")
	now := time.Now()
	a := mkArtifact("t", model.ArtifactKindManifest, "t/manifest.json", now.Add(-10*time.Minute), model.ArtifactStatusReady)
	task := &model.HotmethodTask{TID: "t", Status: TaskStatusDone, EndTime: &now}
	class, exp, _ := s.lifecycleComputeExpiry(&a, task)
	if class != model.RetentionClassManifest {
		t.Fatalf("class=%s", class)
	}
	if exp != nil {
		t.Fatal("manifest must never expire")
	}
}

// ------------------------------------------------------------
// Reconciler
// ------------------------------------------------------------

func TestLifecycleReconcileBackfillsHistorical(t *testing.T) {
	s := newLifecycleTestServer(t, "observe")
	now := time.Now()
	// 终态任务 + 历史 artifact
	if err := s.DB.Create(&model.HotmethodTask{TID: "t-done", Name: "d", UID: "u", Status: TaskStatusDone, CreateTime: now.Add(-3 * 24 * time.Hour)}).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	arts := []model.Artifact{
		mkArtifact("t-done", model.ArtifactKindRaw, "t-done/perf.data", now.Add(-3*24*time.Hour), model.ArtifactStatusReady),
		mkArtifact("t-done", model.ArtifactKindResult, "t-done/flamegraph.svg", now.Add(-3*24*time.Hour), model.ArtifactStatusReady),
		mkArtifact("t-done", model.ArtifactKindRaw, "t-done/profile.collapsed", now.Add(-3*24*time.Hour), model.ArtifactStatusReady),
	}
	if err := s.DB.Create(&arts).Error; err != nil {
		t.Fatalf("create artifacts: %v", err)
	}

	backlog := s.reconcileLifecycle(context.Background())
	if backlog != 0 {
		t.Fatalf("backlog after reconcile = %d, want 0", backlog)
	}
	var rows []model.Artifact
	if err := s.DB.Order("id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	wantClass := []string{model.RetentionClassRawLarge, model.RetentionClassResult, model.RetentionClassRawPortable}
	for i, row := range rows {
		if row.Retention != wantClass[i] {
			t.Fatalf("artifact %d retention=%s want %s", i, row.Retention, wantClass[i])
		}
		if row.RetentionPolicyVersion != s.lifecyclePolicyVersion() {
			t.Fatalf("artifact %d policy version not updated", i)
		}
		if row.ExpiresAt == nil {
			t.Fatalf("artifact %d must have expires_at for terminal task", i)
		}
		if row.RetentionNotBefore == nil {
			t.Fatalf("artifact %d must have not_before after backfill", i)
		}
	}
}

func TestLifecycleReconcileSkipsDeletedTombstone(t *testing.T) {
	s := newLifecycleTestServer(t, "observe")
	now := time.Now()
	deletedAt := now.Add(-1 * time.Hour)
	tomb := mkArtifact("t-del", model.ArtifactKindRaw, "t-del/perf.data", now.Add(-48*time.Hour), model.ArtifactStatusDeleted)
	tomb.DeletedAt = &deletedAt
	if err := s.DB.Create(&tomb).Error; err != nil {
		t.Fatalf("create tombstone: %v", err)
	}
	_ = s.reconcileLifecycle(context.Background())
	var row model.Artifact
	if err := s.DB.First(&row, tomb.ID).Error; err != nil {
		t.Fatalf("reload tombstone: %v", err)
	}
	if row.RetentionPolicyVersion != "" {
		t.Fatal("reconciler must not touch deleted tombstones")
	}
}

func TestLifecycleReconcileRunsAgainWhenTaskBecomesTerminal(t *testing.T) {
	s := newLifecycleTestServer(t, "observe")
	now := time.Now()
	task := model.HotmethodTask{TID: "t-transition", Name: "t", UID: "u", Status: TaskStatusRunning, CreateTime: now.Add(-time.Hour)}
	if err := s.DB.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	a := mkArtifact(task.TID, model.ArtifactKindRaw, task.TID+"/perf.data", now.Add(-time.Hour), model.ArtifactStatusReady)
	if err := s.DB.Create(&a).Error; err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	if backlog := s.reconcileLifecycle(context.Background()); backlog != 0 {
		t.Fatalf("initial backlog=%d", backlog)
	}
	var active model.Artifact
	if err := s.DB.First(&active, a.ID).Error; err != nil {
		t.Fatalf("reload active: %v", err)
	}
	if active.RetentionTaskState != retentionTaskStateActive || active.ExpiresAt != nil {
		t.Fatalf("active lifecycle state=%s expires=%v", active.RetentionTaskState, active.ExpiresAt)
	}
	end := now
	if err := s.DB.Model(&model.HotmethodTask{}).Where("tid = ?", task.TID).
		Updates(map[string]interface{}{"status": TaskStatusDone, "end_time": &end}).Error; err != nil {
		t.Fatalf("finish task: %v", err)
	}
	if backlog := s.reconcileLifecycle(context.Background()); backlog != 0 {
		t.Fatalf("terminal backlog=%d", backlog)
	}
	var done model.Artifact
	if err := s.DB.First(&done, a.ID).Error; err != nil {
		t.Fatalf("reload done: %v", err)
	}
	if done.RetentionTaskState != retentionTaskStateDone || done.ExpiresAt == nil {
		t.Fatalf("terminal lifecycle state=%s expires=%v", done.RetentionTaskState, done.ExpiresAt)
	}
}

// ------------------------------------------------------------
// Cleaner：observe / enforce / 保护 / 重试 / kallsyms
// ------------------------------------------------------------

func TestLifecycleCleanerObserveDoesNotDelete(t *testing.T) {
	s := newLifecycleTestServer(t, "observe")
	mem := s.Storage.(*retentionMemoryStorage)
	now := time.Now()
	end := now.Add(-48 * time.Hour)
	if err := s.DB.Create(&model.HotmethodTask{TID: "t", Name: "t", UID: "u", Status: TaskStatusDone, CreateTime: end, EndTime: &end}).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	expired := now.Add(-1 * time.Hour)
	nb := now.Add(-1 * time.Hour)
	a := mkArtifact("t", model.ArtifactKindRaw, "t/perf.data", end, model.ArtifactStatusReady)
	a.ExpiresAt = &expired
	a.RetentionNotBefore = &nb
	a.RetentionPolicyVersion = s.lifecyclePolicyVersion()
	a.Retention = model.RetentionClassRawLarge
	if err := s.DB.Create(&a).Error; err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	mem.objects["t/perf.data"] = []byte("perf")

	// 走完整周期（内部检查模式）：observe 只记录候选，不自动删除
	s.runArtifactLifecycleCycle(context.Background())
	var row model.Artifact
	if err := s.DB.First(&row, a.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if row.Status != model.ArtifactStatusReady {
		t.Fatalf("observe mode must not delete, status=%s", row.Status)
	}
	if _, ok := mem.objects["t/perf.data"]; !ok {
		t.Fatal("observe mode must not remove objects")
	}
	if atomic.LoadInt64(&metricArtifactCleanupDeletedTotal) != 0 {
		t.Fatalf("observe mode must keep auto-delete counter at 0, got %d", atomic.LoadInt64(&metricArtifactCleanupDeletedTotal))
	}
}

func TestLifecycleCleanerEnforceDeletesAndKeepsTombstone(t *testing.T) {
	resetMetricsForTest()
	s := newLifecycleTestServer(t, "enforce")
	mem := s.Storage.(*retentionMemoryStorage)
	now := time.Now()
	if err := s.DB.Create(&model.HotmethodTask{TID: "t", Name: "t", UID: "u", Status: TaskStatusDone, CreateTime: now.Add(-48 * time.Hour)}).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	expired := now.Add(-1 * time.Hour)
	a := mkArtifact("t", model.ArtifactKindRaw, "t/perf.data", now.Add(-48*time.Hour), model.ArtifactStatusReady)
	a.ExpiresAt = &expired
	a.RetentionPolicyVersion = s.lifecyclePolicyVersion()
	nb := now.Add(-1 * time.Hour)
	a.RetentionNotBefore = &nb
	if err := s.DB.Create(&a).Error; err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	mem.objects["t/perf.data"] = []byte("perf")

	s.processExpiredCandidates(context.Background())

	var row model.Artifact
	if err := s.DB.First(&row, a.ID).Error; err != nil {
		t.Fatalf("tombstone row must persist: %v", err)
	}
	if row.Status != model.ArtifactStatusDeleted || row.DeletedAt == nil || row.DeleteReason != model.DeleteReasonExpired {
		t.Fatalf("tombstone wrong: status=%s deleted_at=%v reason=%s", row.Status, row.DeletedAt, row.DeleteReason)
	}
	if _, ok := mem.objects["t/perf.data"]; ok {
		t.Fatal("expired object must be deleted")
	}
	if _, ok := mem.deleted["t/perf.data"]; !ok {
		t.Fatal("object deletion must be recorded")
	}
	if atomic.LoadInt64(&metricArtifactCleanupDeletedTotal) != 1 {
		t.Fatalf("cleanup_deleted_total=%d want 1", atomic.LoadInt64(&metricArtifactCleanupDeletedTotal))
	}
}

func TestLifecycleCleanerSkipsPinnedTask(t *testing.T) {
	s := newLifecycleTestServer(t, "enforce")
	mem := s.Storage.(*retentionMemoryStorage)
	now := time.Now()
	if err := s.DB.Create(&model.HotmethodTask{TID: "t", Name: "t", UID: "u", Status: TaskStatusDone, CreateTime: now.Add(-48 * time.Hour), ArtifactsPinned: true}).Error; err != nil {
		t.Fatalf("create pinned task: %v", err)
	}
	expired := now.Add(-1 * time.Hour)
	a := mkArtifact("t", model.ArtifactKindRaw, "t/perf.data", now.Add(-48*time.Hour), model.ArtifactStatusReady)
	a.ExpiresAt = &expired
	a.RetentionNotBefore = &expired
	if err := s.DB.Create(&a).Error; err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	mem.objects["t/perf.data"] = []byte("perf")

	s.processExpiredCandidates(context.Background())
	var row model.Artifact
	if err := s.DB.First(&row, a.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if row.Status != model.ArtifactStatusReady {
		t.Fatalf("pinned artifact must not be deleted, status=%s", row.Status)
	}
}

func TestLifecycleCleanerSkipsActiveJobInput(t *testing.T) {
	s := newLifecycleTestServer(t, "enforce")
	now := time.Now()
	if err := s.DB.Create(&model.HotmethodTask{TID: "t", Name: "t", UID: "u", Status: TaskStatusDone, CreateTime: now.Add(-48 * time.Hour)}).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	expired := now.Add(-1 * time.Hour)
	a := mkArtifact("t", model.ArtifactKindRaw, "t/perf.data", now.Add(-48*time.Hour), model.ArtifactStatusReady)
	a.ExpiresAt = &expired
	a.RetentionNotBefore = &expired
	if err := s.DB.Create(&a).Error; err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	ids, _ := json.Marshal([]uint{a.ID})
	if err := s.DB.Create(&model.AnalysisJob{
		TaskTID:          "t",
		Pipeline:         "perf_flamegraph",
		Status:           model.AnalysisJobStatusRunning,
		InputArtifactIDs: ids,
		CreatedAt:        now,
		UpdatedAt:        now,
	}).Error; err != nil {
		t.Fatalf("create job: %v", err)
	}

	s.processExpiredCandidates(context.Background())
	var row model.Artifact
	if err := s.DB.First(&row, a.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if row.Status != model.ArtifactStatusReady {
		t.Fatalf("active job input must not be deleted, status=%s", row.Status)
	}
}

func TestLifecycleCleanerRetryOnFailureAndBackoff(t *testing.T) {
	s := newLifecycleTestServer(t, "enforce")
	mem := &failingMemoryStorage{retentionMemoryStorage: newRetentionMemoryStorage(), failDelete: true}
	s.Storage = mem
	now := time.Now()
	if err := s.DB.Create(&model.HotmethodTask{TID: "t", Name: "t", UID: "u", Status: TaskStatusDone, CreateTime: now.Add(-48 * time.Hour)}).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	expired := now.Add(-1 * time.Hour)
	a := mkArtifact("t", model.ArtifactKindRaw, "t/perf.data", now.Add(-48*time.Hour), model.ArtifactStatusReady)
	a.ExpiresAt = &expired
	a.RetentionNotBefore = &expired
	if err := s.DB.Create(&a).Error; err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	mem.objects["t/perf.data"] = []byte("perf")

	s.processExpiredCandidates(context.Background())
	var row model.Artifact
	if err := s.DB.First(&row, a.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if row.Status != model.ArtifactStatusDeleting {
		t.Fatalf("failed deletion must stay deleting, status=%s", row.Status)
	}
	if row.DeleteAttempts != 1 {
		t.Fatalf("delete_attempts=%d want 1", row.DeleteAttempts)
	}
	if row.NextDeleteAttemptAt == nil {
		t.Fatal("next_delete_attempt_at must be set")
	}
	// 第一次失败 → 1m 退避
	wantNext := now.Add(deleteBackoffMin)
	if row.NextDeleteAttemptAt.Before(wantNext.Add(-time.Second)) || row.NextDeleteAttemptAt.After(wantNext.Add(time.Second)) {
		t.Fatalf("next attempt=%v want ~%v", *row.NextDeleteAttemptAt, wantNext)
	}
	if atomic.LoadInt64(&metricArtifactCleanupFailuresTotal) != 1 {
		t.Fatalf("failures=%d want 1", atomic.LoadInt64(&metricArtifactCleanupFailuresTotal))
	}

	// 故障恢复后，到期重试成功
	mem.failDelete = false
	row.NextDeleteAttemptAt = &now
	if err := s.DB.Model(&model.Artifact{}).Where("id = ?", a.ID).Update("next_delete_attempt_at", &now).Error; err != nil {
		t.Fatalf("update retry time: %v", err)
	}
	s.processDeletingRetries(context.Background())
	if err := s.DB.First(&row, a.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if row.Status != model.ArtifactStatusDeleted {
		t.Fatalf("retry should succeed, status=%s", row.Status)
	}
}

func TestLifecycleCleanerStorageDisconnectedKeepsDeleting(t *testing.T) {
	s := newLifecycleTestServer(t, "enforce")
	now := time.Now()
	end := now.Add(-48 * time.Hour)
	if err := s.DB.Create(&model.HotmethodTask{TID: "t-offline", Name: "t", UID: "u", Status: TaskStatusDone, CreateTime: end, EndTime: &end}).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	expired := now.Add(-time.Hour)
	a := mkArtifact("t-offline", model.ArtifactKindRaw, "t-offline/perf.data", end, model.ArtifactStatusReady)
	a.ExpiresAt = &expired
	a.RetentionNotBefore = &expired
	a.RetentionPolicyVersion = s.lifecyclePolicyVersion()
	a.RetentionTaskState = retentionTaskStateDone
	if err := s.DB.Create(&a).Error; err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	s.Storage = nil
	s.processExpiredCandidates(context.Background())
	var row model.Artifact
	if err := s.DB.First(&row, a.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if row.Status != model.ArtifactStatusDeleting || row.DeletedAt != nil || row.DeleteAttempts != 1 || row.NextDeleteAttemptAt == nil {
		t.Fatalf("offline deletion must remain retryable: %+v", row)
	}
}

func TestDeletedTaskArtifactRetryIsNotFilteredOut(t *testing.T) {
	s := newLifecycleTestServer(t, "enforce")
	mem := &failingMemoryStorage{retentionMemoryStorage: newRetentionMemoryStorage(), failDelete: true}
	s.Storage = mem
	now := time.Now()
	task := model.HotmethodTask{TID: "t-deleted-retry", Name: "t", UID: "u", Status: TaskStatusDone, CreateTime: now}
	if err := s.DB.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	a := mkArtifact(task.TID, model.ArtifactKindRaw, task.TID+"/perf.data", now, model.ArtifactStatusReady)
	if err := s.DB.Create(&a).Error; err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	mem.objects[a.ObjectKey] = []byte("perf")
	if err := s.DB.Delete(&task).Error; err != nil {
		t.Fatalf("soft delete task: %v", err)
	}
	s.taskDeletedArtifacts(context.Background(), task.TID)
	var deleting model.Artifact
	if err := s.DB.First(&deleting, a.ID).Error; err != nil {
		t.Fatalf("reload deleting: %v", err)
	}
	if deleting.Status != model.ArtifactStatusDeleting {
		t.Fatalf("status=%s want deleting", deleting.Status)
	}
	mem.failDelete = false
	past := now.Add(-time.Minute)
	if err := s.DB.Model(&model.Artifact{}).Where("id = ?", a.ID).Update("next_delete_attempt_at", &past).Error; err != nil {
		t.Fatalf("age retry: %v", err)
	}
	s.processDeletingRetries(context.Background())
	var tomb model.Artifact
	if err := s.DB.First(&tomb, a.ID).Error; err != nil {
		t.Fatalf("reload tombstone: %v", err)
	}
	if tomb.Status != model.ArtifactStatusDeleted || tomb.DeletedAt == nil {
		t.Fatalf("soft-deleted task retry did not finish: %+v", tomb)
	}
}

func TestLifecycleCleanerKallsymsSharedObject(t *testing.T) {
	resetMetricsForTest()
	s := newLifecycleTestServer(t, "enforce")
	mem := s.Storage.(*retentionMemoryStorage)
	now := time.Now()
	sha := strings.Repeat("b", 64)
	key := kernelSymbolObjectKey(sha)
	if err := s.DB.Create(&model.KernelSymbolFile{SHA256: sha, ObjectKey: key, SizeBytes: 64, Status: model.SymbolFileStatusReady, CreatedAt: now.Add(-48 * time.Hour)}).Error; err != nil {
		t.Fatalf("create kernel row: %v", err)
	}
	expired := now.Add(-1 * time.Hour)
	// a1 到期（候选），a2 未到期（活跃引用）
	a1 := mkArtifact("t1", model.ArtifactKindRaw, key, now.Add(-48*time.Hour), model.ArtifactStatusReady)
	a1.ExpiresAt = &expired
	a1.RetentionNotBefore = &expired
	a2 := mkArtifact("t2", model.ArtifactKindRaw, key, now.Add(-48*time.Hour), model.ArtifactStatusReady)
	fresh := now.Add(48 * time.Hour)
	a2.ExpiresAt = &fresh
	if err := s.DB.Create(&a1).Error; err != nil {
		t.Fatalf("create a1: %v", err)
	}
	if err := s.DB.Create(&a2).Error; err != nil {
		t.Fatalf("create a2: %v", err)
	}
	mem.objects[key] = testKallsymsBody()

	// 第一轮：a1 到期 → tombstone，但共享对象必须保留（a2 仍引用）
	s.processExpiredCandidates(context.Background())
	if _, ok := mem.objects[key]; !ok {
		t.Fatal("shared kallsyms object must survive while any non-deleted ref exists")
	}
	var liveRefs int64
	s.DB.Model(&model.Artifact{}).Where("object_key = ? AND deleted_at IS NULL", key).Count(&liveRefs)
	if liveRefs != 1 {
		t.Fatalf("live refs=%d want 1", liveRefs)
	}
	var tombRefs int64
	s.DB.Model(&model.Artifact{}).Where("object_key = ? AND status = ?", key, model.ArtifactStatusDeleted).Count(&tombRefs)
	if tombRefs != 1 {
		t.Fatalf("tombstone refs=%d want 1", tombRefs)
	}
	var kernelRows int64
	s.DB.Model(&model.KernelSymbolFile{}).Where("object_key = ?", key).Count(&kernelRows)
	if kernelRows != 1 {
		t.Fatalf("kernel ledger rows=%d want 1 while referenced", kernelRows)
	}

	// 第二轮：a2 也到期 → tombstone → 无活跃引用 → 共享对象与 kernel ledger 删除
	if err := s.DB.Model(&model.Artifact{}).Where("id = ?", a2.ID).Updates(map[string]interface{}{"expires_at": &expired, "retention_not_before": &expired}).Error; err != nil {
		t.Fatalf("age a2: %v", err)
	}
	s.processExpiredCandidates(context.Background())
	if _, ok := mem.objects[key]; ok {
		t.Fatal("orphaned kallsyms object should be deleted when no non-deleted refs remain")
	}
	kernelRows = 0
	s.DB.Model(&model.KernelSymbolFile{}).Where("object_key = ?", key).Count(&kernelRows)
	if kernelRows != 0 {
		t.Fatalf("kernel ledger rows=%d want 0", kernelRows)
	}
	// 两个 tombstone 行保留
	var tombstones int64
	s.DB.Model(&model.Artifact{}).Where("object_key = ? AND deleted_at IS NOT NULL", key).Count(&tombstones)
	if tombstones != 2 {
		t.Fatalf("tombstones=%d want 2", tombstones)
	}
}

func TestLifecycleCleanerKallsymsLastReferenceRetriesBeforeTombstone(t *testing.T) {
	s := newLifecycleTestServer(t, "enforce")
	mem := &failingMemoryStorage{retentionMemoryStorage: newRetentionMemoryStorage(), failDelete: true}
	s.Storage = mem
	now := time.Now()
	sha := strings.Repeat("c", 64)
	key := kernelSymbolObjectKey(sha)
	if err := s.DB.Create(&model.KernelSymbolFile{SHA256: sha, ObjectKey: key, SizeBytes: 64, Status: model.SymbolFileStatusReady, CreatedAt: now}).Error; err != nil {
		t.Fatalf("create kernel ledger: %v", err)
	}
	expired := now.Add(-time.Hour)
	a := mkArtifact("orphan-task", model.ArtifactKindRaw, key, now.Add(-48*time.Hour), model.ArtifactStatusReady)
	a.ExpiresAt = &expired
	a.RetentionNotBefore = &expired
	if err := s.DB.Create(&a).Error; err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	mem.objects[key] = testKallsymsBody()
	s.processExpiredCandidates(context.Background())
	var row model.Artifact
	if err := s.DB.First(&row, a.ID).Error; err != nil {
		t.Fatalf("reload failed delete: %v", err)
	}
	if row.Status != model.ArtifactStatusDeleting || row.DeletedAt != nil {
		t.Fatalf("failed shared delete must remain retryable: %+v", row)
	}
	var ledgers int64
	s.DB.Model(&model.KernelSymbolFile{}).Where("object_key = ?", key).Count(&ledgers)
	if ledgers != 1 {
		t.Fatalf("ledger removed before object deletion succeeded")
	}
	mem.failDelete = false
	past := now.Add(-time.Minute)
	_ = s.DB.Model(&model.Artifact{}).Where("id = ?", a.ID).Update("next_delete_attempt_at", &past).Error
	s.processDeletingRetries(context.Background())
	if err := s.DB.First(&row, a.ID).Error; err != nil {
		t.Fatalf("reload tombstone: %v", err)
	}
	if row.Status != model.ArtifactStatusDeleted || row.DeletedAt == nil {
		t.Fatalf("shared retry did not complete: %+v", row)
	}
	s.DB.Model(&model.KernelSymbolFile{}).Where("object_key = ?", key).Count(&ledgers)
	if ledgers != 0 {
		t.Fatalf("ledger count=%d want 0", ledgers)
	}
}

func TestLifecycleCleanerClaimIsAtomic(t *testing.T) {
	s := newLifecycleTestServer(t, "enforce")
	now := time.Now()
	expired := now.Add(-1 * time.Hour)
	a := mkArtifact("t", model.ArtifactKindRaw, "t/perf.data", now.Add(-48*time.Hour), model.ArtifactStatusReady)
	a.ExpiresAt = &expired
	a.RetentionNotBefore = &expired
	if err := s.DB.Create(&a).Error; err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	// 模拟两个并发 cleaner：第一次领取成功
	if !s.claimArtifactForDeletion(context.Background(), &a, false) {
		t.Fatal("first claim should win")
	}
	var row model.Artifact
	s.DB.First(&row, a.ID)
	if row.Status != model.ArtifactStatusDeleting {
		t.Fatalf("status=%s want deleting", row.Status)
	}
	// 第二次领取（内存里仍是 ready）必须失败——行已被切到 deleting
	if s.claimArtifactForDeletion(context.Background(), &a, false) {
		t.Fatal("second claim must not win (CAS on status)")
	}
}

func TestTaskDeletedOverridesPin(t *testing.T) {
	s := newLifecycleTestServer(t, "observe") // observe 模式任务主动删除仍执行
	mem := s.Storage.(*retentionMemoryStorage)
	now := time.Now()
	if err := s.DB.Create(&model.HotmethodTask{TID: "t", Name: "t", UID: "u", Status: TaskStatusDone, CreateTime: now.Add(-1 * time.Hour), ArtifactsPinned: true}).Error; err != nil {
		t.Fatalf("create pinned task: %v", err)
	}
	a := mkArtifact("t", model.ArtifactKindRaw, "t/perf.data", now.Add(-1*time.Hour), model.ArtifactStatusReady)
	if err := s.DB.Create(&a).Error; err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	mem.objects["t/perf.data"] = []byte("perf")

	s.taskDeletedArtifacts(context.Background(), "t")
	var row model.Artifact
	if err := s.DB.First(&row, a.ID).Error; err != nil {
		t.Fatalf("tombstone row must persist: %v", err)
	}
	if row.Status != model.ArtifactStatusDeleted || row.DeleteReason != model.DeleteReasonTaskDeleted {
		t.Fatalf("task delete must ignore pin: status=%s reason=%s", row.Status, row.DeleteReason)
	}
	if _, ok := mem.objects["t/perf.data"]; ok {
		t.Fatal("task-deleted object must be removed even in observe mode")
	}
}

func TestTombstoneNotResurrectedByNotify(t *testing.T) {
	s := newLifecycleTestServer(t, "observe")
	mem := s.Storage.(*retentionMemoryStorage)
	now := time.Now()
	if err := s.DB.Create(&model.HotmethodTask{TID: "t", Name: "t", UID: "u", Status: TaskStatusDone, CreateTime: now.Add(-1 * time.Hour)}).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	deletedAt := now.Add(-10 * time.Minute)
	a := mkArtifact("t", model.ArtifactKindRaw, "t/perf.data", now.Add(-1*time.Hour), model.ArtifactStatusDeleted)
	a.DeletedAt = &deletedAt
	a.DeleteReason = model.DeleteReasonExpired
	if err := s.DB.Create(&a).Error; err != nil {
		t.Fatalf("create tombstone: %v", err)
	}
	mem.objects[a.ObjectKey] = []byte("late upload")
	// 迟到通知（partial=false）不得复活 tombstone
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/internal/task-notify", s.NotifyTaskResult)
	body := `{"task_id":"t","cos_key":"t/perf.data","artifact_size":100,"artifact_sha256":"abc","partial":false}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/task-notify", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("notify status=%d body=%s", w.Code, w.Body.String())
	}
	var row model.Artifact
	if err := s.DB.First(&row, a.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if row.Status != model.ArtifactStatusDeleted || row.DeletedAt == nil {
		t.Fatalf("tombstone must not be resurrected: status=%s deleted_at=%v", row.Status, row.DeletedAt)
	}
	if _, ok := mem.objects[a.ObjectKey]; ok {
		t.Fatal("late object using a tombstoned key must be removed")
	}
}

func TestPartialNotifyThenCompleteSwitchesUploadingToReady(t *testing.T) {
	s := newLifecycleTestServer(t, "observe")
	now := time.Now()
	if err := s.DB.Create(&model.HotmethodTask{TID: "t", Name: "t", UID: "u", Status: TaskStatusRunning, CreateTime: now.Add(-1 * time.Hour)}).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/internal/task-notify", s.NotifyTaskResult)

	// partial 通知 → artifact uploading
	body := `{"task_id":"t","cos_key":"t/perf.data","artifact_size":100,"artifact_sha256":"abc","partial":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/task-notify", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("partial notify status=%d body=%s", w.Code, w.Body.String())
	}
	var a model.Artifact
	if err := s.DB.Where("task_tid = ? AND kind = ?", "t", model.ArtifactKindRaw).First(&a).Error; err != nil {
		t.Fatalf("load artifact: %v", err)
	}
	if a.Status != model.ArtifactStatusUploading {
		t.Fatalf("partial notify must set uploading, got %s", a.Status)
	}

	// 完整通知 → uploading 切回 ready
	body = `{"task_id":"t","cos_key":"t/perf.data","artifact_size":200,"artifact_sha256":"abc","partial":false}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/internal/task-notify", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("complete notify status=%d body=%s", w.Code, w.Body.String())
	}
	if err := s.DB.First(&a, a.ID).Error; err != nil {
		t.Fatalf("reload artifact: %v", err)
	}
	if a.Status != model.ArtifactStatusReady {
		t.Fatalf("complete notify must switch uploading back to ready, got %s", a.Status)
	}
}

func TestLifecycleCleanerReclaimsStaleUploading(t *testing.T) {
	resetMetricsForTest()
	s := newLifecycleTestServer(t, "enforce")
	mem := s.Storage.(*retentionMemoryStorage)
	now := time.Now()
	end := now.Add(-2 * time.Hour)
	if err := s.DB.Create(&model.HotmethodTask{TID: "t", Name: "t", UID: "u", Status: TaskStatusDone, CreateTime: end, EndTime: &end}).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	// stale uploading：任务已终态、created_at 早于 1h 窗口
	stale := mkArtifact("t", model.ArtifactKindRaw, "t/perf.data", now.Add(-2*time.Hour), model.ArtifactStatusUploading)
	if err := s.DB.Create(&stale).Error; err != nil {
		t.Fatalf("create stale uploading: %v", err)
	}
	// 非 stale uploading：created_at 在 1h 窗口内 → 不回收
	fresh := mkArtifact("t", model.ArtifactKindRaw, "t/raw.bpf", now.Add(-10*time.Minute), model.ArtifactStatusUploading)
	if err := s.DB.Create(&fresh).Error; err != nil {
		t.Fatalf("create fresh uploading: %v", err)
	}
	mem.objects["t/perf.data"] = []byte("perf")
	mem.objects["t/raw.bpf"] = []byte("bpf")

	s.processExpiredCandidates(context.Background())

	var staleRow model.Artifact
	if err := s.DB.First(&staleRow, stale.ID).Error; err != nil {
		t.Fatalf("reload stale: %v", err)
	}
	if staleRow.Status != model.ArtifactStatusDeleted || staleRow.DeleteReason != model.DeleteReasonStaleUploading {
		t.Fatalf("stale uploading must be reclaimed as diagnostic: status=%s reason=%s", staleRow.Status, staleRow.DeleteReason)
	}
	var freshRow model.Artifact
	if err := s.DB.First(&freshRow, fresh.ID).Error; err != nil {
		t.Fatalf("reload fresh: %v", err)
	}
	if freshRow.Status != model.ArtifactStatusUploading {
		t.Fatalf("fresh uploading must not be reclaimed, status=%s", freshRow.Status)
	}
}

// ------------------------------------------------------------
// Pin API
// ------------------------------------------------------------

func TestPinTaskArtifactsAPI(t *testing.T) {
	s := newLifecycleTestServer(t, "observe")
	now := time.Now()
	if err := s.DB.Create(&model.HotmethodTask{TID: "t", Name: "t", UID: "owner", Status: TaskStatusDone, CreateTime: now.Add(-1 * time.Hour)}).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	a1 := mkArtifact("t", model.ArtifactKindRaw, "t/perf.data", now.Add(-1*time.Hour), model.ArtifactStatusReady)
	a2 := mkArtifact("t", model.ArtifactKindResult, "t/flamegraph.svg", now.Add(-1*time.Hour), model.ArtifactStatusReady)
	if err := s.DB.Create(&[]model.Artifact{a1, a2}).Error; err != nil {
		t.Fatalf("create artifacts: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/tasks/:tid/artifacts/pin", s.PinTaskArtifacts)

	// Viewer 不能 pin
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t/artifacts/pin", strings.NewReader(`{"pinned":true}`))
	req.Header.Set("Drop-User-Uid", "owner")
	req.Header.Set("Drop-User-Role", "Viewer")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer pin status=%d", w.Code)
	}

	// 非 owner 不能 pin
	req = httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t/artifacts/pin", strings.NewReader(`{"pinned":true}`))
	req.Header.Set("Drop-User-Uid", "other")
	req.Header.Set("Drop-User-Role", "Operator")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-owner pin status=%d", w.Code)
	}

	// owner pin
	req = httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t/artifacts/pin", strings.NewReader(`{"pinned":true,"reason":"验收保留"}`))
	req.Header.Set("Drop-User-Uid", "owner")
	req.Header.Set("Drop-User-Name", "Owner")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("pin status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Data map[string]interface{} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Data["pinned"] != true {
		t.Fatalf("pin response pinned=%v", resp.Data["pinned"])
	}
	if resp.Data["protected_artifacts"].(float64) != 2 {
		t.Fatalf("protected_artifacts=%v want 2", resp.Data["protected_artifacts"])
	}

	var task model.HotmethodTask
	s.DB.Where("tid = ?", "t").First(&task)
	if !task.ArtifactsPinned || task.ArtifactsPinnedBy != "Owner" || task.ArtifactsPinReason != "验收保留" {
		t.Fatalf("task pin state wrong: %+v", task.ArtifactsPinned)
	}
	// 审计事件
	var events []model.TaskStatusEvent
	s.DB.Where("tid = ? AND source_module = ?", "t", "artifact_pin").Find(&events)
	if len(events) != 1 {
		t.Fatalf("pin audit events=%d want 1", len(events))
	}

	// unpin
	req = httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t/artifacts/pin", strings.NewReader(`{"pinned":false}`))
	req.Header.Set("Drop-User-Uid", "owner")
	req.Header.Set("Drop-User-Name", "Owner")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("unpin status=%d", w.Code)
	}
	s.DB.Where("tid = ?", "t").First(&task)
	if task.ArtifactsPinned {
		t.Fatal("task must be unpinned")
	}
}

func TestPinTaskArtifactsRejectsDeletingArtifact(t *testing.T) {
	s := newLifecycleTestServer(t, "enforce")
	now := time.Now()
	if err := s.DB.Create(&model.HotmethodTask{TID: "t-pin-race", Name: "t", UID: "owner", Status: TaskStatusDone, CreateTime: now}).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	a := mkArtifact("t-pin-race", model.ArtifactKindRaw, "t-pin-race/perf.data", now, model.ArtifactStatusDeleting)
	if err := s.DB.Create(&a).Error; err != nil {
		t.Fatalf("create deleting artifact: %v", err)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/tasks/:tid/artifacts/pin", s.PinTaskArtifacts)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t-pin-race/artifacts/pin", strings.NewReader(`{"pinned":true}`))
	req.Header.Set("Drop-User-Uid", "owner")
	req.Header.Set("Drop-User-Role", "Operator")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("pin deleting artifact status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestNotifyRegistersManifestArtifact(t *testing.T) {
	s := newLifecycleTestServer(t, "observe")
	now := time.Now()
	if err := s.DB.Create(&model.HotmethodTask{TID: "t-manifest", Name: "t", UID: "u", Status: TaskStatusRunning, CreateTime: now}).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/internal/task-notify", s.NotifyTaskResult)
	body := `{"task_id":"t-manifest","cos_key":"t-manifest/perf.data","manifest_key":"t-manifest/manifest.json"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/task-notify", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("notify status=%d body=%s", w.Code, w.Body.String())
	}
	var manifest model.Artifact
	if err := s.DB.Where("task_tid = ? AND kind = ? AND object_key = ?", "t-manifest", model.ArtifactKindManifest, "t-manifest/manifest.json").First(&manifest).Error; err != nil {
		t.Fatalf("manifest artifact not registered: %v", err)
	}
	if manifest.Status != model.ArtifactStatusReady || manifest.ContentType != "application/json" {
		t.Fatalf("manifest metadata wrong: %+v", manifest)
	}
}

// ------------------------------------------------------------
// 列表 / 下载 API
// ------------------------------------------------------------

func TestArtifactListExcludesDeletedAndExposesCleaned(t *testing.T) {
	s := newLifecycleTestServer(t, "observe")
	now := time.Now()
	if err := s.DB.Create(&model.HotmethodTask{TID: "t", Name: "t", UID: "u", Status: TaskStatusDone, CreateTime: now.Add(-1 * time.Hour)}).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	live := mkArtifact("t", model.ArtifactKindRaw, "t/perf.data", now.Add(-1*time.Hour), model.ArtifactStatusReady)
	if err := s.DB.Create(&live).Error; err != nil {
		t.Fatalf("create live artifact: %v", err)
	}
	deleting := mkArtifact("t", model.ArtifactKindIntermediate, "t/folded.txt", now.Add(-time.Hour), model.ArtifactStatusDeleting)
	if err := s.DB.Create(&deleting).Error; err != nil {
		t.Fatalf("create deleting artifact: %v", err)
	}
	deletedAt := now.Add(-10 * time.Minute)
	tomb := mkArtifact("t", model.ArtifactKindResult, "t/flamegraph.svg", now.Add(-1*time.Hour), model.ArtifactStatusDeleted)
	tomb.DeletedAt = &deletedAt
	tomb.DeleteReason = model.DeleteReasonExpired
	tomb.Size = 4096
	if err := s.DB.Create(&tomb).Error; err != nil {
		t.Fatalf("create tombstone: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/tasks/:tid/artifacts", s.ListTaskArtifacts)
	router.GET("/api/v1/tasks/:tid/artifacts/:artifact_id/download", s.DownloadTaskArtifact)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/t/artifacts", nil)
	req.Header.Set("Drop-User-Uid", "u")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var resp struct {
		Data struct {
			Artifacts []map[string]interface{} `json:"artifacts"`
			Cleaned   []map[string]interface{} `json:"cleaned_artifacts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data.Artifacts) != 1 || resp.Data.Artifacts[0]["id"].(float64) != float64(live.ID) {
		t.Fatalf("live artifacts=%v", resp.Data.Artifacts)
	}
	if len(resp.Data.Cleaned) != 1 {
		t.Fatalf("cleaned artifacts=%v", resp.Data.Cleaned)
	}
	cleaned := resp.Data.Cleaned[0]
	if cleaned["delete_reason"] != model.DeleteReasonExpired || cleaned["status"] != model.ArtifactStatusDeleted {
		t.Fatalf("cleaned tombstone fields wrong: %v", cleaned)
	}

	// 下载 deleted tombstone → 404（status != ready）
	req = httptest.NewRequest(http.MethodGet, "/api/v1/tasks/t/artifacts/"+strconv.Itoa(int(tomb.ID))+"/download", nil)
	req.Header.Set("Drop-User-Uid", "u")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("download tombstone status=%d want 404", w.Code)
	}
}
