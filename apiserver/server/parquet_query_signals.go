// ============================================================
// server/parquet_query_signals.go — 阶段六：metrics/histogram/DB 逐小时混合查询
// ============================================================
// 与 CPU（pqQueryAggregateMixed）相同的规划语义：
//   - 每个 UTC 小时按 raw→5m→1h 找最佳已验证（validation=passed AND
//     reconcile_status=passed）Block；
//   - 无 v2 覆盖的小时（当前未封存小时/最近热窗口）整段回退 v1；
//   - 半开区间 [start,end)，请求最后一个时间点单独处理；
//   - 某小时 Parquet 读取失败：热表仍存在则仅该小时回退 v1，热表已清理
//     则返回明确的部分覆盖/依赖错误。
// 响应统一补齐 storage_source / resolution_seconds / mixed_resolution /
// earliest_available_at。
// ============================================================

package server

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mini-drop/apiserver/model"
)

// pqAuthorizedSIDs 授权 session 集合（v2 块跨 session，行级过滤）。
func (s *APIServer) pqAuthorizedSIDs(ctx context.Context, q ProfileQuery) (map[string]struct{}, error) {
	var authorizedSIDs []string
	if err := s.continuousSessionSelection(q).Pluck("sid", &authorizedSIDs).Error; err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(authorizedSIDs))
	for _, sid := range authorizedSIDs {
		out[sid] = struct{}{}
	}
	return out, nil
}

// pqUncoveredHourRuns 返回无 v2 覆盖（或无 reconciled 块）的小时，按连续段分组。
func (s *APIServer) pqUncoveredHourRuns(ctx context.Context, tenant string, q ProfileQuery, signalType string) [][]time.Time {
	hours := pqHourlyRange(q.From, q.To)
	var uncovered []time.Time
	for _, hour := range hours {
		block, err := s.pqFindBestBlock(ctx, tenant, hour, signalType)
		if err != nil || block == nil {
			uncovered = append(uncovered, hour)
		}
	}
	var runs [][]time.Time
	for _, hour := range uncovered {
		if len(runs) > 0 && hour.Sub(runs[len(runs)-1][len(runs[len(runs)-1])-1]) == time.Hour {
			runs[len(runs)-1] = append(runs[len(runs)-1], hour)
			continue
		}
		runs = append(runs, []time.Time{hour})
	}
	return runs
}

// pqStorageSourceForSignal 按指定信号计算存储来源（响应兼容字段）。
func (s *APIServer) pqStorageSourceForSignal(ctx context.Context, q ProfileQuery, signalType string) pqResolveStorageSource {
	out := pqResolveStorageSource{
		ResolutionSeconds: 0, MixedResolution: false, StorageSource: "parquet_v1",
	}
	if !pqModeQueryV2(s.pqModeOf()) {
		out.EarliestAvailable = s.pqEarliestAvailableAt(ctx)
		return out
	}
	tenant := s.Config.ContinuousParquet.Tenant
	hours := pqHourlyRange(q.From, q.To)
	v2Hours, v1Hours, coarsest := 0, 0, 0
	resolutions := map[string]bool{}
	for _, hour := range hours {
		block, err := s.pqFindBestBlock(ctx, tenant, hour, signalType)
		if err != nil || block == nil {
			v1Hours++
			if coarsest < int(pqSignalResolutionSeconds(model.ContinuousParquetResolutionRaw)) {
				coarsest = int(pqSignalResolutionSeconds(model.ContinuousParquetResolutionRaw))
			}
			continue
		}
		v2Hours++
		resolutions[block.Resolution] = true
		if resSec := int(pqSignalResolutionSeconds(block.Resolution)); resSec > coarsest {
			coarsest = resSec
		}
	}
	if v2Hours > 0 {
		out.StorageSource = "parquet_v2"
	}
	out.ResolutionSeconds = coarsest
	if len(resolutions) > 1 || (v1Hours > 0 && v2Hours > 0) {
		out.MixedResolution = true
	}
	out.EarliestAvailable = s.pqEarliestAvailableAt(ctx)
	return out
}

// ---------------------------------------------------------------------------
// Metrics / RSS 时序
// ---------------------------------------------------------------------------

