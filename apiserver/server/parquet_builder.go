// ============================================================
// server/parquet_builder.go — 阶段五：v2 块构建与降采样
// ============================================================
// 层级：分钟 JSON staging → Parquet raw（24h）→ Parquet 5m（7d）→
// Parquet 1h（30d）。层级转换遵循"先成功生成并校验下一层，再删除上一层"：
//   - raw 达到 24h：生成 5m，校验后 raw 宽限 15 分钟删除
//   - 5m 达到 7d：生成 1h，校验后 5m 宽限 15 分钟删除
//   - 1h 达到 30d 或触发 4 GiB 配额：删除最老分区
//
// 降采样规则（见文件内各函数）：
//   - CPU：按 bucket、完整 series labels、完整结构化 stack、unit 聚合，
//     value=sum(value)
//   - Gauge：min/max/avg/last；Counter：reset-aware 正向 delta
//   - Histogram：相同 bucket bounds 的 count 求和，P50/P95/P99 从合并桶重算
//   - DB digest：增量字段求和；lock wait：次数、最大等待、代表查询
// ============================================================

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/mini-drop/apiserver/model"
)

// pqSignalResolution 分辨率秒数。
func pqSignalResolutionSeconds(resolution string) int64 {
	switch resolution {
	case model.ContinuousParquetResolution5m:
		return 300
	case model.ContinuousParquetResolution1h:
		return 3600
	default:
		return 60
	}
}

// pqBucketStart 把时间截断到分辨率桶。
func pqBucketStart(ts time.Time, resolution string) time.Time {
	switch resolution {
	case model.ContinuousParquetResolution5m:
		return ts.UTC().Truncate(5 * time.Minute)
	case model.ContinuousParquetResolution1h:
		return ts.UTC().Truncate(time.Hour)
	default:
		return ts.UTC().Truncate(time.Minute)
	}
}

// pqHourWindow 小时桶 [start, end)。
func pqHourWindow(ts time.Time) (time.Time, time.Time) {
	start := ts.UTC().Truncate(time.Hour)
	return start, start.Add(time.Hour)
}

// pqHashGroupKey 组键分隔符（避免字段拼接歧义）。
const pqGroupSep = "\x00"

// ---------------------------------------------------------------------------
// v1 payload → v2 rows
// ---------------------------------------------------------------------------

// pqCollectWindowRows 把一个 window 的四种信号转成 v2 行。
func pqCollectWindowRows(s *APIServer, window model.ProfileWindow, batch *continuousStoredBatch, in *ContinuousWindowIngest) pqSignalRows {
	var rows pqSignalRows
	ts := window.WindowStart.UnixMilli()
	labels := sanitizeContinuousLabels(windowLabelsMap(window.Labels, in.Labels))
	sessionSID := window.SessionSID

	switch window.SignalType {
	case "cpu_profile", "cpu", "python_memory", "memory":
		for _, sample := range continuousProfileSamplesForIngest(window, in) {
			frames := normalizedSampleFrames(sample)
			row := pqCPURow{
				Timestamp:      ts,
				SessionSID:     sessionSID,
				Service:        firstNonEmpty(inServiceName(s, sessionSID), "hotmethod"),
				Agent:          firstNonEmpty(inLabelsString(in, "agent_id"), ""),
				PID:            int32(sample.PID),
				ProcessStartMs: sample.ProcessStartMs,
				Comm:           sample.Comm,
				Exe:            sample.Exe,
				Backend:        firstNonEmpty(sample.Backend, window.Backend),
				Runtime:        sample.Runtime,
				Labels:         labels,
				Frames:         framesToParquet(frames),
				Value:          firstNonZeroUint64(sample.Count, 1),
				Unit:           "samples",
				ProfileType:    window.SignalType,
			}
			rows.CPU = append(rows.CPU, row)
		}
	case "metrics", "python_rss":
		for _, metric := range in.Metrics {
			kind, registered := pqMetricKindFor(metric.Metric)
			if !registered {
				// 未登记指标按 gauge 处理并记录告警（避免误算 counter delta）
				incParquetMetricUnknownKind(metric.Metric)
			}
			row := pqMetricRow{
				Timestamp:      ts,
				SessionSID:     sessionSID,
				PID:            int32(metric.PID),
				ProcessStartMs: metric.ProcessStartMs,
				Comm:           metric.Comm,
				Exe:            metric.Exe,
				Runtime:        metric.Runtime,
				Metric:         metric.Metric,
				MetricKind:     kind,
				Value:          metric.Value,
				Min:            metric.Value,
				Max:            metric.Value,
				Sum:            metric.Value,
				Count:          1,
				Last:           metric.Value,
				Unit:           metric.Unit,
				Labels:         sanitizeContinuousLabels(metric.Labels),
			}
			rows.Metrics = append(rows.Metrics, row)
		}
	case "io_latency", "io_syscall_latency", "sched_latency":
		for _, histogram := range in.Histograms {
			for _, bucket := range histogram.Buckets {
				row := pqHistogramRow{
					Timestamp:   ts,
					SessionSID:  sessionSID,
					SignalType:  window.SignalType,
					Backend:     firstNonEmpty(histogram.Backend, window.Backend),
					Unit:        histogram.Unit,
					BucketLow:   bucket.Low,
					BucketHigh:  bucket.High,
					Count:       bucket.Count,
					EventCount:  histogram.EventCount,
					Min:         histogram.Summary.Min,
					Max:         histogram.Summary.Max,
					P50:         histogram.Summary.P50,
					P95:         histogram.Summary.P95,
					P99:         histogram.Summary.P99,
					Unavailable: histogram.Unavailable,
					Reason:      histogram.Reason,
					Labels:      sanitizeContinuousLabels(histogram.Labels),
				}
				rows.Histogram = append(rows.Histogram, row)
			}
		}
	case "db", "db_snapshot":
		for _, snapshot := range in.DBSnapshots {
			row := pqDBRow{
				Timestamp:             ts,
				SessionSID:            sessionSID,
				Kind:                  snapshot.Kind,
				Instance:              snapshot.InstanceLabel,
				SchemaName:            snapshot.SchemaName,
				DigestText:            snapshot.DigestText,
				CallCount:             snapshot.CallCount,
				TotalLatencyUs:        snapshot.TotalLatencyUs,
				RowsExaminedTotal:     snapshot.RowsExaminedTotal,
				WaitingPID:            snapshot.WaitingPID,
				WaitingQuery:          snapshot.WaitingQuery,
				BlockingPID:           snapshot.BlockingPID,
				BlockingQuery:         snapshot.BlockingQuery,
				WaitSeconds:           snapshot.WaitSeconds,
				LockedTable:           snapshot.LockedTable,
				OccurrenceCount:       1,
				MaxWaitSeconds:        snapshot.WaitSeconds,
				MaxWaitRepresentative: snapshot.WaitingQuery,
				Labels:                sanitizeContinuousLabels(in.Labels),
			}
			rows.DB = append(rows.DB, row)
		}
	}
	return rows
}

// framesToParquet ContinuousStackFrame → pqCPUFrame。
func framesToParquet(frames []ContinuousStackFrame) []pqCPUFrame {
	if len(frames) == 0 {
		return nil
	}
	out := make([]pqCPUFrame, 0, len(frames))
	for _, frame := range frames {
		out = append(out, pqCPUFrame{
			Function:         frame.Function,
			Raw:              frame.Raw,
			File:             frame.File,
			Line:             frame.Line,
			Address:          frame.Address,
			MappingFile:      frame.MappingFile,
			BuildID:          frame.BuildID,
			NormalizedOffset: frame.NormalizedOffset,
			Resolved:         frame.Resolved,
		})
	}
	return out
}

// continuousProfileSamplesForIngest 返回 window 内应计入 CPU/profile 的样本
// （samples + profiles 合并，按 (profile_id) 去重）。
func continuousProfileSamplesForIngest(window model.ProfileWindow, in *ContinuousWindowIngest) []ContinuousStackSample {
	out := make([]ContinuousStackSample, 0, len(in.Samples)+len(in.Profiles))
	out = append(out, in.Samples...)
	seen := map[string]bool{}
	for _, profile := range in.Profiles {
		if profile.ProfileID != "" {
			if seen[profile.ProfileID] {
				continue
			}
			seen[profile.ProfileID] = true
		}
		out = append(out, profile.Samples...)
	}
	return out
}

// windowLabelsMap 合并 window 行 labels（DB JSONB）与请求内 labels。
func windowLabelsMap(rowLabels []byte, ingestLabels map[string]interface{}) map[string]interface{} {
	if len(rowLabels) > 0 {
		var parsed map[string]interface{}
		if err := json.Unmarshal(rowLabels, &parsed); err == nil && len(parsed) > 0 {
			return parsed
		}
	}
	return ingestLabels
}

