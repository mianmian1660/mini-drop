package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mini-drop/apiserver/config"
	"github.com/mini-drop/apiserver/model"
)

func nowForTest() time.Time {
	return time.Now().Add(-time.Minute)
}

func TestParseV2AgentKeys(t *testing.T) {
	tid, aid, base, ok := parseV2AgentRawKey("tasks/t1/attempts/7/raw/perf.data")
	if !ok || tid != "t1" || aid != 7 || base != "perf.data" {
		t.Fatalf("parse v2 raw: %q %d %q ok=%v", tid, aid, base, ok)
	}
	_, _, _, ok = parseV2AgentRawKey("tasks/t1/attempts/7/raw/../evil")
	if ok {
		t.Fatal("must reject traversal basename")
	}
	_, _, _, ok = parseV2AgentRawKey("t1/perf.data")
	if ok {
		t.Fatal("legacy key must not parse as v2")
	}
	_, _, _, ok = parseV2AgentRawKey("tasks/t1/attempts/0/raw/perf.data")
	if ok {
		t.Fatal("attempt 0 must be rejected")
	}
	mtid, maid, mok := parseV2AgentManifestKey("tasks/t1/attempts/7/manifest.json")
	if !mok || mtid != "t1" || maid != 7 {
		t.Fatalf("parse v2 manifest: %q %d ok=%v", mtid, maid, mok)
	}
}

func TestNotifyRejectsV2WhenDisabledAndMismatchedKeys(t *testing.T) {
	s := newTestAPIServer(t)
	s.Config = &config.Config{
		Storage:    config.StorageConfig{Bucket: "drop-data", PresignExpireSec: 900},
		SingleShot: config.SingleShotConfig{LayoutV2Enabled: false},
	}
	task := mustCreateRunningTask(t, s, "t-v2off")
	_ = task
	// v2 路径但开关关闭 → 400
	w := postNotify(t, s, `{"task_id":"t-v2off","cos_key":"tasks/t-v2off/attempts/1/raw/perf.data"}`)
	if w.Code != 400 {
		t.Fatalf("v2 path with flag off must be rejected, got %d", w.Code)
	}

	s.Config.SingleShot.LayoutV2Enabled = true
	// tid 不匹配 → 400
	w = postNotify(t, s, `{"task_id":"t-v2off","cos_key":"tasks/other/attempts/1/raw/perf.data"}`)
	if w.Code != 400 {
		t.Fatalf("mismatched tid must be rejected, got %d", w.Code)
	}
	// 非法 basename → 400
	w = postNotify(t, s, `{"task_id":"t-v2off","cos_key":"tasks/t-v2off/attempts/1/raw/evil.txt"}`)
	if w.Code != 400 {
		t.Fatalf("unsupported basename must be rejected, got %d", w.Code)
	}
	// attempt 不匹配 → 400
	w = postNotify(t, s, `{"task_id":"t-v2off","cos_key":"tasks/t-v2off/attempts/2/raw/perf.data","attempt_id":3}`)
	if w.Code != 400 {
		t.Fatalf("mismatched attempt must be rejected, got %d", w.Code)
	}
	// v2 manifest 与 RAW 的 attempt 不一致 → 400
	w = postNotify(t, s, `{"task_id":"t-v2off","cos_key":"tasks/t-v2off/attempts/2/raw/perf.data","manifest_key":"tasks/t-v2off/attempts/9/manifest.json"}`)
	if w.Code != 400 {
		t.Fatalf("mismatched manifest attempt must be rejected, got %d", w.Code)
	}
	// 路径内部自洽但数据库 attempt 不存在 → 400，禁止回退完成最新 attempt。
	w = postNotify(t, s, `{"task_id":"t-v2off","cos_key":"tasks/t-v2off/attempts/999/raw/perf.data"}`)
	if w.Code != 400 {
		t.Fatalf("unknown v2 attempt must be rejected, got %d", w.Code)
	}
}

