package server

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
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
	// 阶段三：内建持续剖析块存储指标
	metricContinuousBlocksCreatedTotal     int64
	metricContinuousBlocksReplacedTotal    int64
	metricContinuousBlocksReadTotal        int64
	metricContinuousSourceDeleteRetryTotal int64
	metricContinuousCompactionSkipTotal    int64
	metricContinuousReclaimedBytesTotal    int64
	// 阶段 0：磁盘止血指标
	metricStorageTotalBytes     int64 // gauge：受监控文件系统总字节数
	metricStorageAvailableBytes int64 // gauge：受监控文件系统剩余字节数
	metricStoragePressureLevel  int64 // gauge：0=normal 1=warning 2=critical 3=emergency 4=unknown
	// 存储阶段一：Artifact 生命周期指标
	metricArtifactCleanupDeletedTotal      int64 // counter：清理成功删除的对象数
	metricArtifactCleanupDeletedBytesTotal int64 // counter：清理回收的字节数
	metricArtifactCleanupFailuresTotal     int64 // counter：对象删除失败次数
	metricArtifactReadyBytes               int64 // gauge：ready 状态字节数
	metricArtifactPinnedBytes              int64 // gauge：固定任务下非 deleted 字节数
	metricArtifactExpirationBacklog        int64 // gauge：到期候选（due）数量
	metricArtifactPolicyReconcileBacklog   int64 // gauge：策略版本不一致待重算数量
	// 按 status 计数的 gauge（label: status）
	metricArtifactsByStatusMu sync.Mutex
	metricArtifactsByStatus   = map[string]int64{}
)

// 按来源计数的低磁盘拒收计数（label: source）。
var (
	metricCollectionRejectedLowDiskMu sync.Mutex
	metricCollectionRejectedLowDisk   = map[string]int64{}
)

func incTasksCreated()         { atomic.AddInt64(&metricTasksCreatedTotal, 1) }
func incTaskNotifyFailed()     { atomic.AddInt64(&metricTaskNotifyFailedTotal, 1) }
func incArtifactUploadFailed() { atomic.AddInt64(&metricArtifactUploadFailedTotal, 1) }
func incAnalysisQueued()       { atomic.AddInt64(&metricAnalysisQueuedTotal, 1) }
func incSSEActive()            { atomic.AddInt64(&metricSSEActiveConnections, 1) }
func decSSEActive()            { atomic.AddInt64(&metricSSEActiveConnections, -1) }

func incContinuousBlocksCreated(replaced bool) {
	atomic.AddInt64(&metricContinuousBlocksCreatedTotal, 1)
	if replaced {
		atomic.AddInt64(&metricContinuousBlocksReplacedTotal, 1)
	}
}
func incContinuousBlocksRead()        { atomic.AddInt64(&metricContinuousBlocksReadTotal, 1) }
func incContinuousSourceDeleteRetry() { atomic.AddInt64(&metricContinuousSourceDeleteRetryTotal, 1) }
func incContinuousCompactionSkip()    { atomic.AddInt64(&metricContinuousCompactionSkipTotal, 1) }
func incContinuousReclaimedBytes(bytes int64) {
	atomic.AddInt64(&metricContinuousReclaimedBytesTotal, bytes)
}

func incArtifactCleanupDeleted(_ string) {
	atomic.AddInt64(&metricArtifactCleanupDeletedTotal, 1)
}
func incArtifactCleanupDeletedBytes(bytes int64) {
	atomic.AddInt64(&metricArtifactCleanupDeletedBytesTotal, bytes)
}
func incArtifactCleanupFailures() {
	atomic.AddInt64(&metricArtifactCleanupFailuresTotal, 1)
}