// pqQueryTimeseriesMixed RSS/metrics 时序逐小时混合查询。
func (s *APIServer) pqQueryTimeseriesMixed(ctx context.Context, q ProfileQuery, metricName string, maxSeries int) ([]ProfileTimeseriesSeries, bool, error) {
	if !pqModeQueryV2(s.pqModeOf()) {
		return s.queryNativeContinuousTimeseries(ctx, q, metricName, maxSeries)
	}
	authorized, err := s.pqAuthorizedSIDs(ctx, q)
	if err != nil {
		return nil, false, err
	}
	tenant := s.Config.ContinuousParquet.Tenant
	byKey := map[string]*ProfileTimeseriesSeries{}
	seriesOrder := []string{}
	addPoint := func(pid int32, processStartMs int64, comm, exe, runtime string, ts time.Time, value uint64) {
		key := strconv.Itoa(int(pid)) + "|" + strconv.FormatInt(processStartMs, 10) + "|" + exe
		series := byKey[key]
		if series == nil {
			series = &ProfileTimeseriesSeries{PID: int(pid), ProcessStartMs: processStartMs, Comm: comm, Exe: exe,
				Runtime: firstNonEmpty(runtime, "python"), Metric: metricName, Unit: "bytes", Points: []ProfileTimeseriesPoint{}}
			byKey[key] = series
			seriesOrder = append(seriesOrder, key)
		}
		series.Points = append(series.Points, ProfileTimeseriesPoint{Timestamp: ts, Value: value})
		if value > series.Peak {
			series.Peak = value
		}
	}

	hours := pqHourlyRange(q.From, q.To)
	for _, hour := range hours {
		hFrom, hTo := pqHourBoundary(hour, q)
		if !hFrom.Before(hTo) {
			continue
		}
		block, err := s.pqFindBestBlock(ctx, tenant, hour, model.ContinuousParquetSignalMetrics)
		if err != nil {
			incParquetQueryError()
			return nil, true, err
		}
		if block == nil {
			incParquetV1Fallback()
			sub := q
			sub.From, sub.To = hFrom, hTo
			series, _, err := s.queryNativeContinuousTimeseries(ctx, sub, metricName, maxSeries)
			if err != nil {
				incParquetQueryError()
				return nil, true, err
			}
			for i := range series {
				// 按 timestamp 过滤到 [hFrom, hTo)，消除小时边界窗口双计
				series[i].Points = filterTimeseriesPoints(series[i].Points, hFrom, hTo)
				key := strconv.Itoa(int(series[i].PID)) + "|" + strconv.FormatInt(series[i].ProcessStartMs, 10) + "|" + series[i].Exe
				existing := byKey[key]
				if existing == nil {
					clone := series[i]
					clone.Points = append([]ProfileTimeseriesPoint{}, series[i].Points...)
					byKey[key] = &clone
					seriesOrder = append(seriesOrder, key)
					continue
				}
				existing.Points = append(existing.Points, series[i].Points...)
				if series[i].Peak > existing.Peak {
					existing.Peak = series[i].Peak
				}
			}
			continue
		}
		files, err := s.pqLoadBlockFiles(ctx, block.BlockID)
		if err != nil {
			incParquetQueryError()
			return nil, true, fmt.Errorf("hour %s 加载 block files 失败: %w", hour.UTC().Format(time.RFC3339), err)
		}
		readErr := error(nil)
		for _, file := range files {
			rows, err := readParquetRows[pqMetricRow](s, ctx, file.ObjectKey, hFrom.UnixMilli(), hTo.UnixMilli())
			if err != nil {
				readErr = err
				break
			}
			for i := range rows {
				row := &rows[i]
				if _, ok := authorized[row.SessionSID]; !ok {
					continue
				}
				if row.Metric != metricName || row.Timestamp < hFrom.UnixMilli() || row.Timestamp >= hTo.UnixMilli() {
					continue
				}
				addPoint(row.PID, row.ProcessStartMs, row.Comm, row.Exe, row.Runtime, time.UnixMilli(row.Timestamp), row.Value)
			}
		}
		if readErr != nil {
			incParquetQueryError()
			sub := q
			sub.From, sub.To = hFrom, hTo
			series, _, v1Err := s.queryNativeContinuousTimeseries(ctx, sub, metricName, maxSeries)
			if v1Err == nil {
				incParquetV1Fallback()
				for i := range series {
					series[i].Points = filterTimeseriesPoints(series[i].Points, hFrom, hTo)
					key := strconv.Itoa(int(series[i].PID)) + "|" + strconv.FormatInt(series[i].ProcessStartMs, 10) + "|" + series[i].Exe
					existing := byKey[key]
					if existing == nil {
						clone := series[i]
						clone.Points = append([]ProfileTimeseriesPoint{}, series[i].Points...)
						byKey[key] = &clone
						seriesOrder = append(seriesOrder, key)
						continue
					}
					existing.Points = append(existing.Points, series[i].Points...)
					if series[i].Peak > existing.Peak {
						existing.Peak = series[i].Peak
					}
				}
				continue
			}
			return nil, true, fmt.Errorf("%w: hour %s metrics 读取失败且 v1 回退不可用: %v",
				errPartialCoverage, hour.UTC().Format(time.RFC3339), readErr)
		}
	}

	out := make([]ProfileTimeseriesSeries, 0, len(seriesOrder))
	for _, key := range seriesOrder {
		series := byKey[key]
		sort.Slice(series.Points, func(i, j int) bool { return series.Points[i].Timestamp.Before(series.Points[j].Timestamp) })
		series.Points = downsampleRSSPoints(series.Points, 600)
		out = append(out, *series)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Peak == out[j].Peak {
			return out[i].PID < out[j].PID
		}
		return out[i].Peak > out[j].Peak
	})
	if len(out) > maxSeries {
		out = out[:maxSeries]
	}
	return out, len(out) > 0, nil
}