// inLabelsString 从 window labels 取字符串值。
func inLabelsString(in *ContinuousWindowIngest, key string) string {
	if in == nil || len(in.Labels) == 0 {
		return ""
	}
	value, _ := in.Labels[key].(string)
	return value
}

// inServiceName 返回 session service 名（带小缓存避免每 window 查库）。
var (
	inServiceNameOnce sync.Map // sid → string
	inServiceName     = func(s *APIServer, sessionSID string) string {
		if cached, ok := inServiceNameOnce.Load(sessionSID); ok {
			name, _ := cached.(string)
			return name
		}
		var session model.ContinuousSession
		if err := s.DB.Where("sid = ?", sessionSID).First(&session).Error; err != nil {
			return ""
		}
		name := firstNonEmpty(session.ServiceName, "hotmethod")
		inServiceNameOnce.Store(sessionSID, name)
		return name
	}
)

// ---------------------------------------------------------------------------
// raw 块构建（staging + v1 兼容读取 → Parquet raw）
// ---------------------------------------------------------------------------

// pqBuildRawHour 为指定 UTC 小时构建四类信号的 raw 块。
// 返回 (是否构建了任何块, 错误)。
func (s *APIServer) pqBuildRawHour(ctx context.Context, tenant string, hourStart time.Time) (bool, error) {
	hourStart = hourStart.UTC().Truncate(time.Hour)
	hourEnd := hourStart.Add(time.Hour)

	// 磁盘/配额前置检查
	if ok, reason := s.maintenanceSpaceOK(0); !ok {
		incParquetBuildSkip("low_disk:" + reason)
		return false, nil
	}
	if ok, _ := s.continuousQuotaOK(ctx); !ok {
		incParquetBuildSkip("quota_exceeded")
		// 配额超限：先回收（staging/superseded → 最老 1h）
		s.pqReclaimForQuota(ctx)
		return false, nil
	}

	// 查询该小时窗口（覆盖所有 session，v2 块跨 session）
	var windows []model.ProfileWindow
	if err := s.DB.WithContext(ctx).
		Where("window_start >= ? AND window_start < ?", hourStart, hourEnd).
		Order("window_start ASC").
		Limit(continuousMaxWindowCount*4 + 1).
		Find(&windows).Error; err != nil {
		return false, err
	}
	if len(windows) == 0 {
		return false, nil
	}
	if len(windows) > continuousMaxWindowCount*4 {
		return false, fmt.Errorf("v2 builder: 小时窗口数超过安全上限: %d", len(windows))
	}
	if !s.StorageConnected() {
		return false, errProfileUnavailable
	}

	// 按 object_key 分组加载 payload（一个块对象只解压一次）
	byObject := map[string][]model.ProfileWindow{}
	objectOrder := []string{}
	for _, window := range windows {
		if window.ObjectKey == "" {
			continue
		}
		if _, ok := byObject[window.ObjectKey]; !ok {
			objectOrder = append(objectOrder, window.ObjectKey)
		}
		byObject[window.ObjectKey] = append(byObject[window.ObjectKey], window)
	}

	var cpuRows []pqCPURow
	var metricRows []pqMetricRow
	var histRows []pqHistogramRow
	var dbRows []pqDBRow
	// 阶段六：per-signal lineage。members[signal][batchBID] 只登记该信号
	// 实际消费的 batch；禁止共享全部 batch member。
	members := map[string]map[string]*pqSignalMemberAcc{}
	// consumed[signal][session|start] 记录已消费的源窗口（完整性对账用）。
	consumed := map[string]map[string]bool{}
	sessionSet := map[string]bool{}
	processSet := map[string]bool{}

	for _, objectKey := range objectOrder {
		batches, err := s.loadContinuousBatches(ctx, objectKey)
		if err != nil {
			return false, fmt.Errorf("v2 builder: 加载源对象 %s 失败: %w", objectKey, err)
		}
		batchByID := continuousBatchIndex(batches)
		// 阶段六修正：按 (batch, signal) 去重而非按 batch——同一 batch 的
		// window 可能混合多种信号，若只处理第一个 DB 行（单一信号），其它
		// 信号数据会被整批丢弃。每个 (batch, signal) 处理一次即可覆盖该
		// batch 内该信号的全部 payload 窗口，且避免同信号多行双计。
		seenSignal := map[string]bool{}
		for _, row := range byObject[objectKey] {
			signal := pqLedgerSignalForWindow(row.SignalType)
			if signal == "" {
				continue
			}
			groupKey := row.BatchBID + "|" + signal
			if seenSignal[groupKey] {
				continue
			}
			seenSignal[groupKey] = true
			batch, rowKey, ok := continuousResolveBatch(row, batches, batchByID)
			if !ok {
				return false, fmt.Errorf("v2 builder: 无法解析源窗口 id=%d batch=%s", row.ID, row.BatchBID)
			}
			_ = rowKey
			sessionSet[row.SessionSID] = true
			if batch == nil {
				continue
			}
			for _, in := range batch.Windows {
				if !windowOverlaps(in.WindowStart, in.WindowEnd, hourStart, hourEnd) {
					continue
				}
				rows := pqCollectWindowRows(s, row, batch, &in)
				switch signal {
				case model.ContinuousParquetSignalCPU:
					cpuRows = append(cpuRows, rows.CPU...)
					for _, sample := range rows.CPU {
						processSet[strconv.Itoa(int(sample.PID))+"|"+strconv.FormatInt(sample.ProcessStartMs, 10)] = true
					}
				case model.ContinuousParquetSignalMetrics:
					metricRows = append(metricRows, rows.Metrics...)
				case model.ContinuousParquetSignalHistogram:
					histRows = append(histRows, rows.Histogram...)
				case model.ContinuousParquetSignalDB:
					dbRows = append(dbRows, rows.DB...)
				}
				pqAccumulateSignalMembers(members, signal, row.SessionSID, batch.BatchID,
					rowsForSignal(rows, signal), in.WindowStart, in.WindowEnd)
				if consumed[signal] == nil {
					consumed[signal] = map[string]bool{}
				}
				consumed[signal][row.SessionSID+"|"+strconv.FormatInt(in.WindowStart.UnixMilli(), 10)] = true
			}
		}
	}

	// 阶段六：按信号窗口完整性对账——每个可解析源窗口（v1 可查询的）都必须被
	// 消费进对应信号的 raw 块。漏消费（batch 缺失/解析跳过）→ 块失败（fail-closed），
	// 绝不静默丢失。样本总量不做跨语义比较（v1 sample_count 与 v2 行计数口径
	// 不同：空 samples 回退 agent 计数/metrics 只数 rss/histogram 数 EventCount）。
	for _, signal := range []string{
		model.ContinuousParquetSignalCPU,
		model.ContinuousParquetSignalMetrics,
		model.ContinuousParquetSignalHistogram,
		model.ContinuousParquetSignalDB,
	} {
		types := pqV1SignalTypesFor(signal)
		for _, w := range windows {
			if w.ObjectKey == "" {
				continue // v1 查询同样跳过空对象窗口
			}
			mapped := ""
			for _, t := range types {
				if w.SignalType == t {
					mapped = signal
					break
				}
			}
			if mapped == "" {
				continue
			}
			key := w.SessionSID + "|" + strconv.FormatInt(w.WindowStart.UnixMilli(), 10)
			if !consumed[signal][key] {
				return false, fmt.Errorf("v2 shadow 完整性: 信号 %s 遗漏源窗口 session=%s start=%s",
					signal, w.SessionSID, w.WindowStart.UTC().Format(time.RFC3339))
			}
		}
	}

	// 按信号拆 part 写入
	builtAny := false
	signalRows := []struct {
		signal  string
		cpu     []pqCPURow
		metric  []pqMetricRow
		hist    []pqHistogramRow
		db      []pqDBRow
		members []model.ContinuousParquetBlockMember
	}{
		{signal: model.ContinuousParquetSignalCPU, cpu: cpuRows, members: pqMemberRowsFor(members[model.ContinuousParquetSignalCPU])},
		{signal: model.ContinuousParquetSignalMetrics, metric: metricRows, members: pqMemberRowsFor(members[model.ContinuousParquetSignalMetrics])},
		{signal: model.ContinuousParquetSignalHistogram, hist: histRows, members: pqMemberRowsFor(members[model.ContinuousParquetSignalHistogram])},
		{signal: model.ContinuousParquetSignalDB, db: dbRows, members: pqMemberRowsFor(members[model.ContinuousParquetSignalDB])},
	}
	for _, group := range signalRows {
		total := len(group.cpu) + len(group.metric) + len(group.hist) + len(group.db)
		if total == 0 {
			continue
		}
		ok, err := s.pqWriteSignalBlock(ctx, tenant, hourStart, hourEnd,
			group.signal, model.ContinuousParquetResolutionRaw, "",
			group.cpu, group.metric, group.hist, group.db,
			sessionSet, processSet, group.members)
		if err != nil {
			s.Logger.Warn("v2 builder: raw 块构建失败",
				zap.String("signal", group.signal), zap.Time("hour", hourStart), zap.Error(err))
			continue
		}
		builtAny = builtAny || ok
	}
	return builtAny, nil
}

