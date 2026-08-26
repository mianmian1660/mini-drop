package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/mini-drop/apiserver/config"
	"github.com/mini-drop/apiserver/model"
	"github.com/mini-drop/apiserver/pkg/storage"
	pb_control "github.com/mini-drop/apiserver/proto/control"
	"github.com/mini-drop/apiserver/util"
)

func newTestAPIServer(t *testing.T) *APIServer {
	t.Helper()
	dbName := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	db, err := gorm.Open(sqlite.Open("file:"+dbName+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := model.AutoMigrate(db); err != nil {
		t.Fatalf("migrate sqlite: %v", err)
	}
	return &APIServer{
		DB:       db,
		Logger:   zap.NewNop(),
		Config:   &config.Config{Storage: config.StorageConfig{Bucket: "drop-data", PresignExpireSec: 900}},
		Cron:     cron.New(),
		CronJobs: map[string]cron.EntryID{},
	}
}

type fakeStorage struct{}

func (fakeStorage) EnsureBucket(context.Context, string) error { return nil }
func (fakeStorage) PutObject(context.Context, string, string, io.Reader, int64, string) error {
	return nil
}
func (fakeStorage) GetObject(context.Context, string, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("ok")), nil
}
func (fakeStorage) GetObjectRange(context.Context, string, string, int64, int64) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("ok")), nil
}
func (fakeStorage) PresignedGetURL(context.Context, string, string, time.Duration) (string, error) {
	return "http://example.test/signed", nil
}
func (fakeStorage) ListObjects(context.Context, string, string) ([]storage.FileInfo, error) {
	return []storage.FileInfo{}, nil
}
func (fakeStorage) DeleteObject(context.Context, string, string) error { return nil }
func (fakeStorage) ObjectExists(context.Context, string, string) (bool, error) {
	return true, nil
}
func (fakeStorage) StatObject(context.Context, string, string) (int64, error) {
	return 0, nil
}

type fakeControlClient struct {
	cancelReq *pb_control.CancelTaskRequest
}

func (f *fakeControlClient) CreateTask(context.Context, *pb_control.CreateTaskRequest, ...grpc.CallOption) (*pb_control.CreateTaskResponse, error) {
	return &pb_control.CreateTaskResponse{Code: 0, Msg: "ok"}, nil
}

func (f *fakeControlClient) FetchData(context.Context, *pb_control.FetchDataRequest, ...grpc.CallOption) (*pb_control.FetchDataResponse, error) {
	return &pb_control.FetchDataResponse{Code: 0}, nil
}

func (f *fakeControlClient) StatAgent(context.Context, *pb_control.StatAgentRequest, ...grpc.CallOption) (*pb_control.StatAgentResponse, error) {
	return &pb_control.StatAgentResponse{Code: 404, Msg: "not found"}, nil
}

func (f *fakeControlClient) CancelTask(_ context.Context, req *pb_control.CancelTaskRequest, _ ...grpc.CallOption) (*pb_control.CancelTaskResponse, error) {
	f.cancelReq = req
	return &pb_control.CancelTaskResponse{Code: 0, Msg: "queued task canceled", Canceled: true}, nil
}

func postNotify(t *testing.T, s *APIServer, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/internal/task-notify", s.NotifyTaskResult)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/task-notify", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestAgentMetadataFromStatIncludesStage3Fields(t *testing.T) {
	resp := &pb_control.StatAgentResponse{
		Code:           0,
		AgentId:        "agent-stable-1",
		Hostname:       "demo-host",
		Version:        "1.2.3",
		Platform:       "linux/amd64",
		Capabilities:   []string{"perf_cpu", "go_pprof"},
		Labels:         []string{"local"},
		ResourceBudget: `{"cpu":1}`,
		Online:         true,
		LastSeenUnixMs: time.Date(2026, 8, 5, 1, 2, 3, 0, time.UTC).UnixMilli(),
	}
	updates := agentMetadataFromStat(resp)
	if updates["agent_id"] != "agent-stable-1" || updates["hostname"] != "demo-host" || updates["version"] != "1.2.3" || updates["supported_os"] != "linux/amd64" || updates["status"] != "online" {
		t.Fatalf("metadata updates missing identity fields: %#v", updates)
	}
	if !strings.Contains(string(updates["capabilities"].([]byte)), "perf_cpu") || !strings.Contains(string(updates["labels"].([]byte)), "local") || string(updates["resource_budget"].([]byte)) != `{"cpu":1}` {
		t.Fatalf("metadata updates missing json fields: %#v", updates)
	}
	if !updates["last_seen"].(time.Time).Equal(time.Date(2026, 8, 5, 1, 2, 3, 0, time.UTC)) {
		t.Fatalf("last_seen=%v, want stat timestamp", updates["last_seen"])
	}
}

func mustCreateRunningTask(t *testing.T, s *APIServer, tid string) model.HotmethodTask {
	t.Helper()
	now := time.Now().Add(-time.Minute)
	params, err := util.MarshalJSONB(PerfParams{Duration: 2, Frequency: 49, Event: "cpu-cycles"})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	task := model.HotmethodTask{
		TID:            tid,
		Name:           "notify test",
		Type:           TaskTypeGeneric,
		ProfilerType:   ProfilerPerf,
		TargetIP:       "127.0.0.1",
		RequestParams:  params,
		Status:         TaskStatusRunning,
		StatusInfo:     "running",
		AnalysisStatus: 0,
		UID:            "u1",
		UserName:       "tester",
		CreateTime:     now,
		BeginTime:      &now,
	}
	if err := s.DB.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := s.startTaskAttempt(&task, model.AttemptTriggerInitial); err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	return task
}

func TestPickRawCollectionObjectPrefersPerfData(t *testing.T) {
	key, size, ok := pickRawCollectionObject([]storage.FileInfo{
		{Name: "tid-1/flamegraph.svg", Size: 100},
		{Name: "tid-1/top.json", Size: 20},
		{Name: "tid-1/perf.data", Size: 2048},
	})

	if !ok {
		t.Fatal("expected raw collection artifact")
	}
	if key != "tid-1/perf.data" || size != 2048 {
		t.Fatalf("key=%q size=%d, want tid-1/perf.data 2048", key, size)
	}
}

func TestPickRawCollectionObjectPrefersTypedRawArtifacts(t *testing.T) {
	key, size, ok := pickRawCollectionObject([]storage.FileInfo{
		{Name: "tid-1/perf.data", Size: 2048},
		{Name: "tid-1/bpf_histogram.svg", Size: 100},
		{Name: "tid-1/bpf_data.json", Size: 20},
		{Name: "tid-1/raw.bpf", Size: 512},
	})

	if !ok {
		t.Fatal("expected typed raw collection artifact")
	}
	if key != "tid-1/raw.bpf" || size != 512 {
		t.Fatalf("key=%q size=%d, want tid-1/raw.bpf 512", key, size)
	}
}

func TestPickRawCollectionObjectSkipsResultArtifacts(t *testing.T) {
	_, _, ok := pickRawCollectionObject([]storage.FileInfo{
		{Name: "tid-1/flamegraph.svg", Size: 100},
		{Name: "tid-1/top.json", Size: 20},
		{Name: "tid-1/suggestions.json", Size: 30},
	})

	if ok {
		t.Fatal("result artifacts must not be treated as raw analyzer input")
	}
}

func TestPickRawCollectionLocalFile(t *testing.T) {
	key, size, ok := pickRawCollectionLocalFile([]map[string]interface{}{
		{"name": "tid-1_flamegraph.svg", "size": int64(100)},
		{"name": "tid-1_perf.data", "size": int64(4096)},
	})

	if !ok {
		t.Fatal("expected local raw collection artifact")
	}
	if key != "tid-1_perf.data" || size != 4096 {
		t.Fatalf("key=%q size=%d, want tid-1_perf.data 4096", key, size)
	}
}

func TestNotifyTaskResultRejectsMissingTaskID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &APIServer{}
	router := gin.New()
	router.POST("/api/v1/internal/task-notify", s.NotifyTaskResult)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/task-notify", strings.NewReader(`{"cos_key":"tid-1/perf.data"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400, body=%s", w.Code, w.Body.String())
	}
}