func TestEnsureAnalysisQueuedIdempotentPerAttemptMultiGen(t *testing.T) {
	s := newTestAPIServer(t)
	s.Config = &config.Config{
		Storage:    config.StorageConfig{Bucket: "drop-data", PresignExpireSec: 900},
		SingleShot: config.SingleShotConfig{LayoutV2Enabled: true, GenerationsEnabled: true},
	}
	task := mustCreateRunningTask(t, s, "t-multigen")
	attempt, err := s.startTaskAttempt(&task, model.AttemptTriggerInitial)
	if err != nil {
		t.Fatalf("start attempt: %v", err)
	}
	// 第一次通知：v2 路径，attempt 匹配
	body, _ := json.Marshal(map[string]interface{}{
		"task_id":         "t-multigen",
		"cos_key":         fmt.Sprintf("tasks/t-multigen/attempts/%d/raw/perf.data", attempt.ID),
		"attempt_id":      attempt.ID,
		"artifact_size":   2048,
		"artifact_sha256": "abc",
	})
	w := postNotify(t, s, string(body))
	if w.Code != 200 {
		t.Fatalf("first notify failed: %d %s", w.Code, w.Body.String())
	}
	// 重复通知：幂等，仍只有一条 job
	_ = attempt
	w = postNotify(t, s, string(body))
	if w.Code != 200 {
		t.Fatalf("duplicate notify failed: %d %s", w.Code, w.Body.String())
	}
	var jobs []model.AnalysisJob
	if err := s.DB.Where("task_tid = ?", "t-multigen").Find(&jobs).Error; err != nil {
		t.Fatalf("query jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("want 1 job after duplicate notify, got %d", len(jobs))
	}
	if jobs[0].Generation != 1 || jobs[0].AttemptID != attempt.ID || jobs[0].Trigger != model.AnalysisJobTriggerInitial {
		t.Fatalf("job fields wrong: gen=%d attempt=%d trigger=%s", jobs[0].Generation, jobs[0].AttemptID, jobs[0].Trigger)
	}
	if jobs[0].Pipeline != "perf_flamegraph" {
		t.Fatalf("pipeline=%s", jobs[0].Pipeline)
	}
	var raws []model.Artifact
	if err := s.DB.Where("task_tid = ? AND kind = ?", "t-multigen", model.ArtifactKindRaw).Find(&raws).Error; err != nil {
		t.Fatalf("query raws: %v", err)
	}
	if len(raws) != 1 || raws[0].AttemptID != attempt.ID || raws[0].LogicalName != "perf.data" {
		t.Fatalf("raw artifact fields wrong: %+v", raws)
	}

	// 新 attempt（attempt_id=2）：生成 gen 2
	attempt2, err := s.startTaskAttempt(&task, model.AttemptTriggerRetry)
	if err != nil {
		t.Fatalf("start second attempt: %v", err)
	}
	body2, _ := json.Marshal(map[string]interface{}{
		"task_id":         "t-multigen",
		"cos_key":         fmt.Sprintf("tasks/t-multigen/attempts/%d/raw/perf.data", attempt2.ID),
		"attempt_id":      attempt2.ID,
		"artifact_size":   4096,
		"artifact_sha256": "def",
	})
	w = postNotify(t, s, string(body2))
	if w.Code != 200 {
		t.Fatalf("second attempt notify failed: %d %s", w.Code, w.Body.String())
	}
	jobs = nil
	if err := s.DB.Where("task_tid = ?", "t-multigen").Order("generation ASC").Find(&jobs).Error; err != nil {
		t.Fatalf("query jobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("want 2 jobs, got %d", len(jobs))
	}
	if jobs[1].Generation != 2 || jobs[1].AttemptID != attempt2.ID {
		t.Fatalf("second job fields wrong: gen=%d attempt=%d", jobs[1].Generation, jobs[1].AttemptID)
	}
}

func TestSelectedGenerationFilesAndLegacyKeyAuthorization(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = fakeStorage{}
	s.Config = &config.Config{Storage: config.StorageConfig{Bucket: "drop-data"}}
	taskA := model.HotmethodTask{TID: "task-a", Name: "a", Status: TaskStatusDone, UID: "u", CreateTime: nowForTest()}
	taskB := model.HotmethodTask{TID: "task-b", Name: "b", Status: TaskStatusDone, UID: "u", CreateTime: nowForTest()}
	if err := s.DB.Create(&taskA).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Create(&taskB).Error; err != nil {
		t.Fatal(err)
	}
	job1 := model.AnalysisJob{TaskTID: taskA.TID, Generation: 1, Pipeline: "perf", Trigger: model.AnalysisJobTriggerInitial, Status: model.AnalysisJobStatusSuccess, CreatedAt: nowForTest(), UpdatedAt: nowForTest()}
	job2 := model.AnalysisJob{TaskTID: taskA.TID, Generation: 2, Pipeline: "perf", Trigger: model.AnalysisJobTriggerManual, Status: model.AnalysisJobStatusSuccess, CreatedAt: nowForTest(), UpdatedAt: nowForTest()}
	if err := s.DB.Create(&job1).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Create(&job2).Error; err != nil {
		t.Fatal(err)
	}
	for _, a := range []model.Artifact{
		{TaskTID: taskA.TID, AnalysisJobID: &job1.ID, Kind: model.ArtifactKindResult, ObjectKey: "tasks/task-a/analysis/perf/v/g1/flamegraph.svg", LogicalName: "flamegraph.svg", Status: model.ArtifactStatusReady, CreatedAt: nowForTest()},
		{TaskTID: taskA.TID, AnalysisJobID: &job2.ID, Kind: model.ArtifactKindResult, ObjectKey: "tasks/task-a/analysis/perf/v/g2/flamegraph.svg", LogicalName: "flamegraph.svg", Status: model.ArtifactStatusReady, CreatedAt: nowForTest()},
		{TaskTID: taskB.TID, Kind: model.ArtifactKindRaw, ObjectKey: "tasks/task-b/attempts/1/raw/perf.data", Status: model.ArtifactStatusReady, CreatedAt: nowForTest()},
	} {
		if err := s.DB.Create(&a).Error; err != nil {
			t.Fatal(err)
		}
	}
	files, err := s.listTaskFilesForAnalysisJob(taskA.TID, &job2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || !strings.Contains(files[0]["name"].(string), "/g2/") {
		t.Fatalf("selected generation files=%#v", files)
	}
	if !strings.Contains(files[0]["view_url"].(string), fmt.Sprintf("/artifacts/%d/content", files[0]["artifact_id"])) {
		t.Fatalf("selected file must use artifact content URL: %#v", files[0])
	}
	foreignKey := "tasks/task-b/attempts/1/raw/perf.data"
	if s.cosKeyBelongsToTask(taskA.TID, foreignKey) {
		t.Fatal("task A must not authorize task B object key")
	}
	if !s.cosKeyBelongsToTask(taskB.TID, foreignKey) {
		t.Fatal("artifact owner task must authorize its object key")
	}
}

func TestFailedGenerationRawDoesNotCountAsAvailableResult(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = fakeStorage{}
	s.Config = &config.Config{Storage: config.StorageConfig{Bucket: "drop-data"}}
	task := model.HotmethodTask{TID: "t-no-result", Name: "x", Status: TaskStatusDone, UID: "u", CreateTime: nowForTest()}
	if err := s.DB.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	job := model.AnalysisJob{TaskTID: task.TID, Generation: 2, Pipeline: "perf", Trigger: model.AnalysisJobTriggerManual, Status: model.AnalysisJobStatusFailed, CreatedAt: nowForTest(), UpdatedAt: nowForTest()}
	if err := s.DB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	raw := model.Artifact{TaskTID: task.TID, Kind: model.ArtifactKindRaw, ObjectKey: "t-no-result/perf.data", Status: model.ArtifactStatusReady, CreatedAt: nowForTest()}
	if err := s.DB.Create(&raw).Error; err != nil {
		t.Fatal(err)
	}
	payload := s.analysisJobsPayload(task.TID)
	jobs := payload["analysis_jobs"].([]gin.H)
	if len(jobs) != 1 || jobs[0]["artifacts_available"].(bool) {
		t.Fatalf("failed generation must not inherit RAW availability: %#v", jobs)
	}
	detail, serr := s.taskDetailPayload(task, fmt.Sprint(job.ID))
	if serr != nil {
		t.Fatalf("task detail: %v", serr)
	}
	if files := detail["files"].([]map[string]interface{}); len(files) != 0 {
		t.Fatalf("failed selected generation must not fall back to task RAW/older results: %#v", files)
	}
}

func postReanalyze(t *testing.T, s *APIServer, tid string, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/tasks/:tid/reanalyze", s.ReanalyzeTask)
	req := httptest.NewRequest("POST", "/api/v1/tasks/"+tid+"/reanalyze", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestReanalyzeRejectsInflightAndNonOwner(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = fakeStorage{}
	s.Config = &config.Config{
		Storage:    config.StorageConfig{Bucket: "drop-data", PresignExpireSec: 900},
		SingleShot: config.SingleShotConfig{GenerationsEnabled: true, ReanalyzeEnabled: true},
	}
	// 终态任务 + 已有 pending job → 409
	task := model.HotmethodTask{TID: "t-re", Name: "re", Status: TaskStatusDone, UID: "u1", CreateTime: nowForTest()}
	if err := s.DB.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	job := model.AnalysisJob{
		TaskTID: "t-re", Pipeline: "perf_flamegraph", Status: model.AnalysisJobStatusPending,
		Generation: 1, Trigger: model.AnalysisJobTriggerInitial, CreatedAt: nowForTest(), UpdatedAt: nowForTest(),
	}
	if err := s.DB.Create(&job).Error; err != nil {
		t.Fatalf("create job: %v", err)
	}
	ownerHeaders := map[string]string{"Drop-User-Uid": "u1", "Drop-User-Name": "tester", "Drop-User-Role": "operator"}
	w := postReanalyze(t, s, "t-re", `{}`, ownerHeaders)
	if w.Code != 409 {
		t.Fatalf("inflight job must be 409, got %d body=%s", w.Code, w.Body.String())
	}

	// 非 owner → 403
	otherHeaders := map[string]string{"Drop-User-Uid": "u2", "Drop-User-Name": "other", "Drop-User-Role": "operator"}
	w = postReanalyze(t, s, "t-re", `{}`, otherHeaders)
	if w.Code != 403 {
		t.Fatalf("non-owner must be 403, got %d", w.Code)
	}

	// 功能开关关闭 → 404
	s.Config.SingleShot.ReanalyzeEnabled = false
	w = postReanalyze(t, s, "t-re", `{}`, ownerHeaders)
	if w.Code != 404 {
		t.Fatalf("disabled reanalyze must be 404, got %d", w.Code)
	}
	s.Config.SingleShot.ReanalyzeEnabled = true

	// 非终态任务 → 409（任务还 pending）
	task2 := model.HotmethodTask{TID: "t-nonterminal", Name: "nt", Status: TaskStatusCreated, UID: "u1", CreateTime: nowForTest()}
	if err := s.DB.Create(&task2).Error; err != nil {
		t.Fatalf("create task2: %v", err)
	}
	w = postReanalyze(t, s, "t-nonterminal", `{}`, ownerHeaders)
	if w.Code != 409 {
		t.Fatalf("non-terminal must be 409, got %d", w.Code)
	}

	// 任务不存在 → 404
	w = postReanalyze(t, s, "t-missing", `{}`, ownerHeaders)
	if w.Code != 404 {
		t.Fatalf("missing task must be 404, got %d", w.Code)
	}

	// viewer → 403
	viewerHeaders := map[string]string{"Drop-User-Uid": "u1", "Drop-User-Name": "viewer", "Drop-User-Role": "viewer"}
	w = postReanalyze(t, s, "t-re", `{}`, viewerHeaders)
	if w.Code != 403 {
		t.Fatalf("viewer must be 403, got %d", w.Code)
	}
}

func TestReanalyzeSuccessCreatesGeneration(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = fakeStorage{}
	s.Config = &config.Config{
		Storage:    config.StorageConfig{Bucket: "drop-data", PresignExpireSec: 900},
		SingleShot: config.SingleShotConfig{GenerationsEnabled: true, ReanalyzeEnabled: true},
	}
	task := model.HotmethodTask{TID: "t-ok", Name: "ok", Status: TaskStatusDone, UID: "u1", CreateTime: nowForTest()}
	if err := s.DB.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	attempt := model.TaskAttempt{TaskTID: "t-ok", AttemptSeq: 1, Trigger: model.AttemptTriggerInitial, CreatedAt: nowForTest()}
	if err := s.DB.Create(&attempt).Error; err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	raw := model.Artifact{
		TaskTID: "t-ok", AttemptID: attempt.ID, Kind: model.ArtifactKindRaw,
		ObjectKey: "tasks/t-ok/attempts/1/raw/perf.data", LogicalName: "perf.data",
		ContentType: "application/octet-stream", Status: model.ArtifactStatusReady, CreatedAt: nowForTest(),
	}
	if err := s.DB.Create(&raw).Error; err != nil {
		t.Fatalf("create raw: %v", err)
	}
	ownerHeaders := map[string]string{"Drop-User-Uid": "u1", "Drop-User-Name": "tester", "Drop-User-Role": "operator"}
	w := postReanalyze(t, s, "t-ok", `{}`, ownerHeaders)
	if w.Code != 202 {
		t.Fatalf("reanalyze must be 202, got %d body=%s", w.Code, w.Body.String())
	}
	var created model.AnalysisJob
	if err := s.DB.Where("task_tid = ? AND trigger = ?", "t-ok", model.AnalysisJobTriggerManual).First(&created).Error; err != nil {
		t.Fatalf("query manual job: %v", err)
	}
	if created.Generation != 1 || created.AttemptID != attempt.ID || created.RequestedBy != "tester" {
		t.Fatalf("manual job fields wrong: gen=%d attempt=%d by=%q", created.Generation, created.AttemptID, created.RequestedBy)
	}
	// 已有 pending/running/retry → 再次重分析 409
	w = postReanalyze(t, s, "t-ok", `{}`, ownerHeaders)
	if w.Code != 409 {
		t.Fatalf("second reanalyze with inflight must be 409, got %d", w.Code)
	}
	// 结束进行中的作业后再验证 attempt 校验
	if err := s.DB.Model(&model.AnalysisJob{}).Where("id = ?", created.ID).
		Update("status", model.AnalysisJobStatusSuccess).Error; err != nil {
		t.Fatalf("mark job success: %v", err)
	}
	// 指定错误 attempt → 400
	w = postReanalyze(t, s, "t-ok", `{"attempt_id":999}`, ownerHeaders)
	if w.Code != 400 {
		t.Fatalf("bad attempt must be 400, got %d", w.Code)
	}
	// 再次成功重分析 → generation 递增
	w = postReanalyze(t, s, "t-ok", `{}`, ownerHeaders)
	if w.Code != 202 {
		t.Fatalf("reanalyze after success must be 202, got %d body=%s", w.Code, w.Body.String())
	}
	var latest model.AnalysisJob
	if err := s.DB.Where("task_tid = ? AND trigger = ?", "t-ok", model.AnalysisJobTriggerManual).Order("generation DESC").First(&latest).Error; err != nil {
		t.Fatalf("query latest manual job: %v", err)
	}
	if latest.Generation != 2 {
		t.Fatalf("generation must increment to 2, got %d", latest.Generation)
	}
}