// rowsForSignal 返回信号行集合中属于指定信号的子集（member 统计用）。
func rowsForSignal(rows pqSignalRows, signal string) interface{} {
	switch signal {
	case model.ContinuousParquetSignalCPU:
		return rows.CPU
	case model.ContinuousParquetSignalMetrics:
		return rows.Metrics
	case model.ContinuousParquetSignalHistogram:
		return rows.Histogram
	case model.ContinuousParquetSignalDB:
		return rows.DB
	default:
		return nil
	}
}

// pqSignalMemberAcc 单个 batch 对某信号块的贡献统计（阶段六 member 富化）。
type pqSignalMemberAcc struct {
	SessionSID  string
	StartTime   time.Time
	EndTime     time.Time
	SampleCount uint64
	ValueTotal  uint64
	RowCount    int64
}

// pqAccumulateSignalMembers 把一组信号行累加进 per-signal member 统计。
func pqAccumulateSignalMembers(acc map[string]map[string]*pqSignalMemberAcc, signal, sessionSID, bid string,
	rows interface{}, start, end time.Time) {
	total := 0
	var sampleCount, valueTotal uint64
	switch typed := rows.(type) {
	case []pqCPURow:
		total = len(typed)
		for i := range typed {
			sampleCount = addContinuousCount(sampleCount, typed[i].Value)
			valueTotal = addContinuousCount(valueTotal, typed[i].Value)
		}
	case []pqMetricRow:
		total = len(typed)
		for i := range typed {
			sampleCount = addContinuousCount(sampleCount, typed[i].Count)
			valueTotal = addContinuousCount(valueTotal, typed[i].Value)
		}
	case []pqHistogramRow:
		total = len(typed)
		for i := range typed {
			sampleCount = addContinuousCount(sampleCount, typed[i].Count)
			valueTotal = addContinuousCount(valueTotal, typed[i].Count)
		}
	case []pqDBRow:
		total = len(typed)
		for i := range typed {
			count := typed[i].CallCount
			if count == 0 {
				count = typed[i].OccurrenceCount
			}
			sampleCount = addContinuousCount(sampleCount, count)
			valueTotal = addContinuousCount(valueTotal, count)
		}
	default:
		return
	}
	if total == 0 || bid == "" {
		return
	}
	if acc[signal] == nil {
		acc[signal] = map[string]*pqSignalMemberAcc{}
	}
	item := acc[signal][bid]
	if item == nil {
		item = &pqSignalMemberAcc{SessionSID: sessionSID, StartTime: start, EndTime: end}
		acc[signal][bid] = item
	}
	if start.Before(item.StartTime) {
		item.StartTime = start
	}
	if end.After(item.EndTime) {
		item.EndTime = end
	}
	item.SampleCount = addContinuousCount(item.SampleCount, sampleCount)
	item.ValueTotal = addContinuousCount(item.ValueTotal, valueTotal)
	item.RowCount += int64(total)
}

// pqMemberRowsFor 把 per-signal member 统计转成账本行（稳定顺序）。
func pqMemberRowsFor(acc map[string]*pqSignalMemberAcc) []model.ContinuousParquetBlockMember {
	if len(acc) == 0 {
		return nil
	}
	bids := make([]string, 0, len(acc))
	for bid := range acc {
		bids = append(bids, bid)
	}
	sort.Strings(bids)
	out := make([]model.ContinuousParquetBlockMember, 0, len(bids))
	for _, bid := range bids {
		item := acc[bid]
		out = append(out, model.ContinuousParquetBlockMember{
			SourceKind:  "batch",
			SourceRef:   bid,
			SessionSID:  item.SessionSID,
			StartTime:   item.StartTime,
			EndTime:     item.EndTime,
			SampleCount: item.SampleCount,
			ValueTotal:  item.ValueTotal,
			RowCount:    item.RowCount,
		})
	}
	return out
}

// pqWriteSignalBlock 构建并登记单个 (signal, resolution) 块。
// members：raw 为 batch lineage（per-signal）；降采样为来源块 lineage。
func (s *APIServer) pqWriteSignalBlock(ctx context.Context, tenant string, hourStart, hourEnd time.Time,
	signalType, resolution, sourceBlockID string,
	cpuRows []pqCPURow, metricRows []pqMetricRow, histRows []pqHistogramRow, dbRows []pqDBRow,
	sessionSet map[string]bool, processSet map[string]bool, members []model.ContinuousParquetBlockMember) (bool, error) {

	key := pqBlockKey{Tenant: tenant, BucketStart: hourStart, SignalType: signalType, Resolution: resolution}

	// 单飞锁
	lockKey := "cblk|pqv2|" + tenant + "|" + signalType + "|" + resolution + "|" + hourStart.UTC().Format(time.RFC3339)
	release, err := s.pqLockPartition(ctx, lockKey)
	if err != nil {
		return false, err
	}
	defer release()

	blockID := pqNewBlockID()

	// 版本 = 旧 active 版本 + 1（无旧块则 1）
	version := 1
	if old, err := s.pqFindActiveBlock(ctx, key); err == nil && old != nil {
		version = old.Version + 1
	}

	if _, err := s.pqCreateBuildingBlock(ctx, key, blockID, hourEnd, version); err != nil {
		return false, fmt.Errorf("登记 building 块失败: %w", err)
	}
	if sourceBlockID != "" {
		if err := s.DB.WithContext(ctx).Model(&model.ContinuousParquetBlock{}).
			Where("block_id = ? AND status = ?", blockID, model.ContinuousParquetStatusBuilding).
			Update("source_block_id", sourceBlockID).Error; err != nil {
			_ = s.pqMarkBlockFailed(ctx, blockID, "source_lineage_failed")
			return false, fmt.Errorf("登记来源块失败: %w", err)
		}
	}

	results, stats, err := s.pqWriteAndVerifyParts(ctx, tenant, hourStart, hourEnd,
		signalType, resolution, blockID, cpuRows, metricRows, histRows, dbRows)
	if err != nil {
		_ = s.pqMarkBlockFailed(ctx, blockID, "build_failed")
		return false, err
	}
	expected := pqStatsForSignalRows(signalType, cpuRows, metricRows, histRows, dbRows)
	if stats.RowCount != expected.RowCount || stats.SampleTotal != expected.SampleTotal || stats.ValueTotal != expected.ValueTotal {
		_ = s.pqMarkBlockFailed(ctx, blockID, "source_stats_mismatch")
		return false, fmt.Errorf("v2 源/Parquet 统计不一致 signal=%s rows=%d/%d samples=%d/%d values=%d/%d",
			signalType, stats.RowCount, expected.RowCount, stats.SampleTotal, expected.SampleTotal,
			stats.ValueTotal, expected.ValueTotal)
	}
	// 阶段六：raw 块源数据对账已在 pqBuildRawHour 完成（按信号窗口完整性，
	// fail-closed：任何可解析源窗口未被消费即整体失败）。这里只做对象回读
	// 校验 + 源行统计校验（上方），通过后 reconcile_status=passed；
	// 5m/1h 由来源 raw 链保证（登记时继承 reconcile 状态）。

	// 汇总统计（多 part 聚合）
	for _, result := range results {
		stats.BytesTotal += result.SizeBytes
	}
	stats.SessionCount = len(sessionSet)
	stats.ProcessCount = len(processSet)

	// 登记 active（单事务：退役旧 active → 插入新 active → files → members）。
	// reconcile_status：raw 已在此处通过源对账 → passed；5m/1h 继承来源块状态
	// （来源链未对账通过则不能进查询/GC）。
	reconcileStatus := model.ContinuousParquetReconcilePassed
	if resolution != model.ContinuousParquetResolutionRaw && sourceBlockID != "" {
		var src model.ContinuousParquetBlock
		if err := s.DB.WithContext(ctx).Where("block_id = ?", sourceBlockID).First(&src).Error; err == nil {
			reconcileStatus = src.ReconcileStatus
			if reconcileStatus == "" {
				reconcileStatus = model.ContinuousParquetReconcilePending
			}
		}
	}
	if err := s.pqRegisterActiveBlockMulti(ctx, key, blockID, hourEnd, version, results, stats, members, reconcileStatus); err != nil {
		_ = s.pqMarkBlockFailed(ctx, blockID, "register_failed")
		// 登记失败不删对象（由 sweep 按 block_id 前缀回收）
		return false, err
	}

	s.Logger.Info("parquet v2 块已登记",
		zap.String("signal", signalType), zap.String("resolution", resolution),
		zap.String("block_id", blockID), zap.Time("bucket_start", hourStart),
		zap.Int("version", version), zap.Int64("row_count", stats.RowCount),
		zap.Int64("bytes", stats.BytesTotal), zap.Int("parts", len(results)))
	return true, nil
}