func TestNotifyTaskResultRunningRecordsUploadingBeforeDone(t *testing.T) {
	s := newTestAPIServer(t)
	mustCreateRunningTask(t, s, "tid-notify-success")

	w := postNotify(t, s, `{"task_id":"tid-notify-success","cos_key":"tid-notify-success/perf.data"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200, body=%s", w.Code, w.Body.String())
	}

	var task model.HotmethodTask
	if err := s.DB.Where("tid = ?", "tid-notify-success").First(&task).Error; err != nil {
		t.Fatalf("load task: %v", err)
	}
	if task.Status != TaskStatusDone || task.EndTime == nil {
		t.Fatalf("task status=%d end=%v, want done with end_time", task.Status, task.EndTime)
	}

	var events []model.TaskStatusEvent
	if err := s.DB.Where("tid = ?", task.TID).Order("created_at ASC, id ASC").Find(&events).Error; err != nil {
		t.Fatalf("load events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events=%d, want 2: %#v", len(events), events)
	}
	if events[0].FromStatus != TaskStatusRunning || events[0].ToStatus != TaskStatusUploading {
		t.Fatalf("first event=%d->%d, want running->uploading", events[0].FromStatus, events[0].ToStatus)
	}
	if events[1].FromStatus != TaskStatusUploading || events[1].ToStatus != TaskStatusDone {
		t.Fatalf("second event=%d->%d, want uploading->done", events[1].FromStatus, events[1].ToStatus)
	}

	var artifact model.Artifact
	if err := s.DB.Where("task_tid = ? AND kind = ?", task.TID, model.ArtifactKindRaw).First(&artifact).Error; err != nil {
		t.Fatalf("raw artifact not recorded: %v", err)
	}
	if artifact.ObjectKey != "tid-notify-success/perf.data" || artifact.Status != model.ArtifactStatusReady {
		t.Fatalf("artifact=%#v, want ready raw perf.data", artifact)
	}

	var job model.AnalysisJob
	if err := s.DB.Where("task_tid = ?", task.TID).First(&job).Error; err != nil {
		t.Fatalf("analysis job not queued: %v", err)
	}
	if job.Status != model.AnalysisJobStatusPending || job.Pipeline != "perf_flamegraph" {
		t.Fatalf("job status=%q pipeline=%q, want pending perf_flamegraph", job.Status, job.Pipeline)
	}

	var attempt model.TaskAttempt
	if err := s.DB.Where("task_tid = ?", task.TID).First(&attempt).Error; err != nil {
		t.Fatalf("attempt not recorded: %v", err)
	}
	if attempt.EndTime == nil || attempt.ExitCode != 0 || !strings.Contains(string(attempt.ArtifactKeys), "perf.data") {
		t.Fatalf("attempt evidence not finished correctly: %#v", attempt)
	}
	if artifact.AttemptID != attempt.ID {
		t.Fatalf("artifact attempt_id=%d, want %d", artifact.AttemptID, attempt.ID)
	}
}

func TestNotifyTaskResultSuccessIsIdempotentAfterDone(t *testing.T) {
	s := newTestAPIServer(t)
	mustCreateRunningTask(t, s, "tid-notify-idem")

	for i := 0; i < 2; i++ {
		w := postNotify(t, s, `{"task_id":"tid-notify-idem","cos_key":"tid-notify-idem/perf.data"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("notify %d status=%d, body=%s", i+1, w.Code, w.Body.String())
		}
	}

	var eventCount int64
	s.DB.Model(&model.TaskStatusEvent{}).Where("tid = ?", "tid-notify-idem").Count(&eventCount)
	if eventCount != 2 {
		t.Fatalf("event count=%d, want 2", eventCount)
	}
	var artifactCount int64
	s.DB.Model(&model.Artifact{}).Where("task_tid = ?", "tid-notify-idem").Count(&artifactCount)
	if artifactCount != 1 {
		t.Fatalf("artifact count=%d, want 1", artifactCount)
	}
	var jobCount int64
	s.DB.Model(&model.AnalysisJob{}).Where("task_tid = ?", "tid-notify-idem").Count(&jobCount)
	if jobCount != 1 {
		t.Fatalf("job count=%d, want 1", jobCount)
	}
}

func TestNotifyTaskResultPersistsAttemptArtifactMetadataIdempotently(t *testing.T) {
	s := newTestAPIServer(t)
	task := mustCreateRunningTask(t, s, "tid-notify-meta")
	var attempt model.TaskAttempt
	if err := s.DB.Where("task_tid = ?", task.TID).First(&attempt).Error; err != nil {
		t.Fatalf("load attempt: %v", err)
	}

	body := `{"task_id":"tid-notify-meta","cos_key":"tid-notify-meta/perf.data","attempt_id":` + strconv.Itoa(int(attempt.ID)) + `,"artifact_size":4096,"artifact_sha256":"abc123","manifest_key":"tid-notify-meta/manifest.json"}`
	for i := 0; i < 2; i++ {
		w := postNotify(t, s, body)
		if w.Code != http.StatusOK {
			t.Fatalf("notify %d status=%d body=%s", i+1, w.Code, w.Body.String())
		}
	}

	var artifact model.Artifact
	if err := s.DB.Where("task_tid = ? AND object_key = ?", task.TID, task.TID+"/perf.data").First(&artifact).Error; err != nil {
		t.Fatalf("load artifact: %v", err)
	}
	if artifact.AttemptID != attempt.ID || artifact.Size != 4096 || artifact.SHA256 != "abc123" || artifact.Hash != "sha256:abc123" || artifact.ManifestKey != task.TID+"/manifest.json" {
		t.Fatalf("artifact metadata=%#v, want attempt/size/sha256/manifest", artifact)
	}
	var artifactCount int64
	s.DB.Model(&model.Artifact{}).Where("task_tid = ?", task.TID).Count(&artifactCount)
	if artifactCount != 2 {
		t.Fatalf("artifact count=%d, want 2 after replay (profile and manifest)", artifactCount)
	}
	var manifest model.Artifact
	if err := s.DB.Where("task_tid = ? AND object_key = ?", task.TID, task.TID+"/manifest.json").First(&manifest).Error; err != nil {
		t.Fatalf("load manifest artifact: %v", err)
	}
	if manifest.Kind != model.ArtifactKindManifest || manifest.AttemptID != attempt.ID {
		t.Fatalf("manifest artifact=%#v, want MANIFEST for attempt %d", manifest, attempt.ID)
	}
	var jobCount int64
	s.DB.Model(&model.AnalysisJob{}).Where("task_tid = ?", task.TID).Count(&jobCount)
	if jobCount != 1 {
		t.Fatalf("job count=%d, want 1 after replay", jobCount)
	}
	if err := s.DB.Where("id = ?", attempt.ID).First(&attempt).Error; err != nil {
		t.Fatalf("reload attempt: %v", err)
	}
	if attempt.EndTime == nil || attempt.ExitCode != 0 || !strings.Contains(string(attempt.ArtifactKeys), "perf.data") {
		t.Fatalf("attempt evidence=%#v, want completed with artifact key", attempt)
	}
}

