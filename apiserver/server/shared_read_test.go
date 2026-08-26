package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mini-drop/apiserver/model"
)

func TestSharedReadListsAndOwnerFilter(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	for _, task := range []model.HotmethodTask{
		{TID: "tid-owner", Name: "owner task", UID: "owner", UserName: "Owner", CreateTime: now},
		{TID: "tid-other", Name: "other task", UID: "other", UserName: "Other", CreateTime: now.Add(time.Second)},
	} {
		if err := s.DB.Create(&task).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, schedule := range []model.ScheduleTask{
		{SID: "sch-owner", Name: "owner schedule", UID: "owner", UserName: "Owner", CreatedAt: now, UpdatedAt: now},
		{SID: "sch-other", Name: "other schedule", UID: "other", UserName: "Other", CreatedAt: now.Add(time.Second), UpdatedAt: now},
	} {
		if err := s.DB.Create(&schedule).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, session := range []model.ContinuousSession{
		{SID: "cps-owner", Name: "owner session", UID: "owner", UserName: "Owner", StartedAt: now, CreatedAt: now, UpdatedAt: now},
		{SID: "cps-other", Name: "other session", UID: "other", UserName: "Other", StartedAt: now, CreatedAt: now.Add(time.Second), UpdatedAt: now},
		{SID: "cps-test", Name: "multilang-node-plain", UID: "owner", UserName: "Owner", StartedAt: now, CreatedAt: now.Add(2 * time.Second), UpdatedAt: now},
	} {
		if err := s.DB.Create(&session).Error; err != nil {
			t.Fatal(err)
		}
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/tasks", s.ListTasks)
	router.GET("/api/v1/schedule/tasks", s.ListSchedules)
	router.GET("/api/v1/continuous/sessions", s.ListContinuousSessions)
	router.GET("/api/v1/continuous/sessions/:sid", s.GetContinuousSession)

	assertTotal := func(path, uid string, want int) map[string]interface{} {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Drop-User-Uid", uid)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, w.Code, w.Body.String())
		}
		var response map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		data := response["data"].(map[string]interface{})
		if int(data["total"].(float64)) != want {
			t.Fatalf("%s total=%v want=%d", path, data["total"], want)
		}
		return data
	}

	assertTotal("/api/v1/tasks?pageSize=10", "reader", 2)
	ownerTasks := assertTotal("/api/v1/tasks?pageSize=10&owner_filter=mine", "owner", 1)
	if !ownerTasks["tasks"].([]interface{})[0].(map[string]interface{})["can_manage"].(bool) {
		t.Fatal("owner task should be manageable")
	}
	assertTotal("/api/v1/schedule/tasks", "reader", 2)
	assertTotal("/api/v1/schedule/tasks?owner_filter=mine", "owner", 1)
	assertTotal("/api/v1/continuous/sessions?page_size=10", "reader", 3)
	assertTotal("/api/v1/continuous/sessions?page_size=10&owner_filter=mine", "owner", 2)
	assertTotal("/api/v1/continuous/sessions?page_size=10&test_filter=exclude", "reader", 2)
	assertTotal("/api/v1/continuous/sessions?page_size=10&test_filter=only", "reader", 1)

	badFilter := httptest.NewRequest(http.MethodGet, "/api/v1/continuous/sessions?test_filter=bad", nil)
	badFilter.Header.Set("Drop-User-Uid", "reader")
	badFilterResponse := httptest.NewRecorder()
	router.ServeHTTP(badFilterResponse, badFilter)
	if badFilterResponse.Code != http.StatusBadRequest {
		t.Fatalf("invalid test_filter status=%d body=%s", badFilterResponse.Code, badFilterResponse.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/continuous/sessions/cps-other", nil)
	req.Header.Set("Drop-User-Uid", "reader")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !containsJSONField(w.Body.Bytes(), `"can_manage":false`) {
		t.Fatalf("shared continuous detail status=%d body=%s", w.Code, w.Body.String())
	}
}

func containsJSONField(body []byte, value string) bool {
	for index := 0; index+len(value) <= len(body); index++ {
		if string(body[index:index+len(value)]) == value {
			return true
		}
	}
	return false
}