// pqRegisterActiveBlockMulti 多 part 版登记（扩展单 part 版）：把 building
// 行更新为 active，并登记全部 part 文件与 members。reconcileStatus 写入
// reconcile_status（raw 对账通过为 passed；降采样继承来源链）。
func (s *APIServer) pqRegisterActiveBlockMulti(ctx context.Context, key pqBlockKey, blockID string, bucketEnd time.Time,
	version int, results []parquetWriteResult, stats pqBlockStats, members []model.ContinuousParquetBlockMember, reconcileStatus string) error {
	now := time.Now()
	if reconcileStatus == "" {
		reconcileStatus = model.ContinuousParquetReconcilePassed
	}
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var oldBlock model.ContinuousParquetBlock
		oldFound := false
		if err := tx.Where("tenant = ? AND bucket_start = ? AND signal_type = ? AND resolution = ? AND status = ?",
			key.Tenant, key.BucketStart, key.SignalType, key.Resolution, model.ContinuousParquetStatusActive).
			First(&oldBlock).Error; err == nil {
			oldFound = true
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if oldFound {
			result := tx.Model(&model.ContinuousParquetBlock{}).
				Where("block_id = ? AND status = ?", oldBlock.BlockID, model.ContinuousParquetStatusActive).
				Updates(map[string]interface{}{
					"status": model.ContinuousParquetStatusSuperseded, "superseded_at": now,
					"replaced_by": blockID, "updated_at": now,
				})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return fmt.Errorf("旧 active v2 块 %s 状态已变更", oldBlock.BlockID)
			}
		}
		boundaries, err := marshalRowGroupBoundaries(pqMergeBoundaries(results))
		if err != nil {
			return err
		}
		updates := map[string]interface{}{
			"bucket_end":           bucketEnd,
			"status":               model.ContinuousParquetStatusActive,
			"validation":           model.ContinuousParquetValidationPassed,
			"reconcile_status":     reconcileStatus,
			"reconciled_at":        now,
			"reconcile_error":      "",
			"member_count":         len(members),
			"row_count":            stats.RowCount,
			"value_total":          stats.ValueTotal,
			"sample_total":         stats.SampleTotal,
			"session_count":        stats.SessionCount,
			"process_count":        stats.ProcessCount,
			"bytes_total":          stats.BytesTotal,
			"first_row_time":       stats.FirstRowTime,
			"last_row_time":        stats.LastRowTime,
			"row_group_boundaries": boundaries,
			"updated_at":           now,
		}
		rowResult := tx.Model(&model.ContinuousParquetBlock{}).
			Where("block_id = ? AND status = ?", blockID, model.ContinuousParquetStatusBuilding).
			Updates(updates)
		if rowResult.Error != nil {
			return rowResult.Error
		}
		if rowResult.RowsAffected != 1 {
			return fmt.Errorf("building 块 %s 状态已变更，无法提升为 active", blockID)
		}
		for i, result := range results {
			file := model.ContinuousParquetBlockFile{
				BlockID: blockID, PartIndex: i, ObjectKey: result.ObjectKey,
				SizeBytes: result.SizeBytes, SHA256: result.SHA256,
				RowGroupCount: result.RowGroupCount, RowCount: result.RowCount,
				CreatedAt: now,
			}
			if err := tx.Create(&file).Error; err != nil {
				return err
			}
		}
		for i := range members {
			members[i].BlockID = blockID
			if members[i].CreatedAt.IsZero() {
				members[i].CreatedAt = now
			}
			if err := tx.Create(&members[i]).Error; err != nil {
				return err
			}
		}
		// 阶段六：raw 块激活时在同一事务内按 (session, signal, hour) 重建
		// 覆盖区间（幂等替换相交范围；失败整体回滚，块保持 building 由下轮
		// 重试）。降采样块不重建（segment 独立于 raw Block 生命周期）。
		if key.Resolution == model.ContinuousParquetResolutionRaw {
			if err := s.pqRebuildCoverageSegmentsTx(tx, key.Tenant, key.BucketStart, key.SignalType, blockID, version); err != nil {
				return fmt.Errorf("重建 coverage segments 失败: %w", err)
			}
		}
		return nil
	})
}

// pqMergeBoundaries 合并多 part 的 row group 边界（顺序拼接）。
func pqMergeBoundaries(results []parquetWriteResult) []pqRowGroupBoundary {
	out := []pqRowGroupBoundary{}
	for _, result := range results {
		out = append(out, result.RowGroupBoundary...)
	}
	return out
}

// pqWriteAndVerifyParts 按 max_part_bytes 拆 part 写入并校验。
// 返回 (results, 汇总统计, 错误)。
func (s *APIServer) pqWriteAndVerifyParts(ctx context.Context, tenant string, hourStart, hourEnd time.Time,
	signalType, resolution, blockID string,
	cpuRows []pqCPURow, metricRows []pqMetricRow, histRows []pqHistogramRow, dbRows []pqDBRow) ([]parquetWriteResult, pqBlockStats, error) {
	cfg := s.Config.ContinuousParquet
	maxPartBytes := cfg.MaxPartBytes
	if maxPartBytes <= 0 {
		maxPartBytes = 128 << 20
	}

	// 估算平均行字节（用于 row group 划分）
	switch signalType {
	case model.ContinuousParquetSignalCPU:
		if len(cpuRows) > 0 {
			pqAvgRowBytesEstimate.Store(estimateRowBytes(cpuRows[0]))
		}
	case model.ContinuousParquetSignalMetrics:
		if len(metricRows) > 0 {
			pqAvgRowBytesEstimate.Store(estimateRowBytes(metricRows[0]))
		}
	case model.ContinuousParquetSignalHistogram:
		if len(histRows) > 0 {
			pqAvgRowBytesEstimate.Store(estimateRowBytes(histRows[0]))
		}
	case model.ContinuousParquetSignalDB:
		if len(dbRows) > 0 {
			pqAvgRowBytesEstimate.Store(estimateRowBytes(dbRows[0]))
		}
	}

	// 按信号拆批：每个 part ≤ maxPartBytes（用估算行数切分）
	var results []parquetWriteResult
	var stats pqBlockStats
	partIndex := 0

	writePart := func(rows interface{}) error {
		var result parquetWriteResult
		var err error
		switch typed := rows.(type) {
		case []pqCPURow:
			result, err = writeParquetPartGeneric(s, ctx, parquetObjectKeyV2(tenant, hourStart, signalType, resolution, blockID, partIndex), typed)
		case []pqMetricRow:
			result, err = writeParquetPartGeneric(s, ctx, parquetObjectKeyV2(tenant, hourStart, signalType, resolution, blockID, partIndex), typed)
		case []pqHistogramRow:
			result, err = writeParquetPartGeneric(s, ctx, parquetObjectKeyV2(tenant, hourStart, signalType, resolution, blockID, partIndex), typed)
		case []pqDBRow:
			result, err = writeParquetPartGeneric(s, ctx, parquetObjectKeyV2(tenant, hourStart, signalType, resolution, blockID, partIndex), typed)
		}
		if err != nil {
			return err
		}
		results = append(results, result)
		stats.RowCount += result.RowCount
		partIndex++
		return nil
	}

	// cut 把 total 行切分成 split 个尽量均匀的段（按行数近似字节）。
	cut := func(total int, split int) []int {
		if total <= 0 || split <= 1 {
			return []int{total}
		}
		out := make([]int, 0, split)
		base := total / split
		remainder := total % split
		for i := 0; i < split; i++ {
			size := base
			if i < remainder {
				size++
			}
			out = append(out, size)
		}
		return out
	}
	splitCount := func(total int) int {
		if total == 0 {
			return 0
		}
		count := (int64(total)*pqAvgRowBytesEstimate.Load())/maxPartBytes + 1
		if count < 1 {
			count = 1
		}
		return int(count)
	}

	switch signalType {
	case model.ContinuousParquetSignalCPU:
		start := 0
		for _, size := range cut(len(cpuRows), splitCount(len(cpuRows))) {
			if size <= 0 {
				continue
			}
			if err := writePart(cpuRows[start : start+size]); err != nil {
				return nil, stats, err
			}
			start += size
		}
		for i := range cpuRows {
			stats.SampleTotal = addContinuousCount(stats.SampleTotal, cpuRows[i].Value)
			stats.ValueTotal = addContinuousCount(stats.ValueTotal, cpuRows[i].Value)
		}
		stats.FirstRowTime, stats.LastRowTime = cpuFirstLast(cpuRows)
	case model.ContinuousParquetSignalMetrics:
		start := 0
		for _, size := range cut(len(metricRows), splitCount(len(metricRows))) {
			if size <= 0 {
				continue
			}
			if err := writePart(metricRows[start : start+size]); err != nil {
				return nil, stats, err
			}
			start += size
		}
		for i := range metricRows {
			stats.ValueTotal = addContinuousCount(stats.ValueTotal, metricRows[i].Value)
			stats.SampleTotal = addContinuousCount(stats.SampleTotal, metricRows[i].Count)
		}
	case model.ContinuousParquetSignalHistogram:
		start := 0
		for _, size := range cut(len(histRows), splitCount(len(histRows))) {
			if size <= 0 {
				continue
			}
			if err := writePart(histRows[start : start+size]); err != nil {
				return nil, stats, err
			}
			start += size
		}
		for i := range histRows {
			stats.SampleTotal = addContinuousCount(stats.SampleTotal, histRows[i].Count)
			stats.ValueTotal = addContinuousCount(stats.ValueTotal, histRows[i].Count)
		}
	case model.ContinuousParquetSignalDB:
		start := 0
		for _, size := range cut(len(dbRows), splitCount(len(dbRows))) {
			if size <= 0 {
				continue
			}
			if err := writePart(dbRows[start : start+size]); err != nil {
				return nil, stats, err
			}
			start += size
		}
		for i := range dbRows {
			count := dbRows[i].CallCount
			if count == 0 {
				count = dbRows[i].OccurrenceCount
			}
			stats.SampleTotal = addContinuousCount(stats.SampleTotal, count)
			stats.ValueTotal = addContinuousCount(stats.ValueTotal, count)
		}
	}

	return results, stats, nil
}

