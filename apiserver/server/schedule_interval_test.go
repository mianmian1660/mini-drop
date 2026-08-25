// ============================================================
// server/schedule_interval_test.go — 间隔型周期计划
// 覆盖：
//   1. CreateSchedule 的"间隔 + 开始时间"字段校验与默认值
//   2. 采样时长必须小于采样间隔（重叠校验，不信任旧 window_seconds）
//   3. start_at / next_run_at 对齐计算
//   4. DB 轮询 worker 触发、多实例重复触发保护、错过多个周期恢复
//   5. 停用/启用不影响 cron 注册（间隔型不注册 cron）
//   6. 旧 cron 任务兼容读取与运行
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
	"github.com/mini-drop/apiserver/util"
)

// createScheduleViaHTTP 用 HTTP 请求创建周期计划，返回响应记录器。
func createScheduleViaHTTP(t *testing.T, s *APIServer, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/schedule/task", s.CreateSchedule)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/schedule/task", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Drop-User-Uid", "owner")
	req.Header.Set("Drop-User-Name", "Owner")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestCreateScheduleIntervalMode(t *testing.T) {
	s := newTestAPIServer(t)
	future := time.Now().Add(10 * time.Minute)
	body := `{"name":"间隔计划","interval_seconds":300,"start_at":"` + future.Format(time.RFC3339) + `","target_ip":"127.0.0.1","duration":290}`
	w := createScheduleViaHTTP(t, s, body)
	if w.Code != http.StatusOK {
		t.Fatalf("create interval schedule status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	data := resp["data"].(map[string]interface{})
	if data["interval_seconds"].(float64) != 300 {
		t.Fatalf("interval_seconds 应为 300，实际 %v", data["interval_seconds"])
	}
	if _, ok := data["start_at"]; !ok {
		t.Fatalf("应返回 start_at")
	}
	// 未来 start_at → next_run_at == start_at
	nextRun, _ := time.Parse(time.RFC3339, data["next_run_at"].(string))
	if !nextRun.Truncate(time.Second).Equal(future.Truncate(time.Second)) {
		t.Fatalf("next_run_at 应为 start_at %v，实际 %v", future, nextRun)
	}

	sid := data["sid"].(string)
	// 间隔型计划不注册进程内 cron
	if _, ok := s.CronJobs[sid]; ok {
		t.Fatalf("间隔型计划不应注册 cron: %s", sid)
	}

	// request_params 固化 interval_seconds / window_seconds，防止配置漂移
	var sch model.ScheduleTask
	if err := s.DB.Where("sid = ?", sid).First(&sch).Error; err != nil {
		t.Fatalf("load schedule: %v", err)
	}
	var params PerfParams
	if err := util.UnmarshalJSONB(sch.RequestParams, &params); err != nil {
		t.Fatalf("unmarshal request_params: %v", err)
	}
	if params.IntervalSeconds != 300 || params.WindowSeconds != 300 {
		t.Fatalf("request_params 应固化 interval=300 window=300，实际 %+v", params)
	}
}

func TestCreateScheduleValidation(t *testing.T) {
	s := newTestAPIServer(t)
	cases := []struct {
		name string
		body string
		want int
	}{
		{"既无 cron 也无 interval", `{"name":"x","target_ip":"127.0.0.1","duration":2}`, http.StatusBadRequest},
		{"interval 小于 1 分钟", `{"name":"x","interval_seconds":30,"target_ip":"127.0.0.1","duration":2}`, http.StatusBadRequest},
		{"duration 等于 interval", `{"name":"x","interval_seconds":60,"target_ip":"127.0.0.1","duration":60}`, http.StatusBadRequest},
		{"duration 超过 interval", `{"name":"x","interval_seconds":60,"target_ip":"127.0.0.1","duration":120}`, http.StatusBadRequest},
		// 后端永远以最终的 interval_seconds 校验，不信任前端伪造的旧 window_seconds
		{"duration>=interval 但 window_seconds 伪造为大值", `{"name":"x","interval_seconds":60,"target_ip":"127.0.0.1","duration":120,"window_seconds":3600}`, http.StatusBadRequest},
		{"合法的 interval 计划", `{"name":"x","interval_seconds":300,"target_ip":"127.0.0.1","duration":290}`, http.StatusOK},
	}
	for _, tc := range cases {
		w := createScheduleViaHTTP(t, s, tc.body)
		if w.Code != tc.want {
			t.Fatalf("[%s] status=%d want %d body=%s", tc.name, w.Code, tc.want, w.Body.String())
		}
	}
}

func TestIntervalNextRunAlignment(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	interval := uint64(300)

	// 未来 start_at → 直接取 start_at
	future := now.Add(10 * time.Minute)
	if got := intervalNextRun(&future, interval, now); !got.Equal(future) {
		t.Fatalf("future start_at 应返回 start_at，实际 %v", got)
	}

	// 过去 start_at → 对齐到严格晚于 now 的最近槽位
	past := now.Add(-7 * time.Minute) // 12:00 - 7min = 11:53
	got := intervalNextRun(&past, interval, now)
	// 11:53 + 5min*k，第一个 > 12:00 的是 12:03
	want := now.Add(3 * time.Minute)
	if !got.Equal(want) {
		t.Fatalf("past start_at 应对齐到 %v，实际 %v", want, got)
	}

	// 缺省（nil）start_at → 从 now 对齐：now 本身 > now 不成立，取 now+interval
	got = intervalNextRun(nil, interval, now)
	want = now.Add(5 * time.Minute)
	if !got.Equal(want) {
		t.Fatalf("nil start_at 应对齐到 %v，实际 %v", want, got)
	}
}

func TestIntervalScheduleWorkerTriggersOnce(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	next := now.Add(-2 * time.Minute) // 已到期 2 分钟
	if err := s.DB.Create(&model.ScheduleTask{
		SID: "sch-int", Name: "间隔计划", TaskKind: "perf_cpu", TargetIP: "127.0.0.1",
		IntervalSeconds: 300, Enabled: true, NextRunAt: &next,
		UID: "owner", UserName: "Owner", CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("seed schedule: %v", err)
	}

	var sch model.ScheduleTask
	if err := s.DB.Where("sid = ?", "sch-int").First(&sch).Error; err != nil {
		t.Fatalf("load schedule: %v", err)
	}

	// 同一到期时刻触发两次 → 唯一约束去重，只创建一个子任务
	s.triggerIntervalSchedule(sch, now)
	s.triggerIntervalSchedule(sch, now)

	var childCount int64
	s.DB.Model(&model.HotmethodTask{}).Where("master_task_tid = ?", "sch-int").Count(&childCount)
	if childCount != 1 {
		t.Fatalf("子任务数应为 1，实际 %d", childCount)
	}
	var triggerCount int64
	s.DB.Model(&model.ScheduleTrigger{}).Where("schedule_id = ?", "sch-int").Count(&triggerCount)
	if triggerCount != 1 {
		t.Fatalf("trigger 数应为 1，实际 %d", triggerCount)
	}

	// next_run_at 已推进到未来
	s.DB.Where("sid = ?", "sch-int").First(&sch)
	if sch.NextRunAt == nil || !sch.NextRunAt.After(now) {
		t.Fatalf("next_run_at 应推进到未来，实际 %v", sch.NextRunAt)
	}
}

func TestIntervalScheduleMissedPeriodsSkipBurst(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	// 错过 3 个周期（15 分钟前到期，间隔 5 分钟）
	missed := now.Add(-15 * time.Minute)
	if err := s.DB.Create(&model.ScheduleTask{
		SID: "sch-miss", Name: "错过计划", TaskKind: "perf_cpu", TargetIP: "127.0.0.1",
		IntervalSeconds: 300, Enabled: true, NextRunAt: &missed,
		UID: "owner", UserName: "Owner", CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("seed schedule: %v", err)
	}

	var sch model.ScheduleTask
	if err := s.DB.Where("sid = ?", "sch-miss").First(&sch).Error; err != nil {
		t.Fatalf("load schedule: %v", err)
	}
	s.triggerIntervalSchedule(sch, now)

	// 只补跑 1 个窗口（不爆发多个）
	var childCount int64
	s.DB.Model(&model.HotmethodTask{}).Where("master_task_tid = ?", "sch-miss").Count(&childCount)
	if childCount != 1 {
		t.Fatalf("错过多个周期后应只补跑 1 个窗口，实际 %d", childCount)
	}

	// next_run_at 跳过错过槽位，落到未来
	s.DB.Where("sid = ?", "sch-miss").First(&sch)
	if sch.NextRunAt == nil || !sch.NextRunAt.After(now) {
		t.Fatalf("next_run_at 应跳到未来，实际 %v", sch.NextRunAt)
	}
}

func TestIntervalSchedulePollSkipsDisabled(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	due := now.Add(-time.Minute)
	if err := s.DB.Create(&model.ScheduleTask{
		SID: "sch-off", Name: "停用计划", TaskKind: "perf_cpu", TargetIP: "127.0.0.1",
		IntervalSeconds: 300, Enabled: false, NextRunAt: &due,
		UID: "owner", UserName: "Owner", CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("seed schedule: %v", err)
	}
	s.pollDueSchedules()
	var childCount int64
	s.DB.Model(&model.HotmethodTask{}).Where("master_task_tid = ?", "sch-off").Count(&childCount)
	if childCount != 0 {
		t.Fatalf("停用计划不应触发，实际子任务数 %d", childCount)
	}
}

func TestIntervalScheduleToggleResumes(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	due := now.Add(-time.Minute)
	if err := s.DB.Create(&model.ScheduleTask{
		SID: "sch-tog", Name: "启停计划", TaskKind: "perf_cpu", TargetIP: "127.0.0.1",
		IntervalSeconds: 300, Enabled: true, NextRunAt: &due,
		UID: "owner", UserName: "Owner", CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("seed schedule: %v", err)
	}

	// 停用：更新 enabled=false（生产 Toggle 走 Update 显式写，避开 default:true）
	if err := s.DB.Model(&model.ScheduleTask{}).Where("sid = ?", "sch-tog").Update("enabled", false).Error; err != nil {
		t.Fatalf("disable: %v", err)
	}
	s.pollDueSchedules()
	var childCount int64
	s.DB.Model(&model.HotmethodTask{}).Where("master_task_tid = ?", "sch-tog").Count(&childCount)
	if childCount != 0 {
		t.Fatalf("停用后不应触发，实际子任务数 %d", childCount)
	}

	// 重新启用：next_run_at 仍在过去 → 恢复触发
	if err := s.DB.Model(&model.ScheduleTask{}).Where("sid = ?", "sch-tog").Update("enabled", true).Error; err != nil {
		t.Fatalf("enable: %v", err)
	}
	s.pollDueSchedules()
	s.DB.Model(&model.HotmethodTask{}).Where("master_task_tid = ?", "sch-tog").Count(&childCount)
	if childCount != 1 {
		t.Fatalf("重新启用后应触发 1 次，实际 %d", childCount)
	}
}

func TestLegacyCronScheduleStillWorks(t *testing.T) {
	s := newTestAPIServer(t)
	// 旧客户端仍可用 cron_expr 创建
	body := `{"name":"cron 兼容","cron_expr":"*/5 * * * *","target_ip":"127.0.0.1","duration":2,"window_seconds":60}`
	w := createScheduleViaHTTP(t, s, body)
	if w.Code != http.StatusOK {
		t.Fatalf("legacy cron create status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	sid := resp["data"].(map[string]interface{})["sid"].(string)

	// 旧 cron 计划注册进程内 cron
	if _, ok := s.CronJobs[sid]; !ok {
		t.Fatalf("旧 cron 计划应注册 cron: %s", sid)
	}

	// 兼容执行路径仍创建子任务
	var sch model.ScheduleTask
	if err := s.DB.Where("sid = ?", sid).First(&sch).Error; err != nil {
		t.Fatalf("load schedule: %v", err)
	}
	s.executeScheduledTask(sch)
	var childCount int64
	s.DB.Model(&model.HotmethodTask{}).Where("master_task_tid = ?", sid).Count(&childCount)
	if childCount != 1 {
		t.Fatalf("cron 兼容子任务数应为 1，实际 %d", childCount)
	}
}

func TestScheduleNextRunIntervalUsesPersistedValue(t *testing.T) {
	s := newTestAPIServer(t)
	now := time.Now()
	next := now.Add(time.Hour)
	if err := s.DB.Create(&model.ScheduleTask{
		SID: "sch-next", Name: "下次运行", TaskKind: "perf_cpu", TargetIP: "127.0.0.1",
		IntervalSeconds: 300, Enabled: true, NextRunAt: &next,
		UID: "owner", UserName: "Owner", CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("seed schedule: %v", err)
	}
	var sch model.ScheduleTask
	if err := s.DB.Where("sid = ?", "sch-next").First(&sch).Error; err != nil {
		t.Fatalf("load schedule: %v", err)
	}
	got := s.scheduleNextRun(sch)
	if got == nil || !got.Truncate(time.Second).Equal(next.Truncate(time.Second)) {
		t.Fatalf("间隔型 next_run 应返回持久化值 %v，实际 %v", next, got)
	}

	// 停用 → nil
	if err := s.DB.Model(&model.ScheduleTask{}).Where("sid = ?", "sch-next").Update("enabled", false).Error; err != nil {
		t.Fatalf("disable: %v", err)
	}
	s.DB.Where("sid = ?", "sch-next").First(&sch)
	if got := s.scheduleNextRun(sch); got != nil {
		t.Fatalf("停用计划 next_run 应为 nil，实际 %v", got)
	}
}