// updateArtifactLifecycleGauges 用最新生命周期统计刷新 gauge 指标。
func updateArtifactLifecycleGauges(stats artifactLifecycleStats) {
	atomic.StoreInt64(&metricArtifactReadyBytes, stats.ReadyBytes)
	atomic.StoreInt64(&metricArtifactPinnedBytes, stats.PinnedBytes)
	atomic.StoreInt64(&metricArtifactExpirationBacklog, stats.DueCount)
	atomic.StoreInt64(&metricArtifactPolicyReconcileBacklog, stats.ReconcileBacklog)
}

// refreshArtifactsByStatusGauge 从 DB 刷新按状态计数的 gauge（在指标输出时兜底）。
func (s *APIServer) refreshArtifactsByStatusGauge() {
	if s == nil || s.DB == nil {
		return
	}
	rows := []struct {
		Status string
		Count  int64
	}{}
	_ = s.DB.Model(&model.Artifact{}).Select("status, count(*) as count").Group("status").Scan(&rows).Error
	metricArtifactsByStatusMu.Lock()
	metricArtifactsByStatus = map[string]int64{}
	for _, r := range rows {
		metricArtifactsByStatus[r.Status] = r.Count
	}
	metricArtifactsByStatusMu.Unlock()
}

// storagePressureLevelNumeric 把等级映射为数值（供 gauge 使用）。
func storagePressureLevelNumeric(level StoragePressureLevel) int64 {
	switch level {
	case StoragePressureNormal:
		return 0
	case StoragePressureWarning:
		return 1
	case StoragePressureCritical:
		return 2
	case StoragePressureEmergency:
		return 3
	default:
		return 4 // unknown
	}
}

// updateStorageMetrics 用最新磁盘快照刷新 gauge 指标。
func updateStorageMetrics(snap StorageDiskSnapshot) {
	atomic.StoreInt64(&metricStorageTotalBytes, int64(snap.TotalBytes))
	atomic.StoreInt64(&metricStorageAvailableBytes, int64(snap.AvailableBytes))
	atomic.StoreInt64(&metricStoragePressureLevel, storagePressureLevelNumeric(snap.Level))
}

// incCollectionRejectedLowDisk 累加"低磁盘拒收"计数（按采集入口来源）。
func incCollectionRejectedLowDisk(source string) {
	metricCollectionRejectedLowDiskMu.Lock()
	metricCollectionRejectedLowDisk[source]++
	metricCollectionRejectedLowDiskMu.Unlock()
}

func snapshotRejectedLowDiskCounts() map[string]int64 {
	metricCollectionRejectedLowDiskMu.Lock()
	defer metricCollectionRejectedLowDiskMu.Unlock()
	out := make(map[string]int64, len(metricCollectionRejectedLowDisk))
	for k, v := range metricCollectionRejectedLowDisk {
		out[k] = v
	}
	return out
}

func currentSSEActive() int64 { return atomic.LoadInt64(&metricSSEActiveConnections) }