// pqStatsForSignalRows 从 v1 解码后的源行独立计算登记统计。写入器返回的
// footer/part 统计必须与它完全一致，之后才允许把 building 提升为 active。
func pqStatsForSignalRows(signalType string, cpuRows []pqCPURow, metricRows []pqMetricRow, histRows []pqHistogramRow, dbRows []pqDBRow) pqBlockStats {
	stats := pqBlockStats{}
	switch signalType {
	case model.ContinuousParquetSignalCPU:
		stats.RowCount = int64(len(cpuRows))
		for _, row := range cpuRows {
			stats.SampleTotal = addContinuousCount(stats.SampleTotal, row.Value)
			stats.ValueTotal = addContinuousCount(stats.ValueTotal, row.Value)
		}
	case model.ContinuousParquetSignalMetrics:
		stats.RowCount = int64(len(metricRows))
		for _, row := range metricRows {
			stats.SampleTotal = addContinuousCount(stats.SampleTotal, row.Count)
			stats.ValueTotal = addContinuousCount(stats.ValueTotal, row.Value)
		}
	case model.ContinuousParquetSignalHistogram:
		stats.RowCount = int64(len(histRows))
		for _, row := range histRows {
			stats.SampleTotal = addContinuousCount(stats.SampleTotal, row.Count)
			stats.ValueTotal = addContinuousCount(stats.ValueTotal, row.Count)
		}
	case model.ContinuousParquetSignalDB:
		stats.RowCount = int64(len(dbRows))
		for _, row := range dbRows {
			count := row.CallCount
			if count == 0 {
				count = row.OccurrenceCount
			}
			stats.SampleTotal = addContinuousCount(stats.SampleTotal, count)
			stats.ValueTotal = addContinuousCount(stats.ValueTotal, count)
		}
	}
	return stats
}

func cpuFirstLast(rows []pqCPURow) (time.Time, time.Time) {
	if len(rows) == 0 {
		return time.Time{}, time.Time{}
	}
	return time.UnixMilli(rows[0].Timestamp), time.UnixMilli(rows[len(rows)-1].Timestamp)
}

// estimateRowBytes 估算单行字节（粗略：结构体字段字符串长度之和 + 固定开销）。
func estimateRowBytes(row interface{}) int64 {
	const fixed = 256
	var extra int64
	switch typed := row.(type) {
	case pqCPURow:
		for _, frame := range typed.Frames {
			extra += int64(len(frame.Function) + len(frame.File) + len(frame.MappingFile) + len(frame.BuildID) + len(frame.Raw) + 24)
		}
		extra += int64(len(typed.SessionSID) + len(typed.Service) + len(typed.Comm) + len(typed.Exe) + len(typed.Backend) + len(typed.Runtime) + len(typed.ProfileType))
	case pqMetricRow:
		extra += int64(len(typed.Metric) + len(typed.Comm) + len(typed.Exe) + len(typed.Runtime) + len(typed.Unit))
	case pqHistogramRow:
		extra += int64(len(typed.SignalType) + len(typed.Backend) + len(typed.Unit) + len(typed.Reason))
	case pqDBRow:
		extra += int64(len(typed.Kind) + len(typed.Instance) + len(typed.SchemaName) + len(typed.DigestText) + len(typed.WaitingQuery) + len(typed.BlockingQuery) + len(typed.LockedTable) + len(typed.MaxWaitRepresentative))
	}
	total := fixed + extra
	if total < 128 {
		total = 128
	}
	return total
}

// ---------------------------------------------------------------------------
// 降采样 raw → 5m → 1h
// ---------------------------------------------------------------------------