// pqHourBoundary 返回某小时与查询范围的交集 [hFrom, hTo)。
func pqHourBoundary(hour time.Time, q ProfileQuery) (time.Time, time.Time) {
	hFrom := hour
	hTo := hour.Add(time.Hour)
	if hFrom.Before(q.From) {
		hFrom = q.From
	}
	if hTo.After(q.To) {
		hTo = q.To
	}
	return hFrom, hTo
}

// ---------------------------------------------------------------------------
// Histogram
// ---------------------------------------------------------------------------

// pqBucketAgg histogram bucket 合并累加器（v1/v2 混合查询共享类型）。
type pqBucketAgg struct {
	Range string
	Low   float64
	High  float64
	Count uint64
}

// pqQueryHistogramMixed 延迟直方图逐小时混合查询（bucket 合并/趋势/分位数等价 v1）。
func (s *APIServer) pqQueryHistogramMixed(ctx context.Context, q ProfileQuery, signalType string) (gin.H, bool, error) {
	if !pqModeQueryV2(s.pqModeOf()) {
		return s.queryNativeContinuousHistogram(ctx, q, signalType)
	}
	authorized, err := s.pqAuthorizedSIDs(ctx, q)
	if err != nil {
		return nil, false, err
	}
	tenant := s.Config.ContinuousParquet.Tenant

	merged := map[string]*pqBucketAgg{}
	trend := []gin.H{}
	backends := map[string]bool{}
	var totalEvents uint64
	var unavailableReason string
	objectKeys := []string{}
	seenBlock := map[string]bool{}

	hours := pqHourlyRange(q.From, q.To)
	for _, hour := range hours {
		hFrom, hTo := pqHourBoundary(hour, q)
		if !hFrom.Before(hTo) {
			continue
		}
		block, err := s.pqFindBestBlock(ctx, tenant, hour, model.ContinuousParquetSignalHistogram)
		if err != nil {
			incParquetQueryError()
			return nil, true, err
		}
		if block == nil {
			incParquetV1Fallback()
			sub := q
			sub.From, sub.To = hFrom, hTo
			ok, err := s.mergeV1Histogram(ctx, sub, signalType, merged, &trend, backends, &totalEvents, &unavailableReason, &objectKeys)
			if err != nil {
				incParquetQueryError()
				return nil, true, err
			}
			if ok {
			}
			continue
		}
		if !seenBlock[block.BlockID] {
			seenBlock[block.BlockID] = true
			objectKeys = append(objectKeys, block.BlockID)
		}
		files, err := s.pqLoadBlockFiles(ctx, block.BlockID)
		if err != nil {
			incParquetQueryError()
			return nil, true, err
		}
		readErr := error(nil)
		for _, file := range files {
			rows, err := readParquetRows[pqHistogramRow](s, ctx, file.ObjectKey, hFrom.UnixMilli(), hTo.UnixMilli())
			if err != nil {
				readErr = err
				break
			}
			for i := range rows {
				row := &rows[i]
				if _, ok := authorized[row.SessionSID]; !ok {
					continue
				}
				if row.SignalType != signalType || row.Timestamp < hFrom.UnixMilli() || row.Timestamp >= hTo.UnixMilli() {
					continue
				}
				if row.Backend != "" {
					backends[row.Backend] = true
				}
				if row.Unavailable && unavailableReason == "" {
					unavailableReason = row.Reason
				}
				totalEvents = addContinuousCount(totalEvents, row.EventCount)
				key := row.BucketLowRange() + "|" + strconv.FormatFloat(row.BucketLow, 'f', -1, 64) + "|" + strconv.FormatFloat(row.BucketHigh, 'f', -1, 64)
				item := merged[key]
				if item == nil {
					item = &pqBucketAgg{Range: row.BucketLowRange(), Low: row.BucketLow, High: row.BucketHigh}
					merged[key] = item
				}
				item.Count = addContinuousCount(item.Count, row.Count)
			}
		}
		if readErr != nil {
			incParquetQueryError()
			sub := q
			sub.From, sub.To = hFrom, hTo
			if ok, _ := s.mergeV1Histogram(ctx, sub, signalType, merged, &trend, backends, &totalEvents, &unavailableReason, &objectKeys); ok {
				incParquetV1Fallback()
				continue
			}
			return nil, true, fmt.Errorf("%w: hour %s histogram 读取失败且 v1 回退不可用: %v",
				errPartialCoverage, hour.UTC().Format(time.RFC3339), readErr)
		}
		// v2 趋势点：按 (timestamp, event_count, p50/p95/p99) 聚合
		trend = append(trend, s.pqHistogramTrendFromFile(ctx, q, signalType, files, hFrom, hTo, authorized)...)
	}

	buckets := make([]ContinuousHistogramBucket, 0, len(merged))
	for _, item := range merged {
		buckets = append(buckets, ContinuousHistogramBucket{Range: item.Range, Low: item.Low, High: item.High, Count: item.Count})
	}
	sort.Slice(buckets, func(i, j int) bool {
		if buckets[i].Low == buckets[j].Low {
			return buckets[i].High < buckets[j].High
		}
		return buckets[i].Low < buckets[j].Low
	})
	summary := summarizeContinuousBuckets(buckets)
	backendList := make([]string, 0, len(backends))
	for backend := range backends {
		backendList = append(backendList, backend)
	}
	sort.Strings(backendList)
	empty := len(buckets) == 0 || totalEvents == 0
	message := ""
	if empty {
		message = firstNonEmpty(unavailableReason, "Native Continuous eBPF 暂无 histogram 样本")
	} else if unavailableReason != "" {
		message = "部分窗口不可用: " + unavailableReason
	}
	stats := s.pqStorageSourceForSignal(ctx, q, model.ContinuousParquetSignalHistogram)
	return gin.H{
		"query":                 profileLabelSelector(q),
		"signal_type":           signalType,
		"buckets":               buckets,
		"summary":               summary,
		"trend":                 trend,
		"event_count":           totalEvents,
		"unit":                  "us",
		"backend":               strings.Join(backendList, ","),
		"backends":              backendList,
		"empty":                 empty,
		"message":               message,
		"source":                "mini-drop-native",
		"profile_url":           s.continuousProfileURL(ctx, q, objectKeys),
		"raw_profile_url":       s.continuousRawProfileURL(ctx, objectKeys),
		"generated_at":          time.Now(),
		"storage_source":        stats.StorageSource,
		"resolution_seconds":    stats.ResolutionSeconds,
		"mixed_resolution":      stats.MixedResolution,
		"earliest_available_at": stats.EarliestAvailable,
	}, true, nil
}

