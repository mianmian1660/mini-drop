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
				Value:          sample.Count,
				Unit:           "samples",
				ProfileType:    window.SignalType,
			}
			rows.CPU = append(rows.CPU, row)
		}
	case "metrics":
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
	case "db":
		for _, snapshot := range in.DBSnapshots {
			row := pqDBRow{
				Timestamp:            ts,
				SessionSID:           sessionSID,
				Kind:                 snapshot.Kind,
				Instance:             snapshot.InstanceLabel,
				SchemaName:           snapshot.SchemaName,
				DigestText:           snapshot.DigestText,
				CallCount:            snapshot.CallCount,
				TotalLatencyUs:       snapshot.TotalLatencyUs,
				RowsExaminedTotal:    snapshot.RowsExaminedTotal,
				WaitingPID:           snapshot.WaitingPID,
				WaitingQuery:         snapshot.WaitingQuery,
				BlockingPID:          snapshot.BlockingPID,
				BlockingQuery:        snapshot.BlockingQuery,
				WaitSeconds:          snapshot.WaitSeconds,
				LockedTable:          snapshot.LockedTable,
				OccurrenceCount:      1,
				MaxWaitSeconds:       snapshot.WaitSeconds,
				MaxWaitRepresentative: snapshot.WaitingQuery,
				Labels:               sanitizeContinuousLabels(in.Labels),
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
	inServiceName = func(s *APIServer, sessionSID string) string {
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
	members := map[string]map[string]bool{} // signal → batch bid set
	sessionSet := map[string]bool{}
	processSet := map[string]bool{}

	for _, objectKey := range objectOrder {
		batches, err := s.loadContinuousBatches(ctx, objectKey)
		if err != nil {
			s.Logger.Warn("v2 builder: 加载对象失败，跳过",
				zap.String("object_key", objectKey), zap.Error(err))
			continue
		}
		batchByID := continuousBatchIndex(batches)
		seenBatch := map[string]bool{}
		for _, row := range byObject[objectKey] {
			batch, rowKey, ok := continuousResolveBatch(row, batches, batchByID)
			if !ok || seenBatch[rowKey] {
				continue
			}
			seenBatch[rowKey] = true
			sessionSet[row.SessionSID] = true
			if batch == nil {
				continue
			}
			for _, in := range batch.Windows {
				if !windowOverlaps(in.WindowStart, in.WindowEnd, hourStart, hourEnd) {
					continue
				}
				rows := pqCollectWindowRows(s, row, batch, &in)
				cpuRows = append(cpuRows, rows.CPU...)
				metricRows = append(metricRows, rows.Metrics...)
				histRows = append(histRows, rows.Histogram...)
				dbRows = append(dbRows, rows.DB...)
				for _, sample := range rows.CPU {
					processSet[strconv.Itoa(int(sample.PID))+"|"+strconv.FormatInt(sample.ProcessStartMs, 10)] = true
				}
			}
			if members[batch.BatchID] == nil {
				members[batch.BatchID] = map[string]bool{}
			}
			members[batch.BatchID]["batch"] = true
		}
	}

	// 按信号拆 part 写入
	builtAny := false
	signalRows := []struct {
		signal string
		cpu    []pqCPURow
		metric []pqMetricRow
		hist   []pqHistogramRow
		db     []pqDBRow
	}{
		{signal: model.ContinuousParquetSignalCPU, cpu: cpuRows},
		{signal: model.ContinuousParquetSignalMetrics, metric: metricRows},
		{signal: model.ContinuousParquetSignalHistogram, hist: histRows},
		{signal: model.ContinuousParquetSignalDB, db: dbRows},
	}
	for _, group := range signalRows {
		total := len(group.cpu) + len(group.metric) + len(group.hist) + len(group.db)
		if total == 0 {
			continue
		}
		ok, err := s.pqWriteSignalBlock(ctx, tenant, hourStart, hourEnd,
			group.signal, model.ContinuousParquetResolutionRaw, "",
			group.cpu, group.metric, group.hist, group.db,
			sessionSet, processSet, members)
		if err != nil {
			s.Logger.Warn("v2 builder: raw 块构建失败",
				zap.String("signal", group.signal), zap.Time("hour", hourStart), zap.Error(err))
			continue
		}
		builtAny = builtAny || ok
	}
	return builtAny, nil
}

// pqWriteSignalBlock 构建并登记单个 (signal, resolution) 块。
// sourceMembers：raw 为 batch 集合；降采样为来源块集合。
func (s *APIServer) pqWriteSignalBlock(ctx context.Context, tenant string, hourStart, hourEnd time.Time,
	signalType, resolution, sourceBlockID string,
	cpuRows []pqCPURow, metricRows []pqMetricRow, histRows []pqHistogramRow, dbRows []pqDBRow,
	sessionSet map[string]bool, processSet map[string]bool, sourceMembers map[string]map[string]bool) (bool, error) {

	key := pqBlockKey{Tenant: tenant, BucketStart: hourStart, SignalType: signalType, Resolution: resolution}

	// 单飞锁
	lockKey := "cblk|pqv2|" + tenant + "|" + signalType + "|" + resolution + "|" + hourStart.UTC().Format(time.RFC3339)
	release, err := s.pqLockPartition(ctx, lockKey)
	if err != nil {
		return false, err
	}
	defer release()

	now := time.Now()
	blockID := pqNewBlockID()

	// 版本 = 旧 active 版本 + 1（无旧块则 1）
	version := 1
	if old, err := s.pqFindActiveBlock(ctx, key); err == nil && old != nil {
		version = old.Version + 1
	}

	if _, err := s.pqCreateBuildingBlock(ctx, key, blockID, hourEnd, version); err != nil {
		return false, fmt.Errorf("登记 building 块失败: %w", err)
	}

	results, stats, memberRows, err := s.pqWriteAndVerifyParts(ctx, tenant, hourStart, hourEnd,
		signalType, resolution, blockID, cpuRows, metricRows, histRows, dbRows)
	if err != nil {
		_ = s.pqMarkBlockFailed(ctx, blockID, "build_failed")
		return false, err
	}

	// 汇总统计（多 part 聚合）
	for _, result := range results {
		stats.BytesTotal += result.SizeBytes
	}
	stats.SessionCount = len(sessionSet)
	stats.ProcessCount = len(processSet)
	if len(memberRows) == 0 {
		for bid, kinds := range sourceMembers {
			for kind := range kinds {
				memberRows = append(memberRows, model.ContinuousParquetBlockMember{
					SourceKind: kind, SourceRef: bid, StartTime: hourStart, EndTime: hourEnd,
					CreatedAt: now,
				})
			}
		}
	}

	// 登记 active（单事务：退役旧 active → 插入新 active → files → members）
	if err := s.pqRegisterActiveBlockMulti(ctx, key, blockID, hourEnd, version, results, stats, memberRows); err != nil {
		_ = s.pqMarkBlockFailed(ctx, blockID, "register_failed")
		// 登记失败不删对象（由 sweep 按 block_id 前缀回收）
		return false, err
	}

	// shadow/prefer 模式：v1 仍是查询源，仅对账（不删 v1）
	if s.Config.ContinuousParquet.Mode == "shadow" || s.Config.ContinuousParquet.Mode == "prefer" {
		s.pqShadowReconcile(ctx, key, blockID, stats)
	}
	s.Logger.Info("parquet v2 块已登记",
		zap.String("signal", signalType), zap.String("resolution", resolution),
		zap.String("block_id", blockID), zap.Time("bucket_start", hourStart),
		zap.Int("version", version), zap.Int64("row_count", stats.RowCount),
		zap.Int64("bytes", stats.BytesTotal), zap.Int("parts", len(results)))
	return true, nil
}

// pqRegisterActiveBlockMulti 多 part 版登记（扩展单 part 版）：把 building
// 行更新为 active，并登记全部 part 文件与 members。
func (s *APIServer) pqRegisterActiveBlockMulti(ctx context.Context, key pqBlockKey, blockID string, bucketEnd time.Time,
	version int, results []parquetWriteResult, stats pqBlockStats, members []model.ContinuousParquetBlockMember) error {
	now := time.Now()
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
// 返回 (results, 汇总统计, 空)。
func (s *APIServer) pqWriteAndVerifyParts(ctx context.Context, tenant string, hourStart, hourEnd time.Time,
	signalType, resolution, blockID string,
	cpuRows []pqCPURow, metricRows []pqMetricRow, histRows []pqHistogramRow, dbRows []pqDBRow) ([]parquetWriteResult, pqBlockStats, []model.ContinuousParquetBlockMember, error) {
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
				return nil, stats, nil, err
			}
			start += size
		}
		stats.SampleTotal = uint64(len(cpuRows))
		stats.FirstRowTime, stats.LastRowTime = cpuFirstLast(cpuRows)
	case model.ContinuousParquetSignalMetrics:
		start := 0
		for _, size := range cut(len(metricRows), splitCount(len(metricRows))) {
			if size <= 0 {
				continue
			}
			if err := writePart(metricRows[start : start+size]); err != nil {
				return nil, stats, nil, err
			}
			start += size
		}
		for i := range metricRows {
			stats.ValueTotal = addContinuousCount(stats.ValueTotal, metricRows[i].Value)
		}
	case model.ContinuousParquetSignalHistogram:
		start := 0
		for _, size := range cut(len(histRows), splitCount(len(histRows))) {
			if size <= 0 {
				continue
			}
			if err := writePart(histRows[start : start+size]); err != nil {
				return nil, stats, nil, err
			}
			start += size
		}
		stats.SampleTotal = uint64(len(histRows))
	case model.ContinuousParquetSignalDB:
		start := 0
		for _, size := range cut(len(dbRows), splitCount(len(dbRows))) {
			if size <= 0 {
				continue
			}
			if err := writePart(dbRows[start : start+size]); err != nil {
				return nil, stats, nil, err
			}
			start += size
		}
		stats.SampleTotal = uint64(len(dbRows))
	}

	// value_total 兜底（无 metrics 时用样本数）
	if stats.ValueTotal == 0 {
		stats.ValueTotal = uint64(len(cpuRows) + len(metricRows) + len(histRows) + len(dbRows))
	}
	return results, stats, nil, nil
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

		sourceMembers := map[string]map[string]bool{source.BlockID: {"block": true}}
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
		pid                                                                    int32
		processStartMs                                                         int64
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
		pid                                            int32
		processStartMs                                 int64
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
		low, high                                         float64
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

// pqShadowReconcile shadow 对账：v2 块与 v1 源的统计核对。
// 校验失败为 0 且失败 block 不进入 active（登记已完成，这里只告警+指标）。
func (s *APIServer) pqShadowReconcile(ctx context.Context, key pqBlockKey, blockID string, stats pqBlockStats) {
	var sourceSampleTotal uint64
	if err := s.DB.WithContext(ctx).Model(&model.ProfileWindow{}).
		Where("window_start >= ? AND window_start < ?", key.BucketStart, key.BucketStart.Add(time.Hour)).
		Select("COALESCE(SUM(sample_count),0)").Scan(&sourceSampleTotal).Error; err != nil {
		s.Logger.Warn("v2 shadow 对账: 源统计失败", zap.Error(err))
		return
	}
	if stats.SampleTotal != sourceSampleTotal {
		incParquetShadowFailure()
		s.Logger.Error("v2 shadow 对账不一致",
			zap.String("signal", key.SignalType), zap.String("resolution", key.Resolution),
			zap.String("block_id", blockID),
			zap.Uint64("v2_samples", stats.SampleTotal),
			zap.Uint64("v1_samples", sourceSampleTotal))
		return
	}
	s.Logger.Info("v2 shadow 对账通过",
		zap.String("signal", key.SignalType), zap.String("resolution", key.Resolution),
		zap.String("block_id", blockID), zap.Uint64("samples", stats.SampleTotal))
}

// pqReclaimForQuota 配额回收：先清 superseded/staging，再按最老 1h 回收。
func (s *APIServer) pqReclaimForQuota(ctx context.Context) {
	now := time.Now()
	// 1) superseded 立即回收（不等宽限）
	var superseded []model.ContinuousParquetBlock
	if err := s.DB.WithContext(ctx).Where("status = ?", model.ContinuousParquetStatusSuperseded).
		Order("superseded_at ASC").Limit(50).Find(&superseded).Error; err == nil {
		for i := range superseded {
			blk := &superseded[i]
			_ = s.pqDeleteBlockObjectsByPrefix(ctx, blk, 50)
			_ = s.pqTombstoneBlock(ctx, blk, "quota_reclaim")
		}
	}
	// 2) 最老 1h 块
	oneHourAgo := now.Add(-time.Duration(s.Config.ContinuousParquet.Res1hRetentionHours) * time.Hour)
	_, _ = s.pqReclaimExpiredBlocks(ctx, model.ContinuousParquetResolution1h, oneHourAgo, 20)
	// 3) staging：删除已过 staging 保留期的未压缩 batch 对象
	s.pqReclaimStaging(ctx)
}

// pqReclaimStaging 清理超过 staging 保留期的未压缩 batch 对象。
func (s *APIServer) pqReclaimStaging(ctx context.Context) {
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
	for i := range batches {
		b := &batches[i]
		if err := s.Storage.DeleteObject(ctx, s.Config.Storage.Bucket, b.ObjectKey); err != nil {
			continue
		}
		s.DB.WithContext(ctx).Where("bid = ?", b.BID).Delete(&model.ProfileBatch{})
		incContinuousReclaimedBytes(int64(b.PayloadBytes))
	}
}

// jsonUnmarshal JSON 反序列化（window labels 用）。
var jsonUnmarshal = func(data []byte, v interface{}) error { return json.Unmarshal(data, v) }
