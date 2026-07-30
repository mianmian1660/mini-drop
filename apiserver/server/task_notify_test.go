package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mini-drop/apiserver/pkg/storage"
)

func TestPickRawCollectionObjectPrefersPerfData(t *testing.T) {
	key, size, ok := pickRawCollectionObject([]storage.FileInfo{
		{Name: "tid-1/flamegraph.svg", Size: 100},
		{Name: "tid-1/top.json", Size: 20},
		{Name: "tid-1/perf.data", Size: 2048},
		{Name: "tid-1/raw.bpf", Size: 512},
	})

	if !ok {
		t.Fatal("expected raw collection artifact")
	}
	if key != "tid-1/perf.data" || size != 2048 {
		t.Fatalf("key=%q size=%d, want tid-1/perf.data 2048", key, size)
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