// mergeV1Histogram 把 v1 子范围 histogram 结果并入混合累加器。
func (s *APIServer) mergeV1Histogram(ctx context.Context, q ProfileQuery, signalType string,
	merged map[string]*pqBucketAgg, trend *[]gin.H, backends map[string]bool,
	totalEvents *uint64, unavailableReason *string, objectKeys *[]string) (bool, error) {
	data, found, err := s.queryNativeContinuousHistogram(ctx, q, signalType)
	if err != nil || !found {
		return found, err
	}
	if buckets, ok := data["buckets"].([]ContinuousHistogramBucket); ok {
		for _, bucket := range buckets {
			key := bucket.Range + "|" + strconv.FormatFloat(bucket.Low, 'f', -1, 64) + "|" + strconv.FormatFloat(bucket.High, 'f', -1, 64)
			item := merged[key]
			if item == nil {
				item = &pqBucketAgg{Range: bucket.Range, Low: bucket.Low, High: bucket.High}
				merged[key] = item
			}
			item.Count = addContinuousCount(item.Count, bucket.Count)
		}
	}
	if trendData, ok := data["trend"].([]gin.H); ok {
		*trend = append(*trend, trendData...)
	}
	if backendList, ok := data["backends"].([]string); ok {
		for _, backend := range backendList {
			backends[backend] = true
		}
	}
	if events, ok := data["event_count"].(uint64); ok {
		*totalEvents = addContinuousCount(*totalEvents, events)
	}
	if reason, ok := data["message"].(string); ok && *unavailableReason == "" && strings.HasPrefix(reason, "部分窗口不可用") {
		*unavailableReason = strings.TrimPrefix(reason, "部分窗口不可用: ")
	}
	return true, nil
}