func TestNotifyTaskResultRawBPFQueuesBPFAnalysis(t *testing.T) {
	s := newTestAPIServer(t)
	mustCreateRunningTask(t, s, "tid-notify-bpf")

	w := postNotify(t, s, `{"task_id":"tid-notify-bpf","cos_key":"tid-notify-bpf/raw.bpf","artifact_size":128}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200, body=%s", w.Code, w.Body.String())
	}

	var artifact model.Artifact
	if err := s.DB.Where("task_tid = ? AND kind = ?", "tid-notify-bpf", model.ArtifactKindRaw).First(&artifact).Error; err != nil {
		t.Fatalf("raw artifact not recorded: %v", err)
	}
	if artifact.ObjectKey != "tid-notify-bpf/raw.bpf" || artifact.ContentType != "text/plain; charset=utf-8" {
		t.Fatalf("artifact=%#v, want raw.bpf text artifact", artifact)
	}

	var job model.AnalysisJob
	if err := s.DB.Where("task_tid = ?", "tid-notify-bpf").First(&job).Error; err != nil {
		t.Fatalf("analysis job not queued: %v", err)
	}
	if job.Pipeline != "bpf_histogram" {
		t.Fatalf("pipeline=%q, want bpf_histogram", job.Pipeline)
	}
}

func TestNotifyTaskResultErrorMarksFailedAndAttempt(t *testing.T) {
	s := newTestAPIServer(t)
	mustCreateRunningTask(t, s, "tid-notify-failed")

	w := postNotify(t, s, `{"task_id":"tid-notify-failed","error_message":"perf failed"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200, body=%s", w.Code, w.Body.String())
	}

	var task model.HotmethodTask
	if err := s.DB.Where("tid = ?", "tid-notify-failed").First(&task).Error; err != nil {
		t.Fatalf("load task: %v", err)
	}
	if task.Status != TaskStatusFailed || task.AnalysisStatus != 3 || task.EndTime == nil {
		t.Fatalf("task=%#v, want failed with analysis_status=3 and end_time", task)
	}

	var attempt model.TaskAttempt
	if err := s.DB.Where("task_tid = ?", task.TID).First(&attempt).Error; err != nil {
		t.Fatalf("load attempt: %v", err)
	}
	if attempt.ExitCode != 1 || attempt.ErrorCode != ErrCodeTaskExecutionFailed || !strings.Contains(attempt.ErrorMessage, "perf failed") {
		t.Fatalf("attempt=%#v, want execution failure evidence", attempt)
	}
}

func TestNotifyTaskResultRejectsSuccessWithoutCosKey(t *testing.T) {
	s := newTestAPIServer(t)
	mustCreateRunningTask(t, s, "tid-notify-no-cos")

	w := postNotify(t, s, `{"task_id":"tid-notify-no-cos"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400, body=%s", w.Code, w.Body.String())
	}
	var task model.HotmethodTask
	if err := s.DB.Where("tid = ?", "tid-notify-no-cos").First(&task).Error; err != nil {
		t.Fatalf("load task: %v", err)
	}
	if task.Status != TaskStatusRunning {
		t.Fatalf("status=%d, want running", task.Status)
	}
}

func TestTransitionTaskStatusRecordsDefaultsAndExtra(t *testing.T) {
	s := newTestAPIServer(t)
	task := model.HotmethodTask{TID: "tid-transition", Name: "transition", UID: "u1", Status: TaskStatusCreated, CreateTime: time.Now()}
	if err := s.DB.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	end := time.Now()
	if err := s.transitionTaskStatus(&task, TaskStatusDone, "", "", map[string]interface{}{"end_time": &end, "analysis_status": 2}); err != nil {
		t.Fatalf("transition: %v", err)
	}

	var got model.HotmethodTask
	if err := s.DB.Where("tid = ?", task.TID).First(&got).Error; err != nil {
		t.Fatalf("load task: %v", err)
	}
	if got.Status != TaskStatusDone || got.StatusInfo != "状态迁移" || got.AnalysisStatus != 2 || got.EndTime == nil {
		t.Fatalf("task after transition=%#v", got)
	}
	var event model.TaskStatusEvent
	if err := s.DB.Where("tid = ?", task.TID).First(&event).Error; err != nil {
		t.Fatalf("load event: %v", err)
	}
	if event.FromStatus != TaskStatusCreated || event.ToStatus != TaskStatusDone || event.Source != "apiserver" {
		t.Fatalf("event=%#v, want default source and created->done", event)
	}
}

func TestTaskReadHandlersUseUIDAndReturnEvidence(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	params, _ := util.MarshalJSONB(PerfParams{Duration: 5, Frequency: 99})
	task := model.HotmethodTask{
		TID:            "tid-detail",
		Name:           "detail task",
		Type:           TaskTypeGeneric,
		ProfilerType:   ProfilerPerf,
		TargetIP:       "10.0.0.1",
		RequestParams:  params,
		Status:         TaskStatusDone,
		AnalysisStatus: 2,
		UID:            "owner",
		UserName:       "Owner",
		CreateTime:     now,
	}
	if err := s.DB.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	_ = s.DB.Create(&model.TaskStatusEvent{TID: task.TID, FromStatus: TaskStatusRunning, ToStatus: TaskStatusDone, Reason: "done", CreatedAt: now}).Error
	_ = s.DB.Create(&model.TaskAttempt{TaskTID: task.TID, AttemptSeq: 1, Trigger: model.AttemptTriggerInitial, CreatedAt: now}).Error
	_ = s.DB.Create(&model.Artifact{TaskTID: task.TID, Kind: model.ArtifactKindRaw, ObjectKey: task.TID + "/perf.data", Status: model.ArtifactStatusReady, CreatedAt: now}).Error
	_ = s.DB.Create(&model.AnalysisSuggestion{TID: task.TID, Func: "main.work", Suggestion: "reduce cpu", AISuggestion: "cache hot path", Status: 1}).Error
	_ = s.DB.Create(&model.HotmethodTask{TID: "tid-other", Name: "other", Status: TaskStatusDone, UID: "other", CreateTime: now.Add(time.Second)}).Error

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/tasks", s.ListTasks)
	router.GET("/api/v1/tasks/:tid", s.GetTaskDetail)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks?status=2&keyword=detail&page=1&pageSize=10", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", w.Code, w.Body.String())
	}
	var listResp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	data := listResp["data"].(map[string]interface{})
	if int(data["total"].(float64)) != 1 {
		t.Fatalf("list total=%v, want 1", data["total"])
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/tasks/tid-detail", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", w.Code, w.Body.String())
	}
	var detailResp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &detailResp); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	detailData := detailResp["data"].(map[string]interface{})
	for _, key := range []string{"task", "status_events", "attempts", "artifacts", "suggestions", "files"} {
		if _, ok := detailData[key]; !ok {
			t.Fatalf("detail missing %s in %#v", key, detailData)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/tasks/tid-detail", nil)
	req.Header.Set("Drop-User-Uid", "other")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"can_manage":false`) {
		t.Fatalf("cross-user detail status=%d body=%s, want shared read with can_manage=false", w.Code, w.Body.String())
	}
}

func TestDeleteAndFileHandlers(t *testing.T) {
	s := newTestAPIServer(t)
	task := model.HotmethodTask{TID: "tid-delete", Name: "delete", UID: "owner", CreateTime: time.Now()}
	if err := s.DB.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	fileTask := model.HotmethodTask{TID: "tid-files", Name: "files", UID: "owner", CreateTime: time.Now()}
	if err := s.DB.Create(&fileTask).Error; err != nil {
		t.Fatalf("create file task: %v", err)
	}
	legacyKeyTask := model.HotmethodTask{TID: "tid", Name: "legacy key", UID: "owner", CreateTime: time.Now()}
	if err := s.DB.Create(&legacyKeyTask).Error; err != nil {
		t.Fatalf("create legacy key task: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.DELETE("/api/v1/tasks/:tid", s.DeleteTask)
	router.GET("/api/v1/cosfiles", s.ListCOSFiles)
	router.GET("/api/v1/files/:filename", s.ServeLocalFile)
	router.GET("/api/v1/cosfiles/view", s.ViewCOSFile)
	router.GET("/api/v1/cosfiles/download", s.DownloadCOSFile)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/tasks/tid-delete", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/v1/tasks/tid-delete", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("delete missing status=%d, want 404", w.Code)
	}

	cases := []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/api/v1/cosfiles", http.StatusBadRequest},
		{http.MethodGet, "/api/v1/cosfiles?tid=tid-files", http.StatusOK},
		{http.MethodGet, "/api/v1/files/..secret", http.StatusBadRequest},
		{http.MethodGet, "/api/v1/files/missing.svg", http.StatusNotFound},
		{http.MethodGet, "/api/v1/cosfiles/view", http.StatusBadRequest},
		{http.MethodGet, "/api/v1/cosfiles/view?key=tid/top.json", http.StatusBadRequest},
		{http.MethodGet, "/api/v1/cosfiles/view?key=tid/flamegraph.svg", http.StatusServiceUnavailable},
		{http.MethodGet, "/api/v1/cosfiles/download", http.StatusBadRequest},
		{http.MethodGet, "/api/v1/cosfiles/download?key=tid/top.json", http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		req = httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Drop-User-Uid", "owner")
		w = httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Fatalf("%s status=%d, want %d body=%s", tc.path, w.Code, tc.want, w.Body.String())
		}
	}
}

func TestStage2ResponseCancelArtifactAndRBAC(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = fakeStorage{}
	fakeControl := &fakeControlClient{}
	s.ControlCli = fakeControl
	now := time.Now()
	task := model.HotmethodTask{
		TID: "tid-stage2", Name: "stage2", Status: TaskStatusRunning, UID: "owner", UserName: "Owner",
		TargetIP: "127.0.0.1", CreateTime: now, BeginTime: &now,
	}
	if err := s.DB.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := s.startTaskAttempt(&task, model.AttemptTriggerInitial); err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	var attempt model.TaskAttempt
	if err := s.DB.Where("task_tid = ?", task.TID).First(&attempt).Error; err != nil {
		t.Fatalf("load attempt: %v", err)
	}
	if err := s.DB.Create(&model.Outbox{
		Aggregate: model.OutboxAggregateTask, AggregateID: task.TID, Event: model.OutboxEventDispatchTask,
		Payload: []byte(`{"name":"stage2"}`), Status: model.OutboxStatusPending, CreatedAt: now,
	}).Error; err != nil {
		t.Fatalf("create outbox: %v", err)
	}
	if err := s.DB.Create(&model.Artifact{
		TaskTID: task.TID, Kind: model.ArtifactKindRaw, ObjectKey: task.TID + "/perf.data",
		Size: 123, SHA256: "abc", Status: model.ArtifactStatusReady, CreatedAt: now,
	}).Error; err != nil {
		t.Fatalf("create artifact: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/internal/task-notify", s.NotifyTaskResult)
	router.POST("/api/v1/tasks/:tid/cancel", s.CancelTask)
	router.GET("/api/v1/tasks/:tid/artifacts", s.ListTaskArtifacts)
	router.GET("/api/v1/tasks/:tid/artifacts/:artifact_id/download", s.DownloadTaskArtifact)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/tid-stage2/cancel", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	req.Header.Set("Drop-User-Role", "Viewer")
	req.Header.Set("X-Request-ID", "rid-stage2")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"request_id":"rid-stage2"`) || !strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("viewer cancel response status=%d body=%s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/tasks/tid-stage2/artifacts", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	req.Header.Set("X-Request-ID", "rid-artifacts")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list artifacts status=%d body=%s", w.Code, w.Body.String())
	}
	var artifactResp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &artifactResp); err != nil {
		t.Fatalf("decode artifacts: %v", err)
	}
	if artifactResp["request_id"] != "rid-artifacts" || artifactResp["error"] != nil || artifactResp["code"].(float64) != 0 {
		t.Fatalf("unexpected unified response: %#v", artifactResp)
	}
	artifacts := artifactResp["data"].(map[string]interface{})["artifacts"].([]interface{})
	firstArtifact := artifacts[0].(map[string]interface{})
	if _, ok := firstArtifact["object_key"]; ok {
		t.Fatalf("artifact API must not expose object_key: %#v", firstArtifact)
	}

	artifactID := int(firstArtifact["id"].(float64))
	req = httptest.NewRequest(http.MethodGet, "/api/v1/tasks/tid-stage2/artifacts/"+strconv.Itoa(artifactID)+"/download", nil)
	req.Header.Set("Drop-User-Uid", "other")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("cross user artifact download status=%d body=%s, want shared read", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/tasks/tid-stage2/artifacts/"+strconv.Itoa(artifactID)+"/download", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "/api/v1/tasks/tid-stage2/artifacts/") ||
		!strings.Contains(w.Body.String(), "/content?download=1") {
		t.Fatalf("owner artifact download status=%d body=%s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/tasks/tid-stage2/cancel", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("cancel status=%d body=%s", w.Code, w.Body.String())
	}
	var canceled model.HotmethodTask
	if err := s.DB.Where("tid = ?", task.TID).First(&canceled).Error; err != nil {
		t.Fatalf("load canceled task: %v", err)
	}
	if canceled.Status != TaskStatusCanceled || !canceled.CancelRequested || canceled.CanceledAt == nil {
		t.Fatalf("task after cancel=%#v", canceled)
	}
	if fakeControl.cancelReq == nil || fakeControl.cancelReq.GetTaskID() != task.TID || fakeControl.cancelReq.GetAttemptId() != uint64(attempt.ID) {
		t.Fatalf("cancel rpc req=%#v, want task and latest attempt", fakeControl.cancelReq)
	}
	var canceledAttempt model.TaskAttempt
	if err := s.DB.Where("id = ?", attempt.ID).First(&canceledAttempt).Error; err != nil {
		t.Fatalf("load canceled attempt: %v", err)
	}
	if canceledAttempt.EndTime == nil || canceledAttempt.ErrorCode != ErrCodeTaskCanceled || canceledAttempt.ExitCode != 1 {
		t.Fatalf("canceled attempt=%#v, want terminal TASK_CANCELED", canceledAttempt)
	}

	lateNotifyBody := `{"task_id":"` + task.TID + `","attempt_id":` + strconv.Itoa(int(attempt.ID)) + `,"error_code":"TASK_CANCELED","error_message":"runner observed cancellation"}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/internal/task-notify", strings.NewReader(lateNotifyBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("late cancel notify status=%d body=%s", w.Code, w.Body.String())
	}
	if err := s.DB.Where("tid = ?", task.TID).First(&canceled).Error; err != nil {
		t.Fatalf("reload canceled task after late notify: %v", err)
	}
	if canceled.Status != TaskStatusCanceled {
		t.Fatalf("late cancel notify changed status to %d, want CANCELED", canceled.Status)
	}
	if err := s.DB.Where("id = ?", attempt.ID).First(&canceledAttempt).Error; err != nil {
		t.Fatalf("reload canceled attempt after late notify: %v", err)
	}
	if canceledAttempt.ErrorCode != ErrCodeTaskCanceled || canceledAttempt.ErrorMessage != "runner observed cancellation" {
		t.Fatalf("late cancel attempt=%#v", canceledAttempt)
	}
	var outbox model.Outbox
	if err := s.DB.Where("aggregate_id = ?", task.TID).First(&outbox).Error; err != nil {
		t.Fatalf("load outbox: %v", err)
	}
	if outbox.PublishedAt == nil || !strings.Contains(outbox.LastError, "任务已取消") {
		t.Fatalf("outbox after cancel=%#v, want published skip", outbox)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/tasks/tid-stage2/cancel", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "already_terminal") {
		t.Fatalf("idempotent cancel status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestStage5TaskKindsFilterByAgentCapabilities(t *testing.T) {
	s := newTestAPIServer(t)
	caps, _ := util.MarshalJSONB([]string{"pprof"})
	if err := s.DB.Create(&model.AgentInfo{Hostname: "pprof-agent", IPAddr: "10.0.0.8", Online: true, UID: "owner", Capabilities: caps, LastSeen: time.Now()}).Error; err != nil {
		t.Fatalf("create agent: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/task-kinds", s.ListTaskKinds)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/task-kinds?target_ip=10.0.0.8", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	items := resp["data"].(map[string]interface{})["task_kinds"].([]interface{})
	if len(items) != 2 {
		t.Fatalf("task kinds=%d, want pprof-compatible CPU and heap kinds: %s", len(items), w.Body.String())
	}
	seen := map[string]bool{}
	for _, item := range items {
		seen[item.(map[string]interface{})["id"].(string)] = true
	}
	if !seen[TaskKindGoPprof] || !seen[TaskKindGoPprofHeap] {
		t.Fatalf("task kinds=%v, want %s and %s", seen, TaskKindGoPprof, TaskKindGoPprofHeap)
	}
}

func TestStage5TaskEventSSESnapshotLastEventIDAndPermission(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	task := model.HotmethodTask{
		TID: "tid-sse", Name: "sse", UID: "owner", UserName: "owner",
		Status: TaskStatusDone, AnalysisStatus: 2, RequestID: "task-rid",
		TargetIP: "127.0.0.1", CreateTime: now, EndTime: &now,
	}
	if err := s.DB.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := s.DB.Create(&model.TaskStatusEvent{
		TID: task.TID, FromStatus: TaskStatusUploading, ToStatus: TaskStatusDone,
		Reason: "采集完成", Source: "test", Sequence: 7, CreatedAt: now,
	}).Error; err != nil {
		t.Fatalf("create event: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/tasks/:tid/events/stream", s.StreamTaskEvents)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/tid-sse/events/stream", nil).WithContext(ctx)
	req.Header.Set("Drop-User-Uid", "owner")
	req.Header.Set("Last-Event-ID", "3")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("sse status=%d content-type=%q body=%s", w.Code, w.Header().Get("Content-Type"), body)
	}
	for _, want := range []string{"id: 7", "event: snapshot", `"sequence":7`, `"status_events"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("sse body missing %q: %s", want, body)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/tasks/tid-sse/events/stream", nil)
	req.Header.Set("Drop-User-Uid", "other")
	req.Header.Set("X-Request-ID", "rid-sse-forbid")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"can_manage":false`) {
		t.Fatalf("shared sse status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestStage5SuggestionSSESendsInitialPayloadAndCompletes(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	task := model.HotmethodTask{
		TID: "tid-suggest-sse", Name: "suggest", UID: "owner",
		Status: TaskStatusDone, AnalysisStatus: 2, CreateTime: now, EndTime: &now,
	}
	if err := s.DB.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := s.DB.Create(&model.AnalysisSuggestion{
		TID: task.TID, Func: "hot.work", Suggestion: "检查循环", Status: 0,
	}).Error; err != nil {
		t.Fatalf("create suggestion: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/tasks/:tid/suggestions/stream", s.StreamTaskSuggestions)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/tid-suggest-sse/suggestions/stream", nil).WithContext(ctx)
	req.Header.Set("Drop-User-Uid", "owner")
	req.Header.Set("X-Request-ID", "rid-suggest-sse")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, "event: suggestions") || !strings.Contains(body, "hot.work") || !strings.Contains(body, "event: complete") {
		t.Fatalf("suggestion sse status=%d body=%s", w.Code, body)
	}
}

func TestStage2HealthEndpoints(t *testing.T) {
	s := newTestAPIServer(t)
	s.Storage = fakeStorage{}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/livez", s.Livez)
	router.GET("/readyz", s.Readyz)
	router.GET("/healthz", s.Healthz)

	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"ok"`) {
		t.Fatalf("livez status=%d body=%s", w.Code, w.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"checks"`) {
		t.Fatalf("healthz status=%d body=%s", w.Code, w.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"grpc":"unavailable"`) {
		t.Fatalf("readyz status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestStage7MetricsEndpointExposesCoreMetricsAndRedactsSecrets(t *testing.T) {
	resetMetricsForTest()
	s := newTestAPIServer(t)
	now := time.Now()
	_ = s.DB.Create(&model.HotmethodTask{TID: "tid-metrics", Status: TaskStatusDone, UID: "owner", CreateTime: now}).Error
	_ = s.DB.Create(&model.Outbox{Aggregate: model.OutboxAggregateTask, AggregateID: "tid-metrics", Event: model.OutboxEventDispatchTask, Status: model.OutboxStatusDeadLetter}).Error
	_ = s.DB.Create(&model.AnalysisJob{TaskTID: "tid-metrics", Status: model.AnalysisJobStatusSuccess, CreatedAt: now, UpdatedAt: now}).Error
	_ = s.DB.Create(&model.AgentInfo{IPAddr: "127.0.0.1", Online: true, LastSeen: now}).Error
	incTasksCreated()
	incAnalysisQueued()
	incSSEActive()
	defer decSSEActive()

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/metrics", s.Metrics)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("metrics status=%d content-type=%q body=%s", w.Code, w.Header().Get("Content-Type"), body)
	}
	for _, want := range []string{
		"mini_drop_tasks_created_total 1",
		"mini_drop_sse_active_connections 1",
		`mini_drop_tasks_by_status{status="2"} 1`,
		`mini_drop_outbox_by_status{status="dead_letter"} 1`,
		`mini_drop_analysis_jobs_by_status{status="success"} 1`,
		"mini_drop_agents_online 1",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "dropdrop") || strings.Contains(body, "password=dev") {
		t.Fatalf("metrics must not expose secrets: %s", body)
	}
}

func TestStage7GRPCTransportCredentialsRequireValidMTLSFiles(t *testing.T) {
	s := newTestAPIServer(t)
	creds, insecureTransport, err := s.grpcTransportCredentials()
	if err != nil || creds == nil || !insecureTransport {
		t.Fatalf("default grpc credentials err=%v insecure=%v creds=%v", err, insecureTransport, creds)
	}
	s.Config.GRPC.MTLSCertFile = "/no/such/client.crt"
	s.Config.GRPC.MTLSKeyFile = "/no/such/client.key"
	s.Config.GRPC.MTLSCAFile = "/no/such/ca.crt"
	if _, _, err := s.grpcTransportCredentials(); err == nil {
		t.Fatalf("missing mTLS files should fail")
	}
}

func TestTimelineHandlerValidationAndEffectiveWindow(t *testing.T) {
	s := newTestAPIServer(t)
	base := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	params, _ := util.MarshalJSONB(PerfParams{Duration: 60, Frequency: 99})
	for i := 0; i < 2; i++ {
		task := model.HotmethodTask{
			TID:            []string{"child-a", "child-b"}[i],
			Name:           "child",
			MasterTaskTID:  "master-1",
			RequestParams:  params,
			Status:         TaskStatusDone,
			AnalysisStatus: 2,
			CreateTime:     base.Add(time.Duration(i) * time.Hour),
		}
		if err := s.DB.Create(&task).Error; err != nil {
			t.Fatalf("create child: %v", err)
		}
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/tasks/timeline", s.GetTimeline)

	cases := []struct {
		path string
		want int
	}{
		{"/api/v1/tasks/timeline", http.StatusBadRequest},
		{"/api/v1/tasks/timeline?master_tid=master-1&at=bad", http.StatusBadRequest},
		{"/api/v1/tasks/timeline?master_tid=master-1&at=" + urlQuery(base) + "&span=bad", http.StatusBadRequest},
		{"/api/v1/tasks/timeline?master_tid=master-1&from=bad", http.StatusBadRequest},
		{"/api/v1/tasks/timeline?master_tid=master-1&to=bad", http.StatusBadRequest},
		{"/api/v1/tasks/timeline?master_tid=master-1&at=" + urlQuery(base.Add(2*time.Hour)) + "&span=30m", http.StatusOK},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Fatalf("%s status=%d, want %d body=%s", tc.path, w.Code, tc.want, w.Body.String())
		}
	}
}

func urlQuery(t time.Time) string {
	return strings.ReplaceAll(t.Format(time.RFC3339), ":", "%3A")
}

func TestCreateTaskPersistsTaskOutboxAndIdempotency(t *testing.T) {
	s := newTestAPIServer(t)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/tasks", s.CreateTask)

	body := `{"name":"cpu task","task_type":99,"profiler_type":0,"target_ip":"127.0.0.1","duration":2}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Drop-User-Uid", "owner")
	req.Header.Set("Drop-User-Name", "Owner")
	req.Header.Set("Idempotency-Key", "idem-create")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", w.Code, w.Body.String())
	}
	var first map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode first: %v", err)
	}
	tid := first["data"].(map[string]interface{})["tid"].(string)

	var task model.HotmethodTask
	if err := s.DB.Where("tid = ?", tid).First(&task).Error; err != nil {
		t.Fatalf("task not created: %v", err)
	}
	if task.Type != TaskTypeGeneric || task.ProfilerType != ProfilerPerf || task.UID != "owner" || task.UserName != "Owner" {
		t.Fatalf("task fields not normalized/persisted: %#v", task)
	}
	var params PerfParams
	if err := util.UnmarshalJSONB(task.RequestParams, &params); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if params.Frequency != 99 || params.Callgraph != "fp" || params.Event != "cpu-clock" {
		t.Fatalf("defaults not persisted: %#v", params)
	}
	var outbox model.Outbox
	if err := s.DB.Where("aggregate_id = ?", tid).First(&outbox).Error; err != nil {
		t.Fatalf("outbox not created: %v", err)
	}
	if outbox.Aggregate != model.OutboxAggregateTask || outbox.Event != model.OutboxEventDispatchTask {
		t.Fatalf("outbox=%#v, want task dispatch event", outbox)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/tasks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Drop-User-Uid", "owner")
	req.Header.Set("Idempotency-Key", "idem-create")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("replay status=%d body=%s", w.Code, w.Body.String())
	}
	var second map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &second)
	data := second["data"].(map[string]interface{})
	if data["tid"].(string) != tid || data["replayed"].(bool) != true {
		t.Fatalf("idempotency response=%#v, want same tid replayed", data)
	}
}

func TestCreateTaskValidationFailures(t *testing.T) {
	s := newTestAPIServer(t)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/tasks", s.CreateTask)

	cases := []string{
		`{`,
		`{"target_ip":"127.0.0.1"}`,
		`{"name":"bad pprof","profiler_type":2,"target_ip":"127.0.0.1","duration":1}`,
		`{"name":"bad bpf","profiler_type":3,"target_ip":"127.0.0.1","duration":1,"event":"bad"}`,
	}
	for _, body := range cases {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Drop-User-Uid", "owner")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body %s status=%d, want 400 body=%s", body, w.Code, w.Body.String())
		}
	}
}

func TestRetryTaskCreatesChildOutboxAndRespectsOwner(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	params, _ := util.MarshalJSONB(PerfParams{Duration: 3, Frequency: 77, Callgraph: "dwarf", Event: "cpu-cycles"})
	old := model.HotmethodTask{
		TID:            "tid-retry-old",
		Name:           "old",
		Type:           TaskTypeGeneric,
		ProfilerType:   ProfilerPerf,
		TargetIP:       "127.0.0.1",
		RequestParams:  params,
		Status:         TaskStatusFailed,
		AnalysisStatus: 3,
		UID:            "owner",
		UserName:       "Owner",
		CreateTime:     now,
	}
	if err := s.DB.Create(&old).Error; err != nil {
		t.Fatalf("create old task: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/tasks/:tid/retry", s.RetryTask)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/tid-retry-old/retry", nil)
	req.Header.Set("Drop-User-Uid", "other")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross owner retry status=%d, want 403", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/tasks/tid-retry-old/retry", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("retry status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	newTID := resp["data"].(map[string]interface{})["tid"].(string)
	var child model.HotmethodTask
	if err := s.DB.Where("tid = ?", newTID).First(&child).Error; err != nil {
		t.Fatalf("load child: %v", err)
	}
	if child.MasterTaskTID != old.TID || child.Status != TaskStatusCreated || child.Name != "old(重试)" {
		t.Fatalf("child=%#v, want retry child", child)
	}
	var outboxCount int64
	s.DB.Model(&model.Outbox{}).Where("aggregate_id = ?", newTID).Count(&outboxCount)
	if outboxCount != 1 {
		t.Fatalf("outbox count=%d, want 1", outboxCount)
	}
}

func TestLocalFileAndNormalizeHelpers(t *testing.T) {
	s := newTestAPIServer(t)
	if err := os.MkdirAll("/tmp/drop-output", 0o755); err != nil {
		t.Fatalf("mkdir drop-output: %v", err)
	}
	files := map[string]string{
		"/tmp/drop-output/tid-local_top.json":         `{"sample_unit":"samples","sample_kind":"cpu","source_format":"folded","collector":"perf","self_time_top":[{"function":"main","value":3}]}`,
		"/tmp/drop-output/tid-local_bpf_data.json":    `{"buckets":[{"range":"[1,2)","count":4}]}`,
		"/tmp/drop-output/tid-local_suggestions.json": `{"suggestions":[{"function":"main","advice":"cache"}]}`,
		"/tmp/drop-output/tid-local_flamegraph.svg":   `<svg></svg>`,
	}
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		defer os.Remove(path)
	}

	listed := s.listLocalFiles("tid-local")
	if len(listed) != len(files) {
		t.Fatalf("local files=%d, want %d: %#v", len(listed), len(files), listed)
	}
	top := s.fetchLocalTopFunctions("tid-local")
	if len(top) != 1 || top[0]["sample_unit"] != "samples" || top[0]["collector"] != "perf" {
		t.Fatalf("top=%#v, want normalized metadata", top)
	}
	if bpf := s.fetchLocalBPFData("tid-local"); bpf == nil || bpf["buckets"] == nil {
		t.Fatalf("bpf=%#v, want local bpf data", bpf)
	}
	if suggestions := s.fetchLocalSuggestions("tid-local"); len(suggestions) != 1 {
		t.Fatalf("suggestions=%#v, want one", suggestions)
	}

	if got := mimeType("x.png"); got != "image/png" {
		t.Fatalf("png mime=%q", got)
	}
	if got := contentDisposition("attachment", "bad\"\n名.svg"); !strings.Contains(got, "filename*=") || strings.Contains(got, "\n") {
		t.Fatalf("content disposition not sanitized/encoded: %q", got)
	}
	if funcs := normalizeTopFunctions(map[string]interface{}{"top_functions": []interface{}{"skip", map[string]interface{}{"function": "f"}}}); len(funcs) != 1 {
		t.Fatalf("normalized funcs=%#v, want one map item", funcs)
	}
	if suggestions := normalizeSuggestions(map[string]interface{}{"suggestions": []interface{}{"skip", map[string]interface{}{"function": "f"}}}); len(suggestions) != 1 {
		t.Fatalf("normalized suggestions=%#v, want one map item", suggestions)
	}
}

func TestAttemptHelpersAndAuditNoopBranches(t *testing.T) {
	s := newTestAPIServer(t)
	task := model.HotmethodTask{TID: "tid-attempt", TargetIP: "127.0.0.1", UID: "owner", CreateTime: time.Now()}
	if err := s.DB.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	s.recordTaskAttemptFailure(&task, model.AttemptTriggerRecovery, ErrCodeDependencyUnavailable, "grpc down")
	var attempt model.TaskAttempt
	if err := s.DB.Where("task_tid = ?", task.TID).First(&attempt).Error; err != nil {
		t.Fatalf("load attempt: %v", err)
	}
	if attempt.EndTime == nil || attempt.ExitCode != 1 || attempt.ErrorCode != ErrCodeDependencyUnavailable {
		t.Fatalf("attempt=%#v, want recorded failure", attempt)
	}
	s.finishTaskAttempt(0, "", "")
	s.recordTaskStatusEvent("", 0, 1, "", "")
	s.recordAgentAudit("", "", "", "")
	if err := s.transitionTaskStatus(nil, TaskStatusDone, "", "", nil); err != nil {
		t.Fatalf("nil transition should be no-op: %v", err)
	}
}

func TestAgentHandlersAndAuditBranches(t *testing.T) {
	s := newTestAPIServer(t)
	stale := time.Now().Add(-time.Minute)
	agent := model.AgentInfo{
		Hostname: "host-a", IPAddr: "10.0.0.2", Online: true,
		Version: "1.0.0", Environment: "test", LastSeen: stale, CreatedAt: stale, UpdatedAt: stale,
	}
	if err := s.DB.Create(&agent).Error; err != nil {
		t.Fatalf("create agent: %v", err)
	}
	s.ensureAgentAudited(&agent)
	s.ensureAgentAudited(&agent)
	s.markAgentOfflineIfStale(&agent)
	if agent.Online {
		t.Fatal("stale agent should be marked offline")
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/agents", s.ListAgents)
	router.GET("/api/v1/agent/stat", s.StatAgent)
	router.GET("/api/v1/agent/detail", s.GetAgentDetail)
	router.GET("/api/v1/agents/audits", s.ListAgentAudits)

	cases := []struct {
		path string
		want int
	}{
		{"/api/v1/agents", http.StatusOK},
		{"/api/v1/agent/stat", http.StatusBadRequest},
		{"/api/v1/agent/stat?ip=missing", http.StatusNotFound},
		{"/api/v1/agent/stat?ip=10.0.0.2", http.StatusOK},
		{"/api/v1/agent/detail", http.StatusBadRequest},
		{"/api/v1/agent/detail?ip=missing", http.StatusNotFound},
		{"/api/v1/agent/detail?ip=10.0.0.2", http.StatusOK},
		{"/api/v1/agents/audits?limit=1", http.StatusOK},
		{"/api/v1/agents/audits?limit=bad", http.StatusOK},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Fatalf("%s status=%d, want %d body=%s", tc.path, w.Code, tc.want, w.Body.String())
		}
	}
}

func TestGroupCRUDHandlers(t *testing.T) {
	s := newTestAPIServer(t)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/groups", s.CreateGroup)
	router.GET("/api/v1/groups", s.ListGroups)
	router.GET("/api/v1/groups/:gid", s.GetGroupDetail)
	router.PUT("/api/v1/groups/:gid", s.UpdateGroup)
	router.DELETE("/api/v1/groups/:gid", s.DeleteGroup)
	router.POST("/api/v1/groups/:gid/members", s.AddGroupMember)
	router.DELETE("/api/v1/groups/:gid/members/:uid", s.RemoveGroupMember)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/groups", strings.NewReader(`{"name":"ops"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Drop-User-Uid", "owner")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("create group status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	gid := resp["data"].(map[string]interface{})["gid"].(string)

	cases := []struct {
		method string
		path   string
		body   string
		want   int
	}{
		{http.MethodPost, "/api/v1/groups", `{}`, http.StatusBadRequest},
		{http.MethodGet, "/api/v1/groups", "", http.StatusOK},
		{http.MethodGet, "/api/v1/groups/" + gid, "", http.StatusOK},
		{http.MethodGet, "/api/v1/groups/missing", "", http.StatusNotFound},
		{http.MethodPut, "/api/v1/groups/" + gid, `{}`, http.StatusBadRequest},
		{http.MethodPut, "/api/v1/groups/missing", `{"name":"new"}`, http.StatusNotFound},
		{http.MethodPut, "/api/v1/groups/" + gid, `{"name":"new","owner_id":"owner2"}`, http.StatusOK},
		{http.MethodPost, "/api/v1/groups/missing/members", `{"uid":"u2"}`, http.StatusNotFound},
		{http.MethodPost, "/api/v1/groups/" + gid + "/members", `{}`, http.StatusBadRequest},
		{http.MethodPost, "/api/v1/groups/" + gid + "/members", `{"uid":"u2"}`, http.StatusOK},
		{http.MethodPost, "/api/v1/groups/" + gid + "/members", `{"uid":"u2"}`, http.StatusOK},
		{http.MethodDelete, "/api/v1/groups/" + gid + "/members/u2", "", http.StatusOK},
		{http.MethodDelete, "/api/v1/groups/" + gid + "/members/u2", "", http.StatusNotFound},
		{http.MethodDelete, "/api/v1/groups/" + gid, "", http.StatusOK},
		{http.MethodDelete, "/api/v1/groups/" + gid, "", http.StatusNotFound},
	}
	for _, tc := range cases {
		req = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Drop-User-Uid", "owner")
		if strings.Contains(tc.path, gid) && (strings.Contains(tc.path, "/members") || tc.method == http.MethodDelete) {
			req.Header.Set("Drop-User-Role", "PlatformAdmin")
		}
		w = httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Fatalf("%s %s status=%d, want %d body=%s", tc.method, tc.path, w.Code, tc.want, w.Body.String())
		}
	}
}

func TestScheduleHandlersAndExecution(t *testing.T) {
	s := newTestAPIServer(t)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/schedule/task", s.CreateSchedule)
	router.GET("/api/v1/schedule/tasks", s.ListSchedules)
	router.DELETE("/api/v1/schedule/:sid", s.DeleteSchedule)
	router.POST("/api/v1/schedule/:sid/toggle", s.ToggleSchedule)

	valid := `{"name":"cron cpu","cron_expr":"*/5 * * * *","target_ip":"127.0.0.1","duration":2,"window_seconds":60}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/schedule/task", strings.NewReader(valid))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Drop-User-Uid", "owner")
	req.Header.Set("Drop-User-Name", "Owner")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("create schedule status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	sid := resp["data"].(map[string]interface{})["sid"].(string)

	cases := []struct {
		method string
		path   string
		body   string
		want   int
	}{
		{http.MethodPost, "/api/v1/schedule/task", `{}`, http.StatusBadRequest},
		{http.MethodPost, "/api/v1/schedule/task", `{"name":"bad","cron_expr":"bad","target_ip":"127.0.0.1"}`, http.StatusBadRequest},
		{http.MethodPost, "/api/v1/schedule/task", `{"name":"overlap","cron_expr":"*/5 * * * *","target_ip":"127.0.0.1","duration":60,"window_seconds":60}`, http.StatusBadRequest},
		{http.MethodGet, "/api/v1/schedule/tasks", "", http.StatusOK},
		{http.MethodPost, "/api/v1/schedule/missing/toggle", "", http.StatusNotFound},
		{http.MethodPost, "/api/v1/schedule/" + sid + "/toggle", "", http.StatusOK},
		{http.MethodPost, "/api/v1/schedule/" + sid + "/toggle", "", http.StatusOK},
		{http.MethodDelete, "/api/v1/schedule/missing", "", http.StatusNotFound},
	}
	for _, tc := range cases {
		req = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Drop-User-Uid", "owner")
		w = httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Fatalf("%s %s status=%d, want %d body=%s", tc.method, tc.path, w.Code, tc.want, w.Body.String())
		}
	}

	var sch model.ScheduleTask
	if err := s.DB.Where("sid = ?", sid).First(&sch).Error; err != nil {
		t.Fatalf("load schedule: %v", err)
	}
	s.executeScheduledTask(sch)
	s.executeScheduledTask(sch)
	var childCount int64
	s.DB.Model(&model.HotmethodTask{}).Where("master_task_tid = ?", sid).Count(&childCount)
	if childCount != 1 {
		t.Fatalf("scheduled child count=%d, want 1", childCount)
	}
	var triggerCount int64
	s.DB.Model(&model.ScheduleTrigger{}).Where("schedule_id = ?", sid).Count(&triggerCount)
	if triggerCount != 1 {
		t.Fatalf("schedule trigger count=%d, want 1", triggerCount)
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/v1/schedule/"+sid, nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("delete schedule status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestCompositeTaskCreateChildrenAndAggregate(t *testing.T) {
	s := newTestAPIServer(t)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/tasks/composite", s.CreateCompositeTask)
	router.GET("/api/v1/tasks/:tid/children", s.ListTaskChildren)

	body := `{"name":"复合诊断","target_ip":"127.0.0.1","aggregation_policy":"QUORUM","quorum":1,"children":[{"name":"cpu","task_kind":"perf_cpu","duration":2},{"name":"io","task_kind":"ebpf_io","duration":2,"frequency":1}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/composite", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Drop-User-Uid", "owner")
	req.Header.Set("Drop-User-Name", "Owner")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("create composite status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	parentTID := resp["data"].(map[string]interface{})["tid"].(string)

	var outboxCount int64
	s.DB.Model(&model.Outbox{}).Where("aggregate_id = ?", parentTID).Count(&outboxCount)
	if outboxCount != 0 {
		t.Fatalf("parent outbox count=%d, want 0", outboxCount)
	}
	s.DB.Model(&model.Outbox{}).Where("event = ?", model.OutboxEventDispatchTask).Count(&outboxCount)
	if outboxCount != 2 {
		t.Fatalf("child outbox count=%d, want 2", outboxCount)
	}

	var children []model.HotmethodTask
	if err := s.DB.Where("master_task_tid = ?", parentTID).Order("create_time ASC").Find(&children).Error; err != nil || len(children) != 2 {
		t.Fatalf("children len=%d err=%v", len(children), err)
	}
	end := time.Now()
	s.DB.Model(&model.HotmethodTask{}).Where("tid = ?", children[0].TID).Updates(map[string]interface{}{"status": TaskStatusDone, "analysis_status": 2, "end_time": &end})
	req = httptest.NewRequest(http.MethodGet, "/api/v1/tasks/"+parentTID+"/children", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("children status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"status":2`) || !strings.Contains(w.Body.String(), `"aggregation_policy":"QUORUM"`) {
		t.Fatalf("children body missing aggregate success: %s", w.Body.String())
	}
}

func TestTimelineFiltersAndResultMetadata(t *testing.T) {
	s := newTestAPIServer(t)
	base := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	params, _ := util.MarshalJSONB(PerfParams{Duration: 60, Frequency: 99})
	fixtures := []model.HotmethodTask{
		{TID: "tl-ok", Name: "ok", TaskKind: TaskKindPerfCPU, MasterTaskTID: "sch-filter", RequestParams: params, Status: TaskStatusDone, AnalysisStatus: 2, CreateTime: base},
		{TID: "tl-running", Name: "running", TaskKind: TaskKindEBPFIO, MasterTaskTID: "sch-filter", RequestParams: params, Status: TaskStatusRunning, AnalysisStatus: 0, CreateTime: base.Add(time.Minute)},
		{TID: "tl-analysis-failed", Name: "analysis failed", TaskKind: TaskKindPerfCPU, MasterTaskTID: "sch-filter", RequestParams: params, Status: TaskStatusDone, AnalysisStatus: 3, CreateTime: base.Add(2 * time.Minute)},
	}
	if err := s.DB.Create(&fixtures).Error; err != nil {
		t.Fatalf("create timeline fixtures: %v", err)
	}
	_ = s.DB.Create(&model.ScheduleTrigger{ScheduleID: "sch-filter", ScheduledAt: base, ChildTID: "tl-ok", Status: "created", CreatedAt: base}).Error

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/tasks/timeline", s.GetTimeline)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/timeline?master_tid=sch-filter&status=2&task_kind=perf_cpu&has_result=true", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("timeline status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"total":1`) || !strings.Contains(body, `"result_url":"/task/result?tid=tl-ok"`) || !strings.Contains(body, `"duration_seconds":60`) {
		t.Fatalf("timeline body missing filter metadata: %s", body)
	}
	if strings.Contains(body, "tl-analysis-failed") {
		t.Fatalf("analysis failure must not match has_result=true: %s", body)
	}
}

func TestTimelineTrendsCountCanceledWindows(t *testing.T) {
	s := newTestAPIServer(t)
	base := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	fixtures := []model.HotmethodTask{
		{TID: "tl-canceled", Name: "canceled", MasterTaskTID: "sch-canceled", Status: TaskStatusCanceled, CreateTime: base},
		{TID: "tl-failed", Name: "failed", MasterTaskTID: "sch-canceled", Status: TaskStatusFailed, CreateTime: base.Add(time.Minute)},
	}
	if err := s.DB.Create(&fixtures).Error; err != nil {
		t.Fatalf("create timeline fixtures: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/tasks/timeline", s.GetTimeline)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/timeline?master_tid=sch-canceled", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("timeline status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"canceled":1`) || !strings.Contains(w.Body.String(), `"failed":1`) {
		t.Fatalf("timeline trends missing canceled count: %s", w.Body.String())
	}
}

func TestOutboxAndPollerCoordinatorBranches(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now().Add(-time.Minute)
	params, _ := util.MarshalJSONB(PerfParams{Duration: 1, Frequency: 99, Callgraph: "fp", Event: "cpu-clock"})
	task := model.HotmethodTask{
		TID: "tid-outbox", Name: "outbox", TargetIP: "127.0.0.1", RequestParams: params,
		Status: TaskStatusCreated, UID: "owner", CreateTime: now,
	}
	if err := s.DB.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	payload, _ := util.MarshalJSONB(CreateTaskReq{Name: task.Name, TargetIP: task.TargetIP, Duration: 1, Frequency: 99, Callgraph: "fp"})
	entries := []model.Outbox{
		{Aggregate: model.OutboxAggregateTask, AggregateID: task.TID, Event: "unknown", Payload: payload, CreatedAt: now},
		{Aggregate: model.OutboxAggregateTask, AggregateID: task.TID, Event: model.OutboxEventDispatchTask, Payload: []byte("{"), CreatedAt: now},
		{Aggregate: model.OutboxAggregateTask, AggregateID: "missing", Event: model.OutboxEventDispatchTask, Payload: payload, CreatedAt: now},
		{Aggregate: model.OutboxAggregateTask, AggregateID: task.TID, Event: model.OutboxEventDispatchTask, Payload: payload, CreatedAt: now},
	}
	if err := s.DB.Create(&entries).Error; err != nil {
		t.Fatalf("create outbox entries: %v", err)
	}
	s.drainOutbox()

	var published int64
	s.DB.Model(&model.Outbox{}).Where("published_at IS NOT NULL").Count(&published)
	if published != 3 {
		t.Fatalf("published entries=%d, want 3", published)
	}
	var retryEntry model.Outbox
	if err := s.DB.Where("aggregate_id = ? AND published_at IS NULL", task.TID).First(&retryEntry).Error; err != nil {
		t.Fatalf("retry entry not retained: %v", err)
	}
	if retryEntry.Attempt != 1 || retryEntry.NextAttemptAt == nil || retryEntry.LastError == "" {
		t.Fatalf("retry entry=%#v, want scheduled retry", retryEntry)
	}

	retryEntry.Attempt = 2
	s.retryOrFailOutbox(&retryEntry, &task, "still no grpc")
	var failed model.HotmethodTask
	if err := s.DB.Where("tid = ?", task.TID).First(&failed).Error; err != nil {
		t.Fatalf("load failed task: %v", err)
	}
	if failed.Status != TaskStatusFailed {
		t.Fatalf("task status=%d, want failed after max outbox attempts", failed.Status)
	}

	runningStart := time.Now().Add(-2 * time.Second)
	running := model.HotmethodTask{
		TID: "tid-poller-running", Name: "poll running", RequestParams: params,
		Status: TaskStatusRunning, UID: "owner", CreateTime: runningStart, BeginTime: &runningStart,
	}
	uploading := model.HotmethodTask{
		TID: "tid-poller-upload", Name: "poll upload", RequestParams: params,
		Status: TaskStatusUploading, UID: "owner", CreateTime: runningStart, BeginTime: &runningStart,
	}
	timedOutStart := time.Now().Add(-40 * time.Second)
	timedOut := model.HotmethodTask{
		TID: "tid-poller-timeout", Name: "poll timeout", RequestParams: params,
		Status: TaskStatusUploading, UID: "owner", CreateTime: timedOutStart, BeginTime: &timedOutStart,
	}
	if err := s.DB.Create(&running).Error; err != nil {
		t.Fatalf("create running: %v", err)
	}
	if err := s.DB.Create(&uploading).Error; err != nil {
		t.Fatalf("create uploading: %v", err)
	}
	if err := s.DB.Create(&timedOut).Error; err != nil {
		t.Fatalf("create timed out: %v", err)
	}
	s.pollRunningTasks()

	var gotRunning, gotUploading, gotTimedOut model.HotmethodTask
	_ = s.DB.Where("tid = ?", running.TID).First(&gotRunning).Error
	_ = s.DB.Where("tid = ?", uploading.TID).First(&gotUploading).Error
	_ = s.DB.Where("tid = ?", timedOut.TID).First(&gotTimedOut).Error
	if gotRunning.Status != TaskStatusUploading {
		t.Fatalf("running status=%d, want uploading", gotRunning.Status)
	}
	if gotUploading.Status != TaskStatusUploading {
		t.Fatalf("uploading status=%d, want still uploading before deadline", gotUploading.Status)
	}
	if gotTimedOut.Status != TaskStatusFailed || gotTimedOut.AnalysisStatus != 3 || gotTimedOut.EndTime == nil {
		t.Fatalf("timed out task=%#v, want failed with analysis_status=3 and end_time", gotTimedOut)
	}
	if !strings.Contains(gotTimedOut.StatusInfo, "未收到采集产物") {
		t.Fatalf("timed out status_info=%q, want missing artifact reason", gotTimedOut.StatusInfo)
	}
	legacyEnd := time.Now().Add(-time.Hour).Truncate(time.Second)
	legacyNoArtifact := model.HotmethodTask{
		TID: "tid-legacy-no-artifact", Name: "legacy no artifact", RequestParams: params,
		Status: TaskStatusDone, AnalysisStatus: 0, StatusInfo: "上传等待窗口结束，任务自动标记完成",
		UID: "owner", CreateTime: legacyEnd, EndTime: &legacyEnd,
	}
	if err := s.DB.Create(&legacyNoArtifact).Error; err != nil {
		t.Fatalf("create legacy no-artifact task: %v", err)
	}
	s.backfillAnalysisQueueForCompletedTasks()
	var gotLegacy model.HotmethodTask
	_ = s.DB.Where("tid = ?", legacyNoArtifact.TID).First(&gotLegacy).Error
	if gotLegacy.Status != TaskStatusFailed || gotLegacy.AnalysisStatus != 3 {
		t.Fatalf("legacy no-artifact task=%#v, want reconciled failure", gotLegacy)
	}
	if gotLegacy.EndTime == nil || !gotLegacy.EndTime.Equal(legacyEnd) {
		t.Fatalf("legacy no-artifact end_time=%v, want preserved %v", gotLegacy.EndTime, legacyEnd)
	}
	if s.taskHasArtifacts("") {
		t.Fatal("empty tid must not have artifacts")
	}
}

func TestNormalizeCollectorContract(t *testing.T) {
	tests := []struct {
		profiler, wantType uint32
		pid                int32
		event              string
		wantErr            bool
	}{
		{ProfilerPerf, TaskTypeGeneric, 0, "", false},
		{ProfilerAsync, TaskTypeJava, 123, "cpu", false},
		{ProfilerAsync, TaskTypeJava, 0, "", true},
		{ProfilerPprof, TaskTypePprof, 0, "", false},
		{ProfilerBPF, TaskTypeBPF, 0, "sched", false},
		{ProfilerBPF, TaskTypeBPF, 0, "invalid", true},
	}
	for _, tt := range tests {
		req := CreateTaskReq{ProfilerType: tt.profiler, TaskType: 99, TargetPID: tt.pid, Event: tt.event, Duration: 10, Frequency: 99}
		if tt.profiler == ProfilerPprof {
			req.PprofURL = "http://127.0.0.1:6060/debug/pprof/profile"
		}
		err := normalizeAndValidateCollector(&req)
		if (err != nil) != tt.wantErr {
			t.Fatalf("profiler=%d err=%v", tt.profiler, err)
		}
		if err == nil && req.TaskType != tt.wantType {
			t.Fatalf("profiler=%d task_type=%d want=%d", tt.profiler, req.TaskType, tt.wantType)
		}
		if err == nil && tt.profiler == ProfilerPerf && req.Event != "cpu-clock" {
			t.Fatalf("perf default event=%q want cpu-clock", req.Event)
		}
	}
}

func TestNormalizePprofURL(t *testing.T) {
	base := CreateTaskReq{ProfilerType: ProfilerPprof, Duration: 10, Frequency: 1}
	if err := normalizeAndValidateCollector(&base); err == nil {
		t.Fatal("missing pprof_url must fail")
	}
	base.PprofURL = "ftp://example.invalid/profile"
	if err := normalizeAndValidateCollector(&base); err == nil {
		t.Fatal("non-http pprof_url must fail")
	}
	base.PprofURL = ""
	base.Event = "http://127.0.0.1:6060/debug/pprof/profile"
	if err := normalizeAndValidateCollector(&base); err != nil {
		t.Fatalf("legacy event URL must work: %v", err)
	}
	if base.PprofURL == "" || base.Event != "" {
		t.Fatalf("legacy URL was not normalized: %#v", base)
	}
}

func TestTaskKindDefinitionsCoverCurrentCollectors(t *testing.T) {
	kinds := taskKindDefinitions()
	seen := map[string]TaskKindDefinition{}
	for _, kind := range kinds {
		seen[kind.ID] = kind
		if kind.Runner == "" || kind.AnalysisPipeline == "" || kind.MaxDuration == 0 || len(kind.Schema) == 0 {
			t.Fatalf("task kind missing required metadata: %#v", kind)
		}
	}
	for _, id := range []string{TaskKindPerfCPU, TaskKindAsyncProfilerJava, TaskKindGoPprof, TaskKindEBPFCPU, TaskKindEBPFIO, TaskKindEBPFSched} {
		if _, ok := seen[id]; !ok {
			t.Fatalf("missing task kind %s", id)
		}
	}
	for _, id := range []string{TaskKindEBPFCPU, TaskKindEBPFIO, TaskKindEBPFSched} {
		if seen[id].AnalysisPipeline != "bpf_histogram" {
			t.Fatalf("%s pipeline=%q, want bpf_histogram", id, seen[id].AnalysisPipeline)
		}
	}
	if seen[TaskKindPythonPySpy].Enabled || seen[TaskKindJavaHeap].Enabled {
		t.Fatalf("stage6 extension collectors should be declared but disabled by default")
	}
}

func TestTaskKindsHideDisabledCollectorsByDefault(t *testing.T) {
	s := newTestAPIServer(t)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/task-kinds", s.ListTaskKinds)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/task-kinds", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), TaskKindPythonPySpy) {
		t.Fatalf("disabled collector leaked by default status=%d body=%s", w.Code, w.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/task-kinds?include_disabled=true", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), TaskKindPythonPySpy) || !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("disabled collector declaration missing status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestTaskKindNormalizesLegacyProfilerRequest(t *testing.T) {
	req := CreateTaskReq{ProfilerType: ProfilerBPF, Event: "io", Duration: 10, Frequency: 1}
	if err := normalizeAndValidateCollector(&req); err != nil {
		t.Fatalf("normalize legacy ebpf io: %v", err)
	}
	if req.TaskKind != TaskKindEBPFIO || req.TaskType != TaskTypeBPF || req.ProfilerType != ProfilerBPF {
		t.Fatalf("normalized request=%#v, want ebpf io task kind and bpf types", req)
	}
}

func TestAgentCapabilityMatchingIsConservative(t *testing.T) {
	s := newTestAPIServer(t)
	kind, _ := taskKindByID(TaskKindGoPprof)
	if !s.agentSupportsTaskKind("127.0.0.1", kind) {
		t.Fatal("missing agent capability data should allow current demo collectors")
	}
	if err := s.DB.Create(&model.AgentInfo{IPAddr: "10.0.0.9", Hostname: "agent-1", Capabilities: []byte(`["perf"]`)}).Error; err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if s.agentSupportsTaskKind("10.0.0.9", kind) {
		t.Fatal("agent with explicit perf-only capabilities must not match pprof")
	}
	perfKind, _ := taskKindByID(TaskKindPerfCPU)
	if !s.agentSupportsTaskKind("10.0.0.9", perfKind) {
		t.Fatal("agent with perf capability should match perf task kind")
	}

	if err := s.DB.Create(&model.AgentInfo{IPAddr: "10.0.0.10", Hostname: "legacy-ebpf", Capabilities: []byte(`["ebpf_io","ebpf_sched"]`)}).Error; err != nil {
		t.Fatalf("create legacy ebpf agent: %v", err)
	}
	ebpfCPUKind, _ := taskKindByID(TaskKindEBPFCPU)
	if !s.agentSupportsTaskKind("10.0.0.10", ebpfCPUKind) {
		t.Fatal("agent with legacy ebpf_io/ebpf_sched capabilities should match ebpf_cpu")
	}
	if s.agentSupportsTaskKind("10.0.0.9", ebpfCPUKind) {
		t.Fatal("agent with explicit perf-only capabilities must not match ebpf_cpu")
	}
}
