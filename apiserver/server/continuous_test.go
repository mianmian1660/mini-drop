// ============================================================
// server/continuous_test.go — continuous symbol_status 聚合单测
// ============================================================
// 覆盖（修复计划 Step 4 要求的单测）：
//   - 显式 symbol_status（Agent 的 runtime map 诊断）优先于旧启发式
//   - 旧批次无 symbol_refs 时保持兼容（仍是 not_applicable）
//   - 已聚合出非默认状态时不被后续窗口覆盖
// ============================================================

package server

import "testing"

func TestContinuousAggregateSymbolStatusPrefersExplicit(t *testing.T) {
	agg := &continuousAggregate{SymbolStatus: "not_applicable"}
	refs := map[string]interface{}{
		"symbol_status": "partial",
		"runtime_maps": map[string]interface{}{
			"node": map[string]interface{}{
				"detected": true,
				"ready":    false,
				"missing":  []interface{}{float64(12345)},
				"reason":   "missing --perf-basic-prof flag",
			},
		},
	}
	continuousAggregateSymbolStatus(agg, refs)
	if agg.SymbolStatus != "partial" {
		t.Fatalf("expected partial (explicit symbol_status), got %q", agg.SymbolStatus)
	}
}

func TestContinuousAggregateSymbolStatusExplicitComplete(t *testing.T) {
	agg := &continuousAggregate{SymbolStatus: "not_applicable"}
	refs := map[string]interface{}{
		"symbol_status": "complete",
		"runtime_maps":  map[string]interface{}{"java": map[string]interface{}{"detected": false}},
	}
	continuousAggregateSymbolStatus(agg, refs)
	if agg.SymbolStatus != "complete" {
		t.Fatalf("expected complete, got %q", agg.SymbolStatus)
	}
}

func TestContinuousAggregateSymbolStatusEmptyRefsCompatible(t *testing.T) {
	// 旧批次没有 symbol_refs：保持 not_applicable（兼容旧行为）
	agg := &continuousAggregate{SymbolStatus: "not_applicable"}
	continuousAggregateSymbolStatus(agg, nil)
	if agg.SymbolStatus != "not_applicable" {
		t.Fatalf("expected not_applicable for empty refs, got %q", agg.SymbolStatus)
	}

	agg2 := &continuousAggregate{SymbolStatus: "not_applicable"}
	continuousAggregateSymbolStatus(agg2, map[string]interface{}{})
	if agg2.SymbolStatus != "not_applicable" {
		t.Fatalf("expected not_applicable for empty map, got %q", agg2.SymbolStatus)
	}
}

func TestContinuousAggregateSymbolStatusKeepsWorstWindowStatus(t *testing.T) {
	// 范围内任一窗口 missing 都必须保留，不能被其他窗口的 complete 掩盖。
	agg := &continuousAggregate{SymbolStatus: "complete"}
	refs := map[string]interface{}{"symbol_status": "missing"}
	continuousAggregateSymbolStatus(agg, refs)
	if agg.SymbolStatus != "missing" {
		t.Fatalf("expected missing retained, got %q", agg.SymbolStatus)
	}
}

func TestContinuousAggregateSymbolStatusMergesExplicitWindowStatuses(t *testing.T) {
	agg := &continuousAggregate{SymbolStatus: "not_applicable"}
	continuousAggregateSymbolStatus(agg, map[string]interface{}{"symbol_status": "complete"})
	continuousAggregateSymbolStatus(agg, map[string]interface{}{"symbol_status": "partial"})
	if agg.SymbolStatus != "partial" {
		t.Fatalf("expected partial, got %q", agg.SymbolStatus)
	}
}
