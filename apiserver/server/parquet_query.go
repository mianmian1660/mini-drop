// ============================================================
// server/parquet_query.go — 阶段五：v2 查询桥（prefer/enforce）
// ============================================================
// 查询选择顺序为同一时间片的 raw → 5m → 1h → v1：
//   - prefer/enforce：按小时查 v2 覆盖；完全覆盖走纯 v2，存在缺口整段
//     回退 v1（保证不重不漏；逐小时混合读取在 Release C 观察期后启用）。
//   - shadow/off：查询完全走 v1。
// 响应增加兼容字段：resolution_seconds / mixed_resolution /
// storage_source / earliest_available_at。
//
// v2 块跨多个 session，禁止通过现有 raw object-key 接口直接下载；只能
// 经过已鉴权查询层访问（MinIO 策略对 continuous/v2/* 禁止匿名读取）。
// ============================================================

package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/mini-drop/apiserver/model"
)

// pqHourlyRange 返回 [from, to] 覆盖的 UTC 小时桶列表。
func pqHourlyRange(from, to time.Time) []time.Time {
	if !from.Before(to) {
		return nil
	}
	start := from.UTC().Truncate(time.Hour)
	// 查询是半开区间 [from,to)，整点 to 不应产生一个空的尾部小时。
	end := to.UTC().Add(-time.Nanosecond).Truncate(time.Hour)
	var hours []time.Time
	for current := start; !current.After(end); current = current.Add(time.Hour) {
		hours = append(hours, current)
	}
	return hours
}

// pqReadBlockRows 先完整读取一个 block 的所有 part。调用方只在成功后
// 合并到最终结果，保证任一 part 失败时可以原子地回退整个小时到 v1。
func pqReadBlockRows[T any](s *APIServer, ctx context.Context, blockID string, fromMS, toMS int64) ([]T, []model.ContinuousParquetBlockFile, error) {
	files, err := s.pqLoadBlockFiles(ctx, blockID)
	if err != nil {
		return nil, nil, err
	}
	rows := []T{}
	for _, file := range files {
		part, err := readParquetRows[T](s, ctx, file.ObjectKey, fromMS, toMS)
		if err != nil {
			return nil, files, err
		}
		rows = append(rows, part...)
	}
	return rows, files, nil
}

// pqV2CPUCoverage 查询 [from,to] 的 v2 CPU 覆盖情况。
// 返回 (覆盖小时数, 缺口小时数, 是否完全覆盖)。
func (s *APIServer) pqV2CPUCoverage(ctx context.Context, q ProfileQuery) (int, int, bool) {
	tenant := s.Config.ContinuousParquet.Tenant
	hours := pqHourlyRange(q.From, q.To)
	covered := 0
	for _, hour := range hours {
		// v2 块按 cpu 信号登记（raw 层含全部 profile 类型）
		block, err := s.pqFindBestBlock(ctx, tenant, hour, model.ContinuousParquetSignalCPU)
		if err == nil && block != nil {
			covered++
		}
	}
	return covered, len(hours) - covered, covered == len(hours) && len(hours) > 0
}

// pqSampleFromCPURow pqCPURow → ContinuousStackSample（复用 v1 聚合器）。
func pqSampleFromCPURow(row pqCPURow) ContinuousStackSample {
	sample := ContinuousStackSample{
		Count:          row.Value,
		Comm:           row.Comm,
		PID:            int(row.PID),
		ProcessStartMs: row.ProcessStartMs,
		Exe:            row.Exe,
		StackScope:     "continuous",
		Backend:        row.Backend,
		Runtime:        row.Runtime,
		// 阶段七：profile_id 随行保留，供 v2 查询按与 v1 相同的
		// (profile_id + 进程身份) 键跨窗口去重。
		ProfileID: row.ProfileID,
	}
	if len(row.Frames) > 0 {
		frames := make([]ContinuousStackFrame, 0, len(row.Frames))
		stack := make([]string, 0, len(row.Frames))
		for _, frame := range row.Frames {
			frames = append(frames, ContinuousStackFrame{
				Function: frame.Function, Raw: frame.Raw, File: frame.File, Line: frame.Line,
				Address: frame.Address, MappingFile: frame.MappingFile, BuildID: frame.BuildID,
				NormalizedOffset: frame.NormalizedOffset, Resolved: frame.Resolved,
			})
			stack = append(stack, frameDisplayName(ContinuousStackFrame{
				Function: frame.Function, Raw: frame.Raw, Address: frame.Address, MappingFile: frame.MappingFile,
			}))
		}
		sample.Frames = frames
		sample.Stack = stack
	}
	return sample
}