// pqBuildDownsample 从 sourceResolution 构建 targetResolution 块
// （raw→5m、5m→1h）。
func (s *APIServer) pqBuildDownsample(ctx context.Context, tenant string, hourStart time.Time, sourceResolution, targetResolution string) (bool, error) {
	if ok, reason := s.maintenanceSpaceOK(0); !ok {
		incParquetBuildSkip("low_disk:" + reason)
		return false, nil
	}
	if ok, _ := s.continuousQuotaOK(ctx); !ok {
		incParquetBuildSkip("quota_exceeded")
		s.pqReclaimForQuota(ctx)
		return false, nil
	}

	builtAny := false
	for _, signalType := range []string{
		model.ContinuousParquetSignalCPU,
		model.ContinuousParquetSignalMetrics,
		model.ContinuousParquetSignalHistogram,
		model.ContinuousParquetSignalDB,
	} {
		key := pqBlockKey{Tenant: tenant, BucketStart: hourStart, SignalType: signalType, Resolution: sourceResolution}
		source, err := s.pqFindActiveBlock(ctx, key)
		if err != nil {
			return builtAny, err
		}
		if source == nil {
			continue // 源不存在 → 无降采样
		}
		files, err := s.pqLoadBlockFiles(ctx, source.BlockID)
		if err != nil || len(files) == 0 {
			continue
		}
		// 读取源块全部行（目标粒度查询需要完整窗口）
		var cpuRows []pqCPURow
		var metricRows []pqMetricRow
		var histRows []pqHistogramRow
		var dbRows []pqDBRow
		for _, file := range files {
			switch signalType {
			case model.ContinuousParquetSignalCPU:
				rows, err := readParquetRows[pqCPURow](s, ctx, file.ObjectKey, 0, 0)
				if err != nil {
					s.Logger.Warn("v2 downsample: 读取源块失败", zap.String("block_id", source.BlockID), zap.Error(err))
					return builtAny, err
				}
				cpuRows = append(cpuRows, rows...)
			case model.ContinuousParquetSignalMetrics:
				rows, err := readParquetRows[pqMetricRow](s, ctx, file.ObjectKey, 0, 0)
				if err != nil {
					return builtAny, err
				}
				metricRows = append(metricRows, rows...)
			case model.ContinuousParquetSignalHistogram:
				rows, err := readParquetRows[pqHistogramRow](s, ctx, file.ObjectKey, 0, 0)
				if err != nil {
					return builtAny, err
				}
				histRows = append(histRows, rows...)
			case model.ContinuousParquetSignalDB:
				rows, err := readParquetRows[pqDBRow](s, ctx, file.ObjectKey, 0, 0)
				if err != nil {
					return builtAny, err
				}
				dbRows = append(dbRows, rows...)
			}
		}

		// 降采样聚合
		var outCPU []pqCPURow
		var outMetric []pqMetricRow
		var outHist []pqHistogramRow
		var outDB []pqDBRow
		switch signalType {
		case model.ContinuousParquetSignalCPU:
			outCPU = downsampleCPURows(cpuRows, targetResolution)
		case model.ContinuousParquetSignalMetrics:
			outMetric = downsampleMetricRows(metricRows, targetResolution)
		case model.ContinuousParquetSignalHistogram:
			outHist = downsampleHistogramRows(histRows, targetResolution)
		case model.ContinuousParquetSignalDB:
			outDB = downsampleDBRows(dbRows, targetResolution)
		}

		sessionSet := map[string]bool{}
		for _, row := range cpuRows {
			sessionSet[row.SessionSID] = true
		}
		for _, row := range metricRows {
			sessionSet[row.SessionSID] = true
		}
		for _, row := range histRows {
			sessionSet[row.SessionSID] = true
		}
		for _, row := range dbRows {
			sessionSet[row.SessionSID] = true
		}

		processSet := map[string]bool{}
		for _, row := range cpuRows {
			processSet[strconv.Itoa(int(row.PID))+"|"+strconv.FormatInt(row.ProcessStartMs, 10)] = true
		}
		for _, row := range metricRows {
			processSet[strconv.Itoa(int(row.PID))+"|"+strconv.FormatInt(row.ProcessStartMs, 10)] = true
		}

		sourceMembers := []model.ContinuousParquetBlockMember{{
			SourceKind: "block", SourceRef: source.BlockID,
			SessionSID: "", StartTime: source.BucketStart, EndTime: source.BucketEnd,
			SampleCount: source.SampleTotal, ValueTotal: source.ValueTotal, RowCount: source.RowCount,
		}}
		ok, err := s.pqWriteSignalBlock(ctx, tenant, hourStart, source.BucketEnd,
			signalType, targetResolution, source.BlockID,
			outCPU, outMetric, outHist, outDB, sessionSet, processSet, sourceMembers)
		if err != nil {
			s.Logger.Warn("v2 downsample: 目标块构建失败",
				zap.String("signal", signalType), zap.String("resolution", targetResolution),
				zap.String("source", source.BlockID), zap.Error(err))
			continue
		}
		builtAny = builtAny || ok
	}
	return builtAny, nil
}

// pqSeriesKey 构造 series 组键（用于降采样分组）。
func pqSeriesKey(parts ...string) string {
	return strings.Join(parts, pqGroupSep)
}

// pqSortedLabelKey labels map → 稳定排序键。
func pqSortedLabelKey(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	for _, key := range keys {
		builder.WriteString(key)
		builder.WriteString(pqGroupSep)
		builder.WriteString(labels[key])
		builder.WriteString(pqGroupSep)
	}
	return builder.String()
}

// pqStackKey frames → 稳定键（完整结构化 stack）。
func pqStackKey(frames []pqCPUFrame) string {
	if len(frames) == 0 {
		return ""
	}
	var builder strings.Builder
	for i := range frames {
		if i > 0 {
			builder.WriteString(";")
		}
		frame := &frames[i]
		builder.WriteString(frame.Function)
		builder.WriteByte('|')
		builder.WriteString(frame.File)
		builder.WriteByte('|')
		builder.WriteString(strconv.FormatInt(int64(frame.Line), 10))
		builder.WriteByte('|')
		builder.WriteString(strconv.FormatUint(frame.Address, 16))
		builder.WriteByte('|')
		builder.WriteString(frame.MappingFile)
		builder.WriteByte('|')
		builder.WriteString(frame.BuildID)
		builder.WriteByte('|')
		builder.WriteString(strconv.FormatUint(frame.NormalizedOffset, 16))
	}
	return builder.String()
}

// downsampleCPURows 按 (bucket, series labels, stack, unit) 聚合 value=sum。
func downsampleCPURows(rows []pqCPURow, targetResolution string) []pqCPURow {
	type key struct {
		bucket, session, backend, runtime, labels, stack, unit, profileType string
		pid                                                                 int32
		processStartMs                                                      int64
	}
	acc := map[key]*pqCPURow{}
	order := []key{}
	for i := range rows {
		row := &rows[i]
		bucket := pqBucketStart(time.UnixMilli(row.Timestamp), targetResolution).UnixMilli()
		k := key{
			bucket:         strconv.FormatInt(bucket, 10),
			session:        row.SessionSID,
			backend:        row.Backend,
			runtime:        row.Runtime,
			labels:         pqSortedLabelKey(row.Labels),
			stack:          pqStackKey(row.Frames),
			unit:           row.Unit,
			profileType:    row.ProfileType,
			pid:            row.PID,
			processStartMs: row.ProcessStartMs,
		}
		accRow := acc[k]
		if accRow == nil {
			clone := *row
			clone.Timestamp = bucket
			clone.Value = 0
			acc[k] = &clone
			order = append(order, k)
		}
		acc[k].Value = addContinuousCount(acc[k].Value, row.Value)
	}
	out := make([]pqCPURow, 0, len(order))
	for _, k := range order {
		out = append(out, *acc[k])
	}
	return out
}

// downsampleMetricRows gauge: min/max/sum/count/last；counter: 正向 delta 和。
func downsampleMetricRows(rows []pqMetricRow, targetResolution string) []pqMetricRow {
	type key struct {
		bucket, session, metric, kind, unit, labels string
		pid                                         int32
		processStartMs                              int64
	}
	type acc struct {
		row pqMetricRow
	}
	accs := map[key]*acc{}
	order := []key{}
	for i := range rows {
		row := &rows[i]
		bucket := pqBucketStart(time.UnixMilli(row.Timestamp), targetResolution).UnixMilli()
		k := key{
			bucket: strconv.FormatInt(bucket, 10), session: row.SessionSID,
			metric: row.Metric, kind: row.MetricKind, unit: row.Unit,
			labels: pqSortedLabelKey(row.Labels), pid: row.PID, processStartMs: row.ProcessStartMs,
		}
		item := accs[k]
		if item == nil {
			clone := *row
			clone.Timestamp = bucket
			clone.Min, clone.Max, clone.Sum, clone.Count, clone.Last = row.Value, row.Value, row.Value, 1, row.Value
			clone.Delta = 0
			accs[k] = &acc{row: clone}
			order = append(order, k)
			continue
		}
		a := &item.row
		prevLast := a.Last
		a.Last = row.Value
		a.Count = addContinuousCount(a.Count, 1)
		a.Sum = addContinuousCount(a.Sum, row.Value)
		if row.Value < a.Min {
			a.Min = row.Value
		}
		if row.Value > a.Max {
			a.Max = row.Value
		}
		if row.MetricKind == "counter" {
			// reset-aware：只累加正向增量；回绕/重启后从 0 起。
			if row.Value >= prevLast {
				a.Delta = addContinuousCount(a.Delta, row.Value-prevLast)
			} else {
				a.Delta = addContinuousCount(a.Delta, row.Value)
			}
		}
	}
	out := make([]pqMetricRow, 0, len(order))
	for _, k := range order {
		row := accs[k].row
		// avg 供查询默认返回
		row.Value = row.Sum
		out = append(out, row)
	}
	return out
}