func resetMetricsForTest() {
	atomic.StoreInt64(&metricTasksCreatedTotal, 0)
	atomic.StoreInt64(&metricTaskNotifyFailedTotal, 0)
	atomic.StoreInt64(&metricArtifactUploadFailedTotal, 0)
	atomic.StoreInt64(&metricAnalysisQueuedTotal, 0)
	atomic.StoreInt64(&metricSSEActiveConnections, 0)
	atomic.StoreInt64(&metricContinuousBlocksCreatedTotal, 0)
	atomic.StoreInt64(&metricContinuousBlocksReplacedTotal, 0)
	atomic.StoreInt64(&metricContinuousBlocksReadTotal, 0)
	atomic.StoreInt64(&metricContinuousSourceDeleteRetryTotal, 0)
	atomic.StoreInt64(&metricContinuousCompactionSkipTotal, 0)
	atomic.StoreInt64(&metricContinuousReclaimedBytesTotal, 0)
	atomic.StoreInt64(&metricStorageTotalBytes, 0)
	atomic.StoreInt64(&metricStorageAvailableBytes, 0)
	atomic.StoreInt64(&metricStoragePressureLevel, 4) // unknown 兜底
	atomic.StoreInt64(&metricArtifactCleanupDeletedTotal, 0)
	atomic.StoreInt64(&metricArtifactCleanupDeletedBytesTotal, 0)
	atomic.StoreInt64(&metricArtifactCleanupFailuresTotal, 0)
	atomic.StoreInt64(&metricArtifactReadyBytes, 0)
	atomic.StoreInt64(&metricArtifactPinnedBytes, 0)
	atomic.StoreInt64(&metricArtifactExpirationBacklog, 0)
	atomic.StoreInt64(&metricArtifactPolicyReconcileBacklog, 0)
	metricArtifactsByStatusMu.Lock()
	metricArtifactsByStatus = map[string]int64{}
	metricArtifactsByStatusMu.Unlock()
	metricCollectionRejectedLowDiskMu.Lock()
	metricCollectionRejectedLowDisk = map[string]int64{}
	metricCollectionRejectedLowDiskMu.Unlock()
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
	writeMetricHeader(&b, "mini_drop_continuous_blocks_created_total", "counter", "Continuous profile blocks created by compactor.")
	fmt.Fprintf(&b, "mini_drop_continuous_blocks_created_total %d\n", atomic.LoadInt64(&metricContinuousBlocksCreatedTotal))
	writeMetricHeader(&b, "mini_drop_continuous_blocks_replaced_total", "counter", "Continuous profile blocks replaced by a new version (late batch or retention rewrite).")
	fmt.Fprintf(&b, "mini_drop_continuous_blocks_replaced_total %d\n", atomic.LoadInt64(&metricContinuousBlocksReplacedTotal))
	writeMetricHeader(&b, "mini_drop_continuous_blocks_read_total", "counter", "Continuous profile block objects decompressed by queries.")
	fmt.Fprintf(&b, "mini_drop_continuous_blocks_read_total %d\n", atomic.LoadInt64(&metricContinuousBlocksReadTotal))
	writeMetricHeader(&b, "mini_drop_continuous_source_delete_retry_total", "counter", "Failed source minute-object deletions retried by sweep.")
	fmt.Fprintf(&b, "mini_drop_continuous_source_delete_retry_total %d\n", atomic.LoadInt64(&metricContinuousSourceDeleteRetryTotal))
	writeMetricHeader(&b, "mini_drop_continuous_compaction_skip_total", "counter", "Compaction runs skipped (low disk or dependency unavailable).")
	fmt.Fprintf(&b, "mini_drop_continuous_compaction_skip_total %d\n", atomic.LoadInt64(&metricContinuousCompactionSkipTotal))
	writeMetricHeader(&b, "mini_drop_continuous_reclaimed_bytes_total", "counter", "Bytes reclaimed by block/source-object garbage collection.")
	fmt.Fprintf(&b, "mini_drop_continuous_reclaimed_bytes_total %d\n", atomic.LoadInt64(&metricContinuousReclaimedBytesTotal))
	writeMetricHeader(&b, "mini_drop_storage_total_bytes", "gauge", "Total bytes of the monitored storage filesystem.")
	fmt.Fprintf(&b, "mini_drop_storage_total_bytes %d\n", atomic.LoadInt64(&metricStorageTotalBytes))
	writeMetricHeader(&b, "mini_drop_storage_available_bytes", "gauge", "Available bytes of the monitored storage filesystem.")
	fmt.Fprintf(&b, "mini_drop_storage_available_bytes %d\n", atomic.LoadInt64(&metricStorageAvailableBytes))
	writeMetricHeader(&b, "mini_drop_storage_pressure_level", "gauge", "Storage pressure level: 0=normal 1=warning 2=critical 3=emergency 4=unknown.")
	fmt.Fprintf(&b, "mini_drop_storage_pressure_level %d\n", atomic.LoadInt64(&metricStoragePressureLevel))
	writeMetricHeader(&b, "mini_drop_collection_rejected_low_disk_total", "counter", "Collection attempts rejected because of low disk, by entry source.")
	sources := make([]string, 0, len(metricCollectionRejectedLowDisk))
	metricCollectionRejectedLowDiskMu.Lock()
	for source := range metricCollectionRejectedLowDisk {
		sources = append(sources, source)
	}
	metricCollectionRejectedLowDiskMu.Unlock()
	sort.Strings(sources)
	for _, source := range sources {
		count := func() int64 {
			metricCollectionRejectedLowDiskMu.Lock()
			defer metricCollectionRejectedLowDiskMu.Unlock()
			return metricCollectionRejectedLowDisk[source]
		}()
		fmt.Fprintf(&b, "mini_drop_collection_rejected_low_disk_total{source=%q} %d\n", prometheusLabel(source), count)
	}

	if s != nil && s.DB != nil {
		s.refreshArtifactsByStatusGauge()
		s.writeDBMetrics(&b)
	}
	// 存储阶段一：Artifact 生命周期指标
	writeMetricHeader(&b, "mini_drop_artifacts_by_status", "gauge", "Current artifacts grouped by status.")
	metricArtifactsByStatusMu.Lock()
	statuses := make([]string, 0, len(metricArtifactsByStatus))
	for status := range metricArtifactsByStatus {
		statuses = append(statuses, status)
	}
	metricArtifactsByStatusMu.Unlock()
	sort.Strings(statuses)
	for _, status := range statuses {
		metricArtifactsByStatusMu.Lock()
		count := metricArtifactsByStatus[status]
		metricArtifactsByStatusMu.Unlock()
		fmt.Fprintf(&b, "mini_drop_artifacts_by_status{status=%q} %d\n", prometheusLabel(status), count)
	}
	writeMetricHeader(&b, "mini_drop_artifact_ready_bytes", "gauge", "Bytes of artifacts in ready status.")
	fmt.Fprintf(&b, "mini_drop_artifact_ready_bytes %d\n", atomic.LoadInt64(&metricArtifactReadyBytes))
	writeMetricHeader(&b, "mini_drop_artifact_pinned_bytes", "gauge", "Bytes of non-deleted artifacts under pinned tasks.")
	fmt.Fprintf(&b, "mini_drop_artifact_pinned_bytes %d\n", atomic.LoadInt64(&metricArtifactPinnedBytes))
	writeMetricHeader(&b, "mini_drop_artifact_expiration_backlog", "gauge", "Artifacts due for expiration cleanup.")
	fmt.Fprintf(&b, "mini_drop_artifact_expiration_backlog %d\n", atomic.LoadInt64(&metricArtifactExpirationBacklog))
	writeMetricHeader(&b, "mini_drop_artifact_cleanup_deleted_total", "counter", "Artifacts deleted by lifecycle cleanup.")
	fmt.Fprintf(&b, "mini_drop_artifact_cleanup_deleted_total %d\n", atomic.LoadInt64(&metricArtifactCleanupDeletedTotal))
	writeMetricHeader(&b, "mini_drop_artifact_cleanup_deleted_bytes_total", "counter", "Object bytes reclaimed by lifecycle cleanup.")
	fmt.Fprintf(&b, "mini_drop_artifact_cleanup_deleted_bytes_total %d\n", atomic.LoadInt64(&metricArtifactCleanupDeletedBytesTotal))
	writeMetricHeader(&b, "mini_drop_artifact_cleanup_failures_total", "counter", "Artifact object deletion failures.")
	fmt.Fprintf(&b, "mini_drop_artifact_cleanup_failures_total %d\n", atomic.LoadInt64(&metricArtifactCleanupFailuresTotal))
	writeMetricHeader(&b, "mini_drop_artifact_policy_reconcile_backlog", "gauge", "Artifacts awaiting retention policy reconciliation.")
	fmt.Fprintf(&b, "mini_drop_artifact_policy_reconcile_backlog %d\n", atomic.LoadInt64(&metricArtifactPolicyReconcileBacklog))
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