// pqHistogramTrendFromFile 从 v2 histogram 行构建趋势点（按窗口聚合 p50/p95/p99）。
func (s *APIServer) pqHistogramTrendFromFile(ctx context.Context, q ProfileQuery, signalType string,
	files []model.ContinuousParquetBlockFile, hFrom, hTo time.Time, authorized map[string]struct{}) []gin.H {
	type trendAcc struct {
		eventCount    uint64
		p50, p95, p99 float64
		countWeight   uint64
		unavailable   bool
		reason        string
		backend       string
	}
	acc := map[int64]*trendAcc{}
	for _, file := range files {
		rows, err := readParquetRows[pqHistogramRow](s, ctx, file.ObjectKey, hFrom.UnixMilli(), hTo.UnixMilli())
		if err != nil {
			continue
		}
		for i := range rows {
			row := &rows[i]
			if _, ok := authorized[row.SessionSID]; !ok || row.SignalType != signalType {
				continue
			}
			bucket := row.Timestamp / 60000 * 60000
			item := acc[bucket]
			if item == nil {
				item = &trendAcc{backend: row.Backend, unavailable: row.Unavailable, reason: row.Reason}
				acc[bucket] = item
			}
			if row.EventCount > item.eventCount {
				item.eventCount = row.EventCount
			}
			if row.Count > 0 {
				item.p50 += row.P50 * float64(row.Count)
				item.p95 += row.P95 * float64(row.Count)
				item.p99 += row.P99 * float64(row.Count)
				item.countWeight += row.Count
			}
			if row.Unavailable {
				item.unavailable = true
			}
		}
	}
	keys := make([]int64, 0, len(acc))
	for k := range acc {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]gin.H, 0, len(keys))
	for _, k := range keys {
		item := acc[k]
		p50, p95, p99 := item.p50, item.p95, item.p99
		if item.countWeight > 0 {
			p50 /= float64(item.countWeight)
			p95 /= float64(item.countWeight)
			p99 /= float64(item.countWeight)
		}
		out = append(out, gin.H{
			"window_start": time.UnixMilli(k),
			"window_end":   time.UnixMilli(k + 60000),
			"event_count":  item.eventCount,
			"p50":          p50,
			"p95":          p95,
			"p99":          p99,
			"backend":      item.backend,
			"unavailable":  item.unavailable,
			"reason":       item.reason,
		})
	}
	return out
}

