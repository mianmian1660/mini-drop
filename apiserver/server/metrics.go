package server

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/mini-drop/apiserver/model"
	"github.com/mini-drop/apiserver/util"
)

var (
	metricTasksCreatedTotal         int64
	metricTaskNotifyFailedTotal     int64
	metricArtifactUploadFailedTotal int64
	metricAnalysisQueuedTotal       int64
	metricSSEActiveConnections      int64
)

func incTasksCreated()         { atomic.AddInt64(&metricTasksCreatedTotal, 1) }
func incTaskNotifyFailed()     { atomic.AddInt64(&metricTaskNotifyFailedTotal, 1) }
func incArtifactUploadFailed() { atomic.AddInt64(&metricArtifactUploadFailedTotal, 1) }
func incAnalysisQueued()       { atomic.AddInt64(&metricAnalysisQueuedTotal, 1) }
func incSSEActive()            { atomic.AddInt64(&metricSSEActiveConnections, 1) }
func decSSEActive()            { atomic.AddInt64(&metricSSEActiveConnections, -1) }

func currentSSEActive() int64 { return atomic.LoadInt64(&metricSSEActiveConnections) }

func resetMetricsForTest() {
	atomic.StoreInt64(&metricTasksCreatedTotal, 0)
	atomic.StoreInt64(&metricTaskNotifyFailedTotal, 0)
	atomic.StoreInt64(&metricArtifactUploadFailedTotal, 0)
	atomic.StoreInt64(&metricAnalysisQueuedTotal, 0)
	atomic.StoreInt64(&metricSSEActiveConnections, 0)
}

func (s *APIServer) Metrics(c *gin.Context) {
	var b strings.Builder
	writeMetricHeader(&b, "mini_drop_tasks_created_total", "counter", "Tasks created through the API.")
	fmt.Fprintf(&b, "mini_drop_tasks_created_total %d\n", atomic.LoadInt64(&metricTasksCreatedTotal))
	writeMetricHeader(&b, "mini_drop_task_notify_failed_total", "counter", "Failed task result notifications.")
	fmt.Fprintf(&b, "mini_drop_task_notify_failed_total %d\n", atomic.LoadInt64(&metricTaskNotifyFailedTotal))
	writeMetricHeader(&b, "mini_drop_artifact_upload_failed_total", "counter", "Artifact upload or notify failures.")
	fmt.Fprintf(&b, "mini_drop_artifact_upload_failed_total %d\n", atomic.LoadInt64(&metricArtifactUploadFailedTotal))
	writeMetricHeader(&b, "mini_drop_analysis_queued_total", "counter", "Analysis jobs queued by apiserver.")
	fmt.Fprintf(&b, "mini_drop_analysis_queued_total %d\n", atomic.LoadInt64(&metricAnalysisQueuedTotal))
	writeMetricHeader(&b, "mini_drop_sse_active_connections", "gauge", "Current active SSE connections.")
	fmt.Fprintf(&b, "mini_drop_sse_active_connections %d\n", currentSSEActive())

	if s != nil && s.DB != nil {
		s.writeDBMetrics(&b)
	}
	c.Data(http.StatusOK, "text/plain; version=0.0.4; charset=utf-8", []byte(b.String()))
}

func writeMetricHeader(b *strings.Builder, name string, typ string, help string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func (s *APIServer) writeDBMetrics(b *strings.Builder) {
	writeMetricHeader(b, "mini_drop_tasks_by_status", "gauge", "Current tasks grouped by status.")
	for status, count := range countTasksByStatus(s.DB) {
		fmt.Fprintf(b, "mini_drop_tasks_by_status{status=\"%d\"} %d\n", status, count)
	}

	writeMetricHeader(b, "mini_drop_outbox_by_status", "gauge", "Current outbox records grouped by status.")
	for status, count := range countOutboxByStatus(s.DB) {
		fmt.Fprintf(b, "mini_drop_outbox_by_status{status=\"%s\"} %d\n", prometheusLabel(status), count)
	}

	writeMetricHeader(b, "mini_drop_analysis_jobs_by_status", "gauge", "Current analysis jobs grouped by status.")
	for status, count := range countAnalysisJobsByStatus(s.DB) {
		fmt.Fprintf(b, "mini_drop_analysis_jobs_by_status{status=\"%s\"} %d\n", prometheusLabel(status), count)
	}

	writeMetricHeader(b, "mini_drop_agents_online", "gauge", "Current online agents.")
	fmt.Fprintf(b, "mini_drop_agents_online %d\n", countOnlineAgents(s.DB))
}

func prometheusLabel(raw string) string {
	raw = util.RedactSecret(raw)
	raw = strings.ReplaceAll(raw, "\\", "\\\\")
	raw = strings.ReplaceAll(raw, "\n", "")
	return strings.ReplaceAll(raw, "\"", "\\\"")
}

func countTasksByStatus(db *gorm.DB) map[int]int64 {
	rows := []struct {
		Status int
		Count  int64
	}{}
	_ = db.Model(&model.HotmethodTask{}).Select("status, count(*) as count").Group("status").Scan(&rows).Error
	result := map[int]int64{}
	for _, row := range rows {
		result[row.Status] = row.Count
	}
	return result
}

func countOutboxByStatus(db *gorm.DB) map[string]int64 {
	rows := []struct {
		Status string
		Count  int64
	}{}
	_ = db.Model(&model.Outbox{}).Select("status, count(*) as count").Group("status").Scan(&rows).Error
	result := map[string]int64{}
	for _, row := range rows {
		result[row.Status] = row.Count
	}
	return result
}

func countAnalysisJobsByStatus(db *gorm.DB) map[string]int64 {
	rows := []struct {
		Status string
		Count  int64
	}{}
	_ = db.Model(&model.AnalysisJob{}).Select("status, count(*) as count").Group("status").Scan(&rows).Error
	result := map[string]int64{}
	for _, row := range rows {
		result[row.Status] = row.Count
	}
	return result
}

func countOnlineAgents(db *gorm.DB) int64 {
	var count int64
	_ = db.Model(&model.AgentInfo{}).Where("online = ?", true).Count(&count).Error
	return count
}
