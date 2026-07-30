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
