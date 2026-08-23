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
	"sort"
	"strconv"
	"time"

	"github.com/mini-drop/apiserver/model"
)

// pqHourlyRange 返回 [from, to] 覆盖的 UTC 小时桶列表。
func pqHourlyRange(from, to time.Time) []time.Time {
	start := from.UTC().Truncate(time.Hour)
	end := to.UTC().Truncate(time.Hour)
	var hours []time.Time
	for current := start; !current.After(end); current = current.Add(time.Hour) {
		hours = append(hours, current)
	}
	return hours
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

// pqQueryAggregateV2 纯 v2 聚合（CPU/profile 信号）。
// 返回 (agg, found)。found=false 表示无 v2 覆盖。
func (s *APIServer) pqQueryAggregateV2(ctx context.Context, q ProfileQuery) (continuousAggregate, bool, error) {
	tenant := s.Config.ContinuousParquet.Tenant
	signalType := "cpu_profile"
	if q.ProfileType == "memory" {
		signalType = "python_memory"
	}
	fromMS := q.From.UnixMilli()
	toMS := q.To.UnixMilli()

	agg := continuousAggregate{
		Top:            map[string]*ProfileTopItem{},
		Root:           &continuousTreeNode{Name: "root", Children: map[string]*continuousTreeNode{}},
		LabelValue:     map[string]map[string]bool{
			"comm": {}, "pid": {}, "process_start_ms": {}, "process_instance": {},
			"exe": {}, "runtime": {},
		},
		Backends:           map[string]bool{},
		SymbolStatus:       "not_applicable",
		SymbolReasons:      map[string]bool{},
		Unit:               map[bool]string{true: "bytes", false: "samples"}[q.ProfileType == "memory"],
		RuntimeDiagnostics: map[string]*runtimeDiagnosticAccumulator{},
		SeenProfileIDs:     map[string]bool{},
	}

	hours := pqHourlyRange(q.From, q.To)
	foundAny := false
	for _, hour := range hours {
		block, err := s.pqFindBestBlock(ctx, tenant, hour, model.ContinuousParquetSignalCPU)
		if err != nil {
			return continuousAggregate{}, false, err
		}
		if block == nil {
			continue
		}
		foundAny = true
		agg.ObjectKeys = append(agg.ObjectKeys, block.BlockID)
		files, err := s.pqLoadBlockFiles(ctx, block.BlockID)
		if err != nil {
			return continuousAggregate{}, false, err
		}
		for _, file := range files {
			rows, err := readParquetRows[pqCPURow](s, ctx, file.ObjectKey, fromMS, toMS)
			if err != nil {
				return continuousAggregate{}, false, err
			}
			for i := range rows {
				row := &rows[i]
				if row.Timestamp < fromMS || row.Timestamp > toMS {
					continue
				}
				if row.ProfileType != signalType && !(signalType == "cpu_profile" && row.ProfileType == "cpu") {
					continue
				}
				sample := pqSampleFromCPURow(*row)
				if !continuousSampleMatches(sample, pqLabelsInterface(row.Labels), q.Filters) {
					continue
				}
				continuousAddSample(&agg, sample, pqLabelsInterface(row.Labels))
				agg.WindowCount++
			}
		}
	}
	if !foundAny {
		return continuousAggregate{}, false, nil
	}
	continuousFinalizeSymbolStatus(&agg)
	return agg, true, nil
}

// pqResolveStorageSource 返回查询使用的存储来源与分辨率（响应兼容字段）。
type pqResolveStorageSource struct {
	ResolutionSeconds  int    `json:"resolution_seconds"`
	MixedResolution    bool   `json:"mixed_resolution"`
	StorageSource      string `json:"storage_source"`
	EarliestAvailable  *time.Time `json:"earliest_available_at"`
}

// pqStorageSourceFor 计算查询的存储来源描述。
func (s *APIServer) pqStorageSourceFor(ctx context.Context, q ProfileQuery) pqResolveStorageSource {
	out := pqResolveStorageSource{
		ResolutionSeconds: 0,
		MixedResolution:   false,
		StorageSource:     "parquet_v1",
	}
	mode := s.pqModeOf()
	if pqModeQueryV2(mode) {
		out.StorageSource = "parquet_v2"
	}
	hours := pqHourlyRange(q.From, q.To)
	if len(hours) > 0 && pqModeQueryV2(mode) {
		tenant := s.Config.ContinuousParquet.Tenant
		resolutions := map[string]bool{}
		covered := 0
		for _, hour := range hours {
			block, err := s.pqFindBestBlock(ctx, tenant, hour, model.ContinuousParquetSignalCPU)
			if err != nil || block == nil {
				out.MixedResolution = true
				continue
			}
			covered++
			resolutions[block.Resolution] = true
			switch block.Resolution {
			case model.ContinuousParquetResolutionRaw:
				out.ResolutionSeconds = 0 // 原始 10s 窗口
			case model.ContinuousParquetResolution5m:
				if out.ResolutionSeconds == 0 {
					out.ResolutionSeconds = 300
				}
			case model.ContinuousParquetResolution1h:
				if out.ResolutionSeconds == 0 {
					out.ResolutionSeconds = 3600
				}
			}
		}
		if len(resolutions) > 1 {
			out.MixedResolution = true
		}
		if covered == 0 {
			out.StorageSource = "parquet_v1"
		}
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
