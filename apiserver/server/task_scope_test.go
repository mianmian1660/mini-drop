// ============================================================
// server/task_scope_test.go — 周期性采样与单次任务列表重构
// 覆盖：
//   1. ListTasks 的 task_scope=single|periodic|all 过滤
//   2. 复合子任务 / 人工重试仍归属单次，周期窗口只归属 periodic
//   3. 周期计划删除后其历史窗口仍归 periodic，不回流单次
//   4. 计划详情接口权限（can_manage）与 404
//   5. 计划列表 target_ip / keyword / enabled / 分页过滤 与 next_run_at
// ============================================================

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mini-drop/apiserver/model"
)

// seedScopeTasks 写入一组覆盖四种归属的任务：
//   - normal：普通任务（master 为空）
//   - comp：复合子任务（master 是父任务 tid）
//   - retry：人工重试（master 是原任务 tid）
//   - periodic：周期计划生成的窗口（master 是 sch-*）
func seedScopeTasks(t *testing.T, s *APIServer) {
	t.Helper()
	base := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	seed := []model.HotmethodTask{
		{TID: "t-normal", Name: "普通任务", TaskKind: "perf_cpu", TargetIP: "1.2.3.4", Status: TaskStatusDone, AnalysisStatus: 2, UID: "owner", UserName: "Owner", CreateTime: base.Add(time.Minute), MasterTaskTID: ""},
		{TID: "t-comp", Name: "复合子任务", TaskKind: "perf_cpu", TargetIP: "1.2.3.4", Status: TaskStatusDone, AnalysisStatus: 2, UID: "owner", UserName: "Owner", CreateTime: base.Add(2 * time.Minute), MasterTaskTID: "tid-20260822-parent"},
		{TID: "t-retry", Name: "人工重试", TaskKind: "perf_cpu", TargetIP: "1.2.3.4", Status: TaskStatusDone, AnalysisStatus: 2, UID: "owner", UserName: "Owner", CreateTime: base.Add(3 * time.Minute), MasterTaskTID: "t-normal"},
		{TID: "t-periodic", Name: "周期窗口", TaskKind: "perf_cpu", TargetIP: "1.2.3.4", Status: TaskStatusDone, AnalysisStatus: 2, UID: "owner", UserName: "Owner", CreateTime: base.Add(4 * time.Minute), MasterTaskTID: "sch-20260820-abc"},
	}
	if err := s.DB.Create(&seed).Error; err != nil {
		t.Fatalf("seed tasks: %v", err)
	}
}

func scopeTIDs(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	tasksRaw, ok := resp["data"].(map[string]interface{})["tasks"].([]interface{})
	if !ok {
		t.Fatalf("tasks 缺失: %s", w.Body.String())
	}
	var tids []string
	for _, item := range tasksRaw {
		tids = append(tids, item.(map[string]interface{})["tid"].(string))
	}
	return tids
}

func TestListTasksTaskScope(t *testing.T) {
	s := newTestAPIServer(t)
	seedScopeTasks(t, s)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/tasks", s.ListTasks)

	doGet := func(query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks?"+query, nil)
		req.Header.Set("Drop-User-Uid", "owner")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	// 默认 all：四种都返回
	w := doGet("")
	if got := scopeTIDs(t, w); len(got) != 4 {
		t.Fatalf("all 应有 4 条，实际 %v", got)
	}

	// single：排除周期窗口，保留普通/复合/重试
	w = doGet("task_scope=single")
	got := scopeTIDs(t, w)
	expected := map[string]bool{"t-normal": true, "t-comp": true, "t-retry": true}
	if len(got) != 3 {
		t.Fatalf("single 应有 3 条，实际 %v", got)
	}
	for _, tid := range got {
		if !expected[tid] {
			t.Fatalf("single 不应包含 %s", tid)
		}
		if tid == "t-periodic" {
			t.Fatalf("single 不应包含周期窗口 t-periodic")
		}
	}

	// periodic：只返回周期窗口
	w = doGet("task_scope=periodic")
	got = scopeTIDs(t, w)
	if len(got) != 1 || got[0] != "t-periodic" {
		t.Fatalf("periodic 应只有 t-periodic，实际 %v", got)
	}

	// 非法值 → 400
	w = doGet("task_scope=bad")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法 task_scope 应 400，实际 %d", w.Code)
	}
}