func continuousProfileSampleSeen(seen map[string]bool, sample ContinuousStackSample) bool {
	if sample.ProfileID == "" {
		return false
	}
	key := sample.ProfileID + "|" + strconv.Itoa(sample.PID) + "|" + strconv.FormatInt(sample.ProcessStartMs, 10) + "|" + sample.Exe + "|"
	if len(sample.Frames) > 0 {
		key += pqStackKey(framesToParquet(sample.Frames))
	} else {
		key += strings.Join(sample.Stack, "\x00") + "|" + sample.StackString
	}
	if seen[key] {
		return true
	}
	seen[key] = true
	return false
}

func continuousProfileSampleKey(sample ContinuousStackSample) string {
	key := sample.ProfileID + "|" + strconv.Itoa(sample.PID) + "|" + strconv.FormatInt(sample.ProcessStartMs, 10) + "|" + sample.Exe + "|"
	if len(sample.Frames) > 0 {
		return key + pqStackKey(framesToParquet(sample.Frames))
	}
	return key + strings.Join(sample.Stack, "\x00") + "|" + sample.StackString
}

func continuousProfileSampleSeenAt(seen map[string]int64, sample ContinuousStackSample, timestamp int64) bool {
	if sample.ProfileID == "" {
		return false
	}
	key := continuousProfileSampleKey(sample)
	if previous, ok := seen[key]; ok {
		return previous != timestamp
	}
	seen[key] = timestamp
	return false
}

// pqLabelsInterface map[string]string → map[string]interface{}。
func pqLabelsInterface(labels map[string]string) map[string]interface{} {
	if len(labels) == 0 {
		return nil
	}
	out := make(map[string]interface{}, len(labels))
	for key, value := range labels {
		out[key] = value
	}
	return out
}