// BucketLowRange 兼容 v1 bucket.Range 字段（低边界字符串）。
func (r *pqHistogramRow) BucketLowRange() string {
	return strconv.FormatFloat(r.BucketLow, 'f', -1, 64)
}

// pqDigestAgg v2/v1 DB digest 聚合累加器（混合查询共享类型）。
type pqDigestAgg struct {
	InstanceLabel  string
	SchemaName     string
	DigestText     string
	CallCount      uint64
	TotalLatencyUs uint64
	RowsExamined   uint64
}

// ---------------------------------------------------------------------------
// DB 快照
// ---------------------------------------------------------------------------

// pqQueryDBMixed 数据库快照逐小时混合查询（digest 累加/锁等待等价 v1）。
func (s *APIServer) pqQueryDBMixed(ctx context.Context, q ProfileQuery) (gin.H, bool, error) {
	if !pqModeQueryV2(s.pqModeOf()) {
		return s.queryNativeContinuousDBSnapshot(ctx, q)
	}
	authorized, err := s.pqAuthorizedSIDs(ctx, q)
	if err != nil {
		return nil, false, err
	}
	tenant := s.Config.ContinuousParquet.Tenant

	digests := map[string]*pqDigestAgg{}
	lockWaits := []gin.H{}
	objectKeys := []string{}
	seenBlock := map[string]bool{}

	hours := pqHourlyRange(q.From, q.To)
	for _, hour := range hours {
		hFrom, hTo := pqHourBoundary(hour, q)
		if !hFrom.Before(hTo) {
			continue
		}
		block, err := s.pqFindBestBlock(ctx, tenant, hour, model.ContinuousParquetSignalDB)
		if err != nil {
			incParquetQueryError()
			return nil, true, err
		}
		if block == nil {
			incParquetV1Fallback()
			sub := q
			sub.From, sub.To = hFrom, hTo
			ok, err := s.mergeV1DB(ctx, sub, digests, &lockWaits, &objectKeys)
			if err != nil {
				incParquetQueryError()
				return nil, true, err
			}
			if ok {
			}
			continue
		}
		if !seenBlock[block.BlockID] {
			seenBlock[block.BlockID] = true
			objectKeys = append(objectKeys, block.BlockID)
		}
		files, err := s.pqLoadBlockFiles(ctx, block.BlockID)
		if err != nil {
			incParquetQueryError()
			return nil, true, err
		}
		readErr := error(nil)
		for _, file := range files {
			rows, err := readParquetRows[pqDBRow](s, ctx, file.ObjectKey, hFrom.UnixMilli(), hTo.UnixMilli())
			if err != nil {
				readErr = err
				break
			}
			for i := range rows {
				row := &rows[i]
				if _, ok := authorized[row.SessionSID]; !ok {
					continue
				}
				if row.Timestamp < hFrom.UnixMilli() || row.Timestamp >= hTo.UnixMilli() {
					continue
				}
				switch row.Kind {
				case "digest":
					key := row.Instance + "|" + row.SchemaName + "|" + row.DigestText
					item := digests[key]
					if item == nil {
						item = &pqDigestAgg{InstanceLabel: row.Instance, SchemaName: row.SchemaName, DigestText: row.DigestText}
						digests[key] = item
					}
					item.CallCount = addContinuousCount(item.CallCount, row.CallCount)
					item.TotalLatencyUs = addContinuousCount(item.TotalLatencyUs, row.TotalLatencyUs)
					item.RowsExamined = addContinuousCount(item.RowsExamined, row.RowsExaminedTotal)
				case "lock_wait":
					lockWaits = append(lockWaits, gin.H{
						"instance_label":   row.Instance,
						"schema_name":      row.SchemaName,
						"waiting_pid":      row.WaitingPID,
						"waiting_query":    row.WaitingQuery,
						"blocking_pid":     row.BlockingPID,
						"blocking_query":   row.BlockingQuery,
						"wait_seconds":     row.WaitSeconds,
						"locked_table":     row.LockedTable,
						"occurrence_count": row.OccurrenceCount,
						"max_wait_seconds": row.MaxWaitSeconds,
						"timestamp":        time.UnixMilli(row.Timestamp),
					})
				}
			}
		}
		if readErr != nil {
			incParquetQueryError()
			sub := q
			sub.From, sub.To = hFrom, hTo
			if ok, _ := s.mergeV1DB(ctx, sub, digests, &lockWaits, &objectKeys); ok {
				incParquetV1Fallback()
				continue
			}
			return nil, true, fmt.Errorf("%w: hour %s db 读取失败且 v1 回退不可用: %v",
				errPartialCoverage, hour.UTC().Format(time.RFC3339), readErr)
		}
	}

	digestList := make([]gin.H, 0, len(digests))
	for _, item := range digests {
		digestList = append(digestList, gin.H{
			"instance_label":   item.InstanceLabel,
			"schema_name":      item.SchemaName,
			"digest_text":      item.DigestText,
			"call_count":       item.CallCount,
			"total_latency_us": item.TotalLatencyUs,
			"rows_examined":    item.RowsExamined,
		})
	}
	sort.Slice(digestList, func(i, j int) bool {
		a, _ := digestList[i]["total_latency_us"].(uint64)
		b, _ := digestList[j]["total_latency_us"].(uint64)
		if a == b {
			c, _ := digestList[i]["call_count"].(uint64)
			d, _ := digestList[j]["call_count"].(uint64)
			return c > d
		}
		return a > b
	})
	stats := s.pqStorageSourceForSignal(ctx, q, model.ContinuousParquetSignalDB)
	empty := len(digestList) == 0 && len(lockWaits) == 0
	return gin.H{
		"query":                 profileLabelSelector(q),
		"signal_type":           "db_snapshot",
		"digests":               digestList,
		"lock_waits":            lockWaits,
		"empty":                 empty,
		"source":                "mini-drop-native",
		"profile_url":           s.continuousProfileURL(ctx, q, objectKeys),
		"raw_profile_url":       s.continuousRawProfileURL(ctx, objectKeys),
		"generated_at":          time.Now(),
		"storage_source":        stats.StorageSource,
		"resolution_seconds":    stats.ResolutionSeconds,
		"mixed_resolution":      stats.MixedResolution,
		"earliest_available_at": stats.EarliestAvailable,
	}, true, nil
}

