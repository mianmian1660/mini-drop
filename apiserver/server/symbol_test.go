package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mini-drop/apiserver/model"
)

func newSymbolTestServer(t *testing.T) *APIServer {
	t.Helper()
	s := newTestAPIServer(t)
	s.Storage = fakeStorage{}
	return s
}

func symbolTestRouter(s *APIServer) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/symbols/check", s.CheckSymbols)
	router.PUT("/api/v1/symbols/:build_id", s.UploadSymbol)
	return router
}

func TestCheckSymbolsReportsMissingForUnknownBuildID(t *testing.T) {
	s := newSymbolTestServer(t)
	router := symbolTestRouter(s)

	body := `{"tid":"tid-1","entries":[{"build_id":"aabbccdd11223344","dso_path":"/usr/local/bin/pprof-demo"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/symbols/check", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "aabbccdd11223344") {
		t.Fatalf("expected missing to include the unknown build_id, got %s", w.Body.String())
	}

	var row model.TaskBuildID
	if err := s.DB.Where("tid = ? AND build_id = ?", "tid-1", "aabbccdd11223344").First(&row).Error; err != nil {
		t.Fatalf("task_build_ids row not recorded: %v", err)
	}
	if row.DSOPath != "/usr/local/bin/pprof-demo" {
		t.Fatalf("dso_path not recorded: %#v", row)
	}
}

func TestUploadSymbolThenCheckExcludesIt(t *testing.T) {
	s := newSymbolTestServer(t)
	router := symbolTestRouter(s)

	buildID := "aabbccdd11223344"
	elfBody := append([]byte("\x7fELF"), []byte("fake-elf-content-for-test")...)
	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/symbols/"+buildID, bytes.NewReader(elfBody))
	putReq.ContentLength = int64(len(elfBody))
	putW := httptest.NewRecorder()
	router.ServeHTTP(putW, putReq)
	if putW.Code != http.StatusOK {
		t.Fatalf("upload status = %d, body = %s", putW.Code, putW.Body.String())
	}
	if !strings.Contains(putW.Body.String(), "ready") {
		t.Fatalf("expected status ready, got %s", putW.Body.String())
	}

	body := `{"tid":"tid-2","entries":[{"build_id":"` + buildID + `","dso_path":"/usr/local/bin/pprof-demo"}]}`
	checkReq := httptest.NewRequest(http.MethodPost, "/api/v1/symbols/check", strings.NewReader(body))
	checkReq.Header.Set("Content-Type", "application/json")
	checkW := httptest.NewRecorder()
	router.ServeHTTP(checkW, checkReq)

	if checkW.Code != http.StatusOK {
		t.Fatalf("check status = %d, body = %s", checkW.Code, checkW.Body.String())
	}
	if strings.Contains(checkW.Body.String(), buildID) {
		t.Fatalf("build_id should no longer be missing after upload, got %s", checkW.Body.String())
	}
}

func TestUploadSymbolConcurrentDuplicateKeepsOneRow(t *testing.T) {
	s := newSymbolTestServer(t)
	router := symbolTestRouter(s)

	buildID := "ffeeddccbbaa9988"
	elfBody := append([]byte("\x7fELF"), []byte("dup-content")...)

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPut, "/api/v1/symbols/"+buildID, bytes.NewReader(elfBody))
			req.ContentLength = int64(len(elfBody))
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Errorf("concurrent upload status = %d, body = %s", w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()

	var count int64
	if err := s.DB.Model(&model.SymbolFile{}).Where("build_id = ?", buildID).Count(&count).Error; err != nil {
		t.Fatalf("count symbol_files: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 row for duplicate uploads, got %d", count)
	}
}

func TestUploadSymbolRejectsMalformedBuildID(t *testing.T) {
	s := newSymbolTestServer(t)
	router := symbolTestRouter(s)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/symbols/not-hex!!", bytes.NewReader([]byte("\x7fELFdata")))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed build_id, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUploadSymbolRejectsNonELFContent(t *testing.T) {
	s := newSymbolTestServer(t)
	router := symbolTestRouter(s)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/symbols/aabbccdd11223344", bytes.NewReader([]byte("not an elf file")))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-ELF content, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCheckSymbolsEmptyEntriesReturnsEmptyMissing(t *testing.T) {
	s := newSymbolTestServer(t)
	router := symbolTestRouter(s)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/symbols/check", strings.NewReader(`{"tid":"tid-3","entries":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"missing":[]`) {
		t.Fatalf("expected empty missing array, got %s", w.Body.String())
	}
}