func TestTaskScopeDeletedScheduleHistory(t *testing.T) {
	s := newTestAPIServer(t)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/tasks", s.ListTasks)
	router.POST("/api/v1/schedule/task", s.CreateSchedule)
	router.DELETE("/api/v1/schedule/:sid", s.DeleteSchedule)

	// 创建计划并让 cron 触发生成一个采集窗口
	body := `{"name":"历史计划","cron_expr":"*/5 * * * *","target_ip":"9.9.9.9","duration":2,"window_seconds":60}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/schedule/task", strings.NewReader(body))
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

	var sch model.ScheduleTask
	if err := s.DB.Where("sid = ?", sid).First(&sch).Error; err != nil {
		t.Fatalf("load schedule: %v", err)
	}
	s.executeScheduledTask(sch)

	// 删除计划（历史窗口保留在 DB）
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/schedule/"+sid, nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("delete schedule status=%d body=%s", w.Code, w.Body.String())
	}

	// 删除后历史窗口仍归 periodic
	req = httptest.NewRequest(http.MethodGet, "/api/v1/tasks?task_scope=periodic", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	got := scopeTIDs(t, w)
	if len(got) != 1 || !strings.HasPrefix(got[0], "tid-") {
		t.Fatalf("删除计划后 periodic 应保留 1 个历史窗口，实际 %v", got)
	}

	// 且不会回流单次任务列表
	req = httptest.NewRequest(http.MethodGet, "/api/v1/tasks?task_scope=single", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	got = scopeTIDs(t, w)
	if len(got) != 0 {
		t.Fatalf("删除计划后 single 应为空，实际 %v", got)
	}
}

func TestScheduleDetailPermission(t *testing.T) {
	s := newTestAPIServer(t)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/schedule/:sid", s.GetScheduleDetail)

	now := time.Now()
	if err := s.DB.Create(&model.ScheduleTask{
		SID: "sch-detail", Name: "详情计划", CronExpr: "*/5 * * * *",
		TargetIP: "1.2.3.4", Enabled: true, UID: "owner", UserName: "Owner",
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("seed schedule: %v", err)
	}

	getDetail := func(uid string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/schedule/sch-detail", nil)
		req.Header.Set("Drop-User-Uid", uid)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	// 属主：can_manage=true，且带 next_run_at
	w := getDetail("owner")
	if w.Code != http.StatusOK {
		t.Fatalf("owner detail status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	data := resp["data"].(map[string]interface{})
	if data["can_manage"] != true {
		t.Fatalf("owner can_manage 应为 true")
	}
	if _, ok := data["next_run_at"]; !ok {
		t.Fatalf("enabled 计划应带 next_run_at")
	}

	// 非属主：仍可读，can_manage=false
	w = getDetail("viewer")
	if w.Code != http.StatusOK {
		t.Fatalf("viewer detail status=%d body=%s", w.Code, w.Body.String())
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	data = resp["data"].(map[string]interface{})
	if data["can_manage"] != false {
		t.Fatalf("viewer can_manage 应为 false")
	}

	// 不存在 → 404
	req := httptest.NewRequest(http.MethodGet, "/api/v1/schedule/missing", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing detail 应 404，实际 %d", w.Code)
	}
}

func TestListSchedulesFiltersAndPagination(t *testing.T) {
	s := newTestAPIServer(t)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/schedule/tasks", s.ListSchedules)

	now := time.Now()
	seed := []model.ScheduleTask{
		{SID: "sch-1", Name: "CPU 计划", CronExpr: "*/5 * * * *", TargetIP: "1.2.3.4", Enabled: true, UID: "owner", UserName: "Owner", CreatedAt: now},
		{SID: "sch-2", Name: "IO 计划", CronExpr: "*/10 * * * *", TargetIP: "1.2.3.4", Enabled: true, UID: "owner", UserName: "Owner", CreatedAt: now.Add(time.Minute)},
		{SID: "sch-3", Name: "堆内存", CronExpr: "0 * * * *", TargetIP: "5.6.7.8", Enabled: true, UID: "other", UserName: "Other", CreatedAt: now.Add(2 * time.Minute)},
	}
	for i := range seed {
		seed[i].UpdatedAt = seed[i].CreatedAt
	}
	if err := s.DB.Create(&seed).Error; err != nil {
		t.Fatalf("seed schedules: %v", err)
	}
	// Enabled 字段带 default:true，Create 时零值 false 会被 GORM 省略而落库为 true；
	// 停用状态与生产一致地通过显式 Update 写入。
	if err := s.DB.Model(&model.ScheduleTask{}).Where("sid = ?", "sch-2").Update("enabled", false).Error; err != nil {
		t.Fatalf("disable sch-2: %v", err)
	}

	doGet := func(query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/schedule/tasks?"+query, nil)
		req.Header.Set("Drop-User-Uid", "owner")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	countSchedules := func(w *httptest.ResponseRecorder) (int, int) {
		var resp map[string]interface{}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		data := resp["data"].(map[string]interface{})
		return len(data["schedules"].([]interface{})), int(data["total"].(float64))
	}

	// target_ip 过滤
	w := doGet("target_ip=1.2.3.4")
	if n, total := countSchedules(w); n != 2 || total != 2 {
		t.Fatalf("target_ip 过滤 应 2 条，实际 n=%d total=%d", n, total)
	}

	// enabled=false
	w = doGet("enabled=false")
	if n, total := countSchedules(w); n != 1 || total != 1 {
		t.Fatalf("enabled=false 应 1 条，实际 n=%d total=%d", n, total)
	}

	// keyword 匹配名称
	w = doGet("keyword=%E5%A0%86%E5%86%85%E5%AD%98")
	if n, _ := countSchedules(w); n != 1 {
		t.Fatalf("keyword 堆内存 应 1 条，实际 %d", n)
	}

	// owner_filter=mine 只看属主
	w = doGet("owner_filter=mine")
	if n, total := countSchedules(w); n != 2 || total != 2 {
		t.Fatalf("mine 应 2 条，实际 n=%d total=%d", n, total)
	}

	// 分页：page_size=2 → 返回 2 条，total=3
	w = doGet("page=1&page_size=2")
	if n, total := countSchedules(w); n != 2 || total != 3 {
		t.Fatalf("分页 应 n=2 total=3，实际 n=%d total=%d", n, total)
	}
	w = doGet("page=2&page_size=2")
	if n, _ := countSchedules(w); n != 1 {
		t.Fatalf("第 2 页应 1 条，实际 %d", n)
	}

	// 启用的计划应带 next_run_at
	req := httptest.NewRequest(http.MethodGet, "/api/v1/schedule/tasks?enabled=true", nil)
	req.Header.Set("Drop-User-Uid", "owner")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	for _, item := range resp["data"].(map[string]interface{})["schedules"].([]interface{}) {
		m := item.(map[string]interface{})
		if _, ok := m["next_run_at"]; !ok {
			t.Fatalf("enabled 计划应带 next_run_at: %v", m["sid"])
		}
	}
}