// pqQueryAggregateMixed 逐小时混合规划器（阶段六）：
//
//	每个 UTC 小时按 raw→5m→1h 找最佳已验证（validation=passed AND
//	reconcile_status=passed）Block；当前未封存小时和最近热窗口从 v1 staging
//	读取；一个请求可同时读 Parquet 历史小时 + v1 当前小时。
//	半开区间 [start,end)，请求最后一个时间点单独处理，避免小时边界重复计数。
//	某小时 Parquet 读取失败：热表仍存在则仅该小时回退 v1；热表已清理则返回
//	明确的部分覆盖/依赖错误（errPartialCoverage），绝不静默返回不完整结果。
func (s *APIServer) pqQueryAggregateMixed(ctx context.Context, q ProfileQuery) (continuousAggregate, bool, error) {
	tenant := s.Config.ContinuousParquet.Tenant
	signalType := "cpu_profile"
	if q.ProfileType == "memory" {
		signalType = "python_memory"
	}
	var authorizedSIDs []string
	if err := s.continuousSessionSelection(q).Pluck("sid", &authorizedSIDs).Error; err != nil {
		return continuousAggregate{}, false, err
	}
	authorized := make(map[string]struct{}, len(authorizedSIDs))
	for _, sid := range authorizedSIDs {
		authorized[sid] = struct{}{}
	}

	agg := continuousAggregate{
		Top:  map[string]*ProfileTopItem{},
		Root: &continuousTreeNode{Name: "root", Children: map[string]*continuousTreeNode{}},
		LabelValue: map[string]map[string]bool{
			"comm": {}, "pid": {}, "process_start_ms": {}, "process_instance": {},
			"exe": {}, "runtime": {},
		},
		Backends:           map[string]bool{},
		SymbolStatus:       "not_applicable",
		SymbolReasons:      map[string]bool{},
		Unit:               map[bool]string{true: "bytes", false: "samples"}[q.ProfileType == "memory"],
		RuntimeDiagnostics: map[string]*runtimeDiagnosticAccumulator{},
		SeenProfileIDs:     map[string]bool{},
		SeenProfileSamples: map[string]int64{},
	}

	hours := pqHourlyRange(q.From, q.To)
	if len(hours) == 0 {
		return agg, false, nil
	}
	anyData := false
	for _, hour := range hours {
		hFrom := hour
		hTo := hour.Add(time.Hour)
		if hFrom.Before(q.From) {
			hFrom = q.From
		}
		if hTo.After(q.To) {
			hTo = q.To
		}
		if !hFrom.Before(hTo) {
			continue
		}
		block, err := s.pqFindBestBlock(ctx, tenant, hour, model.ContinuousParquetSignalCPU)
		if err != nil {
			incParquetQueryError()
			return agg, true, err
		}
		if block == nil {
			// 该小时无 v2 → v1 staging 回退（当前小时/热窗口）
			incParquetV1Fallback()
			found, v1Err := s.aggregateV1WindowsInto(ctx, q, hFrom, hTo, &agg)
			if v1Err != nil {
				incParquetQueryError()
				return agg, true, fmt.Errorf("hour %s v1 fallback 失败: %w", hour.UTC().Format(time.RFC3339), v1Err)
			}
			if found {
				anyData = true
			}
			continue
		}
		rows, _, readErr := pqReadBlockRows[pqCPURow](s, ctx, block.BlockID, hFrom.UnixMilli(), hTo.UnixMilli())
		if readErr == nil {
			for i := range rows {
				row := &rows[i]
				if _, ok := authorized[row.SessionSID]; !ok {
					continue
				}
				// 半开区间 [hFrom, hTo)。
				if row.Timestamp < hFrom.UnixMilli() {
					continue
				}
				if row.Timestamp >= hTo.UnixMilli() {
					continue
				}
				if row.ProfileType != signalType && !(signalType == "cpu_profile" && row.ProfileType == "cpu") {
					continue
				}
				sample := pqSampleFromCPURow(*row)
				if row.ProfileStatus == "failed" || row.ProfileStatus == "duplicate" {
					continue
				}
				if !continuousSampleMatches(sample, pqLabelsInterface(row.Labels), q.Filters) {
					continue
				}
				// 阶段七：v2 与 v1 同一 profile 去重语义。同一 profile 的
				// 样本可能因共享引擎/窗口边界出现在多个小时块，按
				// (profile_id + pid + start + exe) 只消费一次，防止跨层
				// 双计。旧 Parquet 无 profile_id（空串）不参与去重。
				if row.ProfileID != "" {
					if continuousProfileSampleSeenAt(agg.SeenProfileSamples, sample, row.Timestamp) {
						continue
					}
				}
				continuousAddSample(&agg, sample, pqLabelsInterface(row.Labels))
				agg.WindowCount++
			}
		}
		if readErr != nil {
			// Parquet 读取失败：热表仍存在则仅该小时回退 v1；热表已清理则
			// 返回明确的部分覆盖/依赖错误。
			incParquetQueryError()
			found, v1Err := s.aggregateV1WindowsInto(ctx, q, hFrom, hTo, &agg)
			if v1Err == nil && found {
				incParquetV1Fallback()
				anyData = true
				continue
			}
			return agg, true, fmt.Errorf("%w: hour %s parquet 读取失败且 v1 回退不可用: %v",
				errPartialCoverage, hour.UTC().Format(time.RFC3339), readErr)
		}
		agg.ObjectKeys = append(agg.ObjectKeys, block.BlockID)
		anyData = true
	}
	continuousFinalizeSymbolStatus(&agg)
	return agg, anyData, nil
}