// downsampleHistogramRows 相同 bucket bounds count 求和，min/max 合并，
// 分位数用加权平均近似（查询侧按合并桶重算）。
func downsampleHistogramRows(rows []pqHistogramRow, targetResolution string) []pqHistogramRow {
	type key struct {
		bucket, session, signal, backend, unit, reason string
		low, high                                      float64
	}
	type acc struct {
		row         pqHistogramRow
		totalP50    float64
		totalP95    float64
		totalP99    float64
		countWeight uint64
		hasMinMax   bool
	}
	accs := map[key]*acc{}
	order := []key{}
	for i := range rows {
		row := &rows[i]
		bucket := pqBucketStart(time.UnixMilli(row.Timestamp), targetResolution).UnixMilli()
		k := key{
			bucket: strconv.FormatInt(bucket, 10), session: row.SessionSID,
			signal: row.SignalType, backend: row.Backend, unit: row.Unit, reason: row.Reason,
			low: row.BucketLow, high: row.BucketHigh,
		}
		item := accs[k]
		if item == nil {
			clone := *row
			clone.Timestamp = bucket
			clone.Count = 0
			clone.EventCount = 0
			item = &acc{row: clone}
			accs[k] = item
			order = append(order, k)
		}
		a := item
		a.row.Count = addContinuousCount(a.row.Count, row.Count)
		a.row.EventCount = addContinuousCount(a.row.EventCount, row.EventCount)
		if !a.hasMinMax || row.Min < a.row.Min {
			a.row.Min = row.Min
		}
		if !a.hasMinMax || row.Max > a.row.Max {
			a.row.Max = row.Max
		}
		a.hasMinMax = true
		if row.Unavailable {
			a.row.Unavailable = true
		}
		if row.Count > 0 {
			weight := row.Count
			a.totalP50 += row.P50 * float64(weight)
			a.totalP95 += row.P95 * float64(weight)
			a.totalP99 += row.P99 * float64(weight)
			a.countWeight += weight
		}
	}
	out := make([]pqHistogramRow, 0, len(order))
	for _, k := range order {
		item := accs[k]
		if item.countWeight > 0 {
			item.row.P50 = item.totalP50 / float64(item.countWeight)
			item.row.P95 = item.totalP95 / float64(item.countWeight)
			item.row.P99 = item.totalP99 / float64(item.countWeight)
		}
		out = append(out, item.row)
	}
	return out
}

// downsampleDBRows digest 增量求和；lock_wait 次数/最大等待/代表查询。
func downsampleDBRows(rows []pqDBRow, targetResolution string) []pqDBRow {
	type key struct {
		bucket, session, kind, instance, schema, digest, waitingQ, blockingQ, table string
		waitingPID, blockingPID                                                     int64
	}
	type acc struct {
		row pqDBRow
	}
	accs := map[key]*acc{}
	order := []key{}
	for i := range rows {
		row := &rows[i]
		bucket := pqBucketStart(time.UnixMilli(row.Timestamp), targetResolution).UnixMilli()
		k := key{
			bucket: strconv.FormatInt(bucket, 10), session: row.SessionSID, kind: row.Kind,
			instance: row.Instance, schema: row.SchemaName, digest: row.DigestText,
			waitingQ: row.WaitingQuery, blockingQ: row.BlockingQuery, table: row.LockedTable,
			waitingPID: row.WaitingPID, blockingPID: row.BlockingPID,
		}
		item := accs[k]
		if item == nil {
			clone := *row
			clone.Timestamp = bucket
			clone.CallCount = 0
			clone.TotalLatencyUs = 0
			clone.RowsExaminedTotal = 0
			clone.OccurrenceCount = 0
			clone.MaxWaitSeconds = 0
			accs[k] = &acc{row: clone}
			order = append(order, k)
			item = accs[k]
		}
		a := &item.row
		a.CallCount = addContinuousCount(a.CallCount, row.CallCount)
		a.TotalLatencyUs = addContinuousCount(a.TotalLatencyUs, row.TotalLatencyUs)
		a.RowsExaminedTotal = addContinuousCount(a.RowsExaminedTotal, row.RowsExaminedTotal)
		if row.Kind == "lock_wait" {
			a.OccurrenceCount = addContinuousCount(a.OccurrenceCount, 1)
			// 单事件等待时间用 WaitSeconds；聚合保留最大等待与对应代表查询。
			if row.WaitSeconds > a.MaxWaitSeconds {
				a.MaxWaitSeconds = row.WaitSeconds
				a.WaitSeconds = row.WaitSeconds
				a.MaxWaitRepresentative = row.MaxWaitRepresentative
			}
		}
	}
	out := make([]pqDBRow, 0, len(order))
	for _, k := range order {
		out = append(out, accs[k].row)
	}
	return out
}

// pqV1SignalTypesFor 返回某 v2 信号对应的 v1 window signal_type 集合。
func pqV1SignalTypesFor(signalType string) []string {
	switch signalType {
	case model.ContinuousParquetSignalCPU:
		return []string{"cpu_profile", "cpu", "python_memory", "memory"}
	case model.ContinuousParquetSignalMetrics:
		return []string{"metrics", "python_rss"}
	case model.ContinuousParquetSignalHistogram:
		return []string{"io_latency", "io_syscall_latency", "sched_latency"}
	case model.ContinuousParquetSignalDB:
		return []string{"db", "db_snapshot"}
	default:
		return nil
	}
}

// pqRawSignalSourceSampleTotal 保留用于指标/诊断：按信号统计某小时 v1 window
// 样本总量（与 v2 口径不完全一致——仅作观察，不作为对账判据）。
func (s *APIServer) pqRawSignalSourceSampleTotal(ctx context.Context, bucketStart time.Time, signalType string) (uint64, error) {
	types := pqV1SignalTypesFor(signalType)
	if len(types) == 0 {
		return 0, fmt.Errorf("未知 v2 信号类型: %s", signalType)
	}
	var total uint64
	err := s.DB.WithContext(ctx).Model(&model.ProfileWindow{}).
		Where("window_start >= ? AND window_start < ? AND signal_type IN ?", bucketStart, bucketStart.Add(time.Hour), types).
		Select("COALESCE(SUM(sample_count),0)").Scan(&total).Error
	return total, err
}

// pqShadowReconcileBlock 周期性对账（阶段六）：对 raw 块按信号做窗口完整性
// 校验——该小时该信号下每个可查询源窗口（非空 object_key）的 batch_bid 必须
// 是块的 lineage member。任何遗漏（迟到 batch 未被重写/部分失败）→ failed。
// 不做样本总量比较（v1 sample_count 与 v2 行计数口径不同，见 builder 注释）。
func (s *APIServer) pqShadowReconcileBlock(ctx context.Context, block *model.ContinuousParquetBlock) error {
	if block == nil || block.Resolution != model.ContinuousParquetResolutionRaw {
		return nil
	}
	now := time.Now()
	// 块内 lineage member（batch bid 集合）
	var members []model.ContinuousParquetBlockMember
	if err := s.DB.WithContext(ctx).
		Where("block_id = ? AND source_kind = ?", block.BlockID, "batch").
		Find(&members).Error; err != nil {
		return err
	}
	memberSet := make(map[string]bool, len(members))
	for _, m := range members {
		memberSet[m.SourceRef] = true
	}
	// 该小时该信号的源窗口（只检查构建时已存在的窗口：构建后到达的迟到
	// 窗口由 pqSealedRawHours 触发块重建，不是本块的数据缺失）。
	types := pqV1SignalTypesFor(block.SignalType)
	var windows []model.ProfileWindow
	if err := s.DB.WithContext(ctx).
		Where("window_start >= ? AND window_start < ? AND signal_type IN ? AND created_at <= ?",
			block.BucketStart, block.BucketStart.Add(time.Hour), types, block.CreatedAt).
		Select("id, session_sid, batch_bid, object_key, window_start").
		Find(&windows).Error; err != nil {
		return err
	}
	var missed []string
	for _, w := range windows {
		if w.ObjectKey == "" {
			continue
		}
		if !memberSet[w.BatchBID] {
			missed = append(missed, fmt.Sprintf("win=%d batch=%s", w.ID, w.BatchBID))
		}
	}
	if len(missed) > 0 {
		incParquetShadowFailure()
		msg := fmt.Sprintf("信号 %s 遗漏 %d 个源窗口: %s", block.SignalType, len(missed), strings.Join(missed[:minInt(len(missed), 5)], ","))
		_ = s.DB.WithContext(ctx).Model(&model.ContinuousParquetBlock{}).
			Where("block_id = ? AND status = ?", block.BlockID, model.ContinuousParquetStatusActive).
			Updates(map[string]interface{}{
				"reconcile_status": model.ContinuousParquetReconcileFailed,
				"reconciled_at":    now,
				"reconcile_error":  msg, "updated_at": now,
			}).Error
		s.Logger.Error("v2 shadow 对账不一致（窗口完整性）",
			zap.String("signal", block.SignalType), zap.String("block_id", block.BlockID), zap.String("error", msg))
		return nil
	}
	if err := s.DB.WithContext(ctx).Model(&model.ContinuousParquetBlock{}).
		Where("block_id = ? AND status = ?", block.BlockID, model.ContinuousParquetStatusActive).
		Updates(map[string]interface{}{
			"reconcile_status": model.ContinuousParquetReconcilePassed,
			"reconciled_at":    now,
			"reconcile_error":  "", "updated_at": now,
		}).Error; err != nil {
		return err
	}
	return nil
}