// mergeV1DB 把 v1 子范围 db 快照结果并入混合累加器。
func (s *APIServer) mergeV1DB(ctx context.Context, q ProfileQuery,
	digests map[string]*pqDigestAgg, lockWaits *[]gin.H, objectKeys *[]string) (bool, error) {
	data, found, err := s.queryNativeContinuousDBSnapshot(ctx, q)
	if err != nil || !found {
		return found, err
	}
	if list, ok := data["digests"].([]gin.H); ok {
		for _, item := range list {
			instance, _ := item["instance_label"].(string)
			schema, _ := item["schema_name"].(string)
			digestText, _ := item["digest_text"].(string)
			key := instance + "|" + schema + "|" + digestText
			agg := digests[key]
			if agg == nil {
				agg = &pqDigestAgg{InstanceLabel: instance, SchemaName: schema, DigestText: digestText}
				digests[key] = agg
			}
			if v, ok := item["call_count"].(uint64); ok {
				agg.CallCount = addContinuousCount(agg.CallCount, v)
			}
			if v, ok := item["total_latency_us"].(uint64); ok {
				agg.TotalLatencyUs = addContinuousCount(agg.TotalLatencyUs, v)
			}
			if v, ok := item["rows_examined"].(uint64); ok {
				agg.RowsExamined = addContinuousCount(agg.RowsExamined, v)
			}
		}
	}
	if list, ok := data["lock_waits"].([]gin.H); ok {
		*lockWaits = append(*lockWaits, list...)
	}
	return true, nil
}

// filterTimeseriesPoints 按 [from, to) 过滤时序点（v1 回退小时边界去重）。
func filterTimeseriesPoints(points []ProfileTimeseriesPoint, from, to time.Time) []ProfileTimeseriesPoint {
	if len(points) == 0 {
		return points
	}
	out := make([]ProfileTimeseriesPoint, 0, len(points))
	for _, point := range points {
		if point.Timestamp.Before(from) || !point.Timestamp.Before(to) {
			continue
		}
		out = append(out, point)
	}
	return out
}