// errPartialCoverage 明确的部分覆盖错误（热表已清理且 parquet 缺失/失败）。
var errPartialCoverage = errors.New("parquet 覆盖缺失且 v1 热表已清理，存在数据缺口")

// pqResolveStorageSource 返回查询使用的存储来源与分辨率（响应兼容字段）。
type pqResolveStorageSource struct {
	ResolutionSeconds int        `json:"resolution_seconds"`
	MixedResolution   bool       `json:"mixed_resolution"`
	StorageSource     string     `json:"storage_source"`
	EarliestAvailable *time.Time `json:"earliest_available_at"`
}

// pqStorageSourceFor 计算查询的存储来源描述（阶段六：逐小时混合语义）。
//   - 全部小时走 v2 → storage_source=parquet_v2
//   - 全部小时走 v1 → storage_source=parquet_v1
//   - 混合 → storage_source=parquet_v2（v2 已接管），mixed_resolution=true
//   - resolution_seconds 表示本次查询使用的最粗粒度（v1 视为 60s raw）
func (s *APIServer) pqStorageSourceFor(ctx context.Context, q ProfileQuery) pqResolveStorageSource {
	out := pqResolveStorageSource{
		ResolutionSeconds: 0,
		MixedResolution:   false,
		StorageSource:     "parquet_v1",
	}
	mode := s.pqModeOf()
	if !pqModeQueryV2(mode) {
		out.EarliestAvailable = s.pqEarliestAvailableAt(ctx)
		return out
	}
	hours := pqHourlyRange(q.From, q.To)
	tenant := s.Config.ContinuousParquet.Tenant
	v2Hours := 0
	v1Hours := 0
	coarsest := 0
	resolutions := map[string]bool{}
	for _, hour := range hours {
		block, err := s.pqFindBestBlock(ctx, tenant, hour, model.ContinuousParquetSignalCPU)
		if err != nil || block == nil {
			v1Hours++
			if coarsest < int(pqSignalResolutionSeconds(model.ContinuousParquetResolutionRaw)) {
				coarsest = int(pqSignalResolutionSeconds(model.ContinuousParquetResolutionRaw))
			}
			continue
		}
		v2Hours++
		resolutions[block.Resolution] = true
		resSec := int(pqSignalResolutionSeconds(block.Resolution))
		if resSec > coarsest {
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

// pqAggregateQueryStats 查询统计（返回给 handler 拼装响应）。
type pqAggregateQueryStats struct {
	ResolutionSeconds int        `json:"resolution_seconds"`
	MixedResolution   bool       `json:"mixed_resolution"`
	StorageSource     string     `json:"storage_source"`
	EarliestAvailable *time.Time `json:"earliest_available_at"`
}

// pqQueryStatsFor 供各 handler 复用的查询统计。
func (s *APIServer) pqQueryStatsFor(ctx context.Context, q ProfileQuery) pqAggregateQueryStats {
	source := s.pqStorageSourceFor(ctx, q)
	return pqAggregateQueryStats{
		ResolutionSeconds: source.ResolutionSeconds,
		MixedResolution:   source.MixedResolution,
		StorageSource:     source.StorageSource,
		EarliestAvailable: source.EarliestAvailable,
	}
}

// pqHourKeys 小时桶去重排序（查询聚合辅助）。
func pqHourKeys(hours []time.Time) []string {
	set := map[string]bool{}
	for _, hour := range hours {
		set[strconv.FormatInt(hour.Unix(), 10)] = true
	}
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