// minInt 返回两 int 较小值（Go 1.22 前无内置 min）。
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// pqReconcileQuarantine 把连续多次对账失败（且跨越至少 30 分钟）的 raw 块
// 标记为 quarantined，fail-closed：查询与细粒度 GC 均不接受。
func (s *APIServer) pqReconcileQuarantine(ctx context.Context, limit int) {
	if limit <= 0 {
		limit = 50
	}
	now := time.Now()
	var failed []model.ContinuousParquetBlock
	if err := s.DB.WithContext(ctx).
		Where("status = ? AND resolution = ? AND reconcile_status = ?",
			model.ContinuousParquetStatusActive, model.ContinuousParquetResolutionRaw,
			model.ContinuousParquetReconcileFailed).
		Order("reconciled_at ASC").Limit(limit).Find(&failed).Error; err != nil {
		return
	}
	for i := range failed {
		blk := &failed[i]
		if blk.ReconciledAt == nil || now.Sub(*blk.ReconciledAt) < 30*time.Minute {
			continue
		}
		_ = s.DB.WithContext(ctx).Model(&model.ContinuousParquetBlock{}).
			Where("block_id = ? AND status = ? AND reconcile_status = ?",
				blk.BlockID, model.ContinuousParquetStatusActive, model.ContinuousParquetReconcileFailed).
			Updates(map[string]interface{}{
				"reconcile_status": model.ContinuousParquetReconcileQuarantined,
				"updated_at":       now,
			}).Error
	}
}

// pqReclaimForQuota 配额回收：先清 superseded/staging，再按最老 1h 回收。
func (s *APIServer) pqReclaimForQuota(ctx context.Context) {
	// 1) superseded 立即回收（不等宽限）
	var superseded []model.ContinuousParquetBlock
	if err := s.DB.WithContext(ctx).Where("status = ?", model.ContinuousParquetStatusSuperseded).
		Order("superseded_at ASC").Limit(50).Find(&superseded).Error; err == nil {
		for i := range superseded {
			blk := &superseded[i]
			if err := s.pqDeleteBlockObjectsByPrefix(ctx, blk, 50); err == nil {
				_ = s.pqTombstoneBlock(ctx, blk, "quota_reclaim")
			}
		}
	}
	// 2) staging：只删除已有完整、校验通过 raw v2 lineage 的源对象。
	s.pqReclaimStaging(ctx)
	// 3) 若仍超目标水位，按最老 1h 分区立即回收到 target。
	s.pqReclaimOldest1hToTarget(ctx, 100)
}

// pqReclaimStaging 清理超过 staging 保留期的未压缩 batch 元数据与源对象。
// 清理顺序（阶段六修正，修复"先删 batch 遗留 orphan window"问题）：
//
//	覆盖确认 → window/batch 同事务清理 → 提交后才删除源对象。
//
// 对象删除失败进入 continuous_migration_failures 可重试状态，不阻塞元数据清理，
// 且绝不允许"对象已删但 DB 仍保留 active 查询引用"。
func (s *APIServer) pqReclaimStaging(ctx context.Context) {
	// off/shadow/prefer 仍可能整段回退 v1；删除分钟对象会让 ProfileWindow
	// 指向不存在的源。只有 enforce 且 lineage 完整时才允许回收。
	if s.pqModeOf() != "enforce" {
		return
	}
	retention := time.Duration(s.Config.ContinuousParquet.StagingMinutesRetention) * time.Minute
	if retention <= 0 {
		retention = 120 * time.Minute
	}
	cutoff := time.Now().Add(-retention)
	var batches []model.ProfileBatch
	if err := s.DB.WithContext(ctx).
		Where("(block_id IS NULL OR block_id = '') AND created_at < ?", cutoff).
		Order("created_at ASC").Limit(200).Find(&batches).Error; err != nil {
		return
	}
	seenObjects := map[string]bool{}
	for i := range batches {
		b := &batches[i]
		if b.ObjectKey == "" || seenObjects[b.ObjectKey] {
			continue
		}
		seenObjects[b.ObjectKey] = true
		var refs []model.ProfileBatch
		if err := s.DB.WithContext(ctx).
			Where("object_key = ? AND (block_id IS NULL OR block_id = '')", b.ObjectKey).
			Find(&refs).Error; err != nil || len(refs) == 0 {
			continue
		}
		covered := true
		ids := make([]uint, 0, len(refs))
		bids := make([]string, 0, len(refs))
		for j := range refs {
			if !refs[j].CreatedAt.Before(cutoff) || !s.pqBatchCoveredByValidatedRaw(ctx, &refs[j]) {
				covered = false
				break
			}
			ids = append(ids, refs[j].ID)
			bids = append(bids, refs[j].BID)
		}
		if !covered {
			continue
		}
		var reclaimed int64
		for j := range refs {
			reclaimed += int64(refs[j].PayloadBytes)
		}
		// 1) 事务内清理 window + batch 元数据（CASCADE 外键下删 batch 即级联
		//    window；显式先删 window 兼容 NOT VALID 过渡期，绝不遗留 orphan）。
		txErr := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if len(bids) > 0 {
				if err := tx.Where("batch_bid IN ?", bids).Delete(&model.ProfileWindow{}).Error; err != nil {
					return err
				}
			}
			return tx.Where("id IN ?", ids).Delete(&model.ProfileBatch{}).Error
		})
		if txErr != nil {
			s.Logger.Warn("v2 staging 元数据清理失败（对象保持不动）", zap.String("object_key", b.ObjectKey), zap.Error(txErr))
			continue
		}
		// 2) 提交后删除源对象；失败进入可重试异常记录。
		if err := s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, b.ObjectKey); err != nil {
			s.recordContinuousMigrationFailure(ctx, "batch", bids[0], b.SessionSID, b.ObjectKey, "object_delete", err.Error())
			continue
		}
		incContinuousReclaimedBytes(reclaimed)
	}
}

// pqBatchCoveredByValidatedRaw batch 的每种实际信号都已被 active、validated、
// reconciled 的 raw Block lineage 覆盖（阶段六：对账通过才允许清理）。
func (s *APIServer) pqBatchCoveredByValidatedRaw(ctx context.Context, batch *model.ProfileBatch) bool {
	if batch == nil || batch.BID == "" {
		return false
	}
	expected := map[string]bool{}
	var signalTypes []string
	_ = json.Unmarshal(batch.SignalTypes, &signalTypes)
	for _, signalType := range signalTypes {
		if signal := pqLedgerSignalForWindow(signalType); signal != "" {
			expected[signal] = true
		}
	}
	if len(expected) == 0 {
		expected[model.ContinuousParquetSignalCPU] = true
	}
	for signal := range expected {
		var count int64
		err := s.DB.WithContext(ctx).Table("continuous_parquet_block_members AS m").
			Joins("JOIN continuous_parquet_blocks AS b ON b.block_id = m.block_id").
			Where("m.source_kind = ? AND m.source_ref = ?", "batch", batch.BID).
			Where("b.signal_type = ? AND b.resolution = ? AND b.status = ? AND b.validation = ? AND b.reconcile_status = ?",
				signal, model.ContinuousParquetResolutionRaw, model.ContinuousParquetStatusActive,
				model.ContinuousParquetValidationPassed, model.ContinuousParquetReconcilePassed).
			Count(&count).Error
		if err != nil || count == 0 {
			return false
		}
	}
	return true
}

func (s *APIServer) pqReclaimOldest1hToTarget(ctx context.Context, limit int) {
	snap := s.continuousQuotaSnapshot(ctx)
	target := snap.TargetBytes
	if target <= 0 || target >= snap.QuotaBytes {
		target = snap.QuotaBytes * 9 / 10
	}
	if snap.UsedBytes <= target {
		return
	}
	var blocks []model.ContinuousParquetBlock
	if err := s.DB.WithContext(ctx).
		Where("status = ? AND resolution = ?", model.ContinuousParquetStatusActive, model.ContinuousParquetResolution1h).
		Order("bucket_start ASC, id ASC").Limit(limit).Find(&blocks).Error; err != nil {
		return
	}
	projected := snap.UsedBytes
	for i := range blocks {
		if projected <= target {
			break
		}
		blk := &blocks[i]
		if err := s.pqSupersedeAndDeleteBlock(ctx, blk, "quota_reclaim"); err != nil {
			continue
		}
		if err := s.pqDeleteBlockObjectsByPrefix(ctx, blk, 1000); err != nil {
			continue
		}
		if err := s.pqTombstoneBlock(ctx, blk, "quota_reclaim"); err != nil {
			continue
		}
		projected -= blk.BytesTotal
	}
}

// jsonUnmarshal JSON 反序列化（window labels 用）。
var jsonUnmarshal = func(data []byte, v interface{}) error { return json.Unmarshal(data, v) }
