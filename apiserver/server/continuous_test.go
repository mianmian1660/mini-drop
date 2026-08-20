// ============================================================
// server/continuous_test.go — continuous symbol diagnostics 单测
// ============================================================
// 覆盖（修复计划 Step 4 要求的单测）：
//   - 最终状态只取决于过滤后实际帧，不受 runtime 元数据干扰
//   - native Go ready/pending/failed 仅作为结构化诊断
// ============================================================

package server

import "testing"

func TestContinuousSymbolMetadataCollectsGoState(t *testing.T) {
	agg := &continuousAggregate{SymbolStatus: "not_applicable", SymbolReasons: map[string]bool{}}
	refs := map[string]interface{}{
		"symbol_status": "missing",
		"native_go": map[string]interface{}{
			"pending": []interface{}{map[string]interface{}{"build_id": "abc", "dso": "/usr/bin/containerd", "reason": "background extraction"}},
			"ready":   []interface{}{},
			"failed":  []interface{}{},
		},
	}
	continuousAggregateSymbolMetadata(agg, refs, map[string]bool{"/usr/bin/containerd": true})
	if !agg.GoSymbolPending || !agg.SymbolReasons["background extraction"] {
		t.Fatalf("expected pending Go metadata, got %#v", agg)
	}
	// Explicit legacy status is intentionally ignored.
	agg.TotalFrameWeight = 10
	agg.UnresolvedFrameWeight = 2
	continuousFinalizeSymbolStatus(agg)
	if agg.SymbolStatus != "partial" {
		t.Fatalf("expected frame-derived partial, got %q", agg.SymbolStatus)
	}
}

func TestContinuousSymbolMetadataExcludesUnrelatedDSO(t *testing.T) {
	agg := &continuousAggregate{SymbolStatus: "not_applicable", SymbolReasons: map[string]bool{}}
	refs := map[string]interface{}{
		"native_go": map[string]interface{}{
			"pending": []interface{}{map[string]interface{}{"build_id": "dockerd-id", "dso": "/usr/bin/dockerd", "reason": "background extraction"}},
			"ready":   []interface{}{map[string]interface{}{"build_id": "containerd-id", "dso": "/usr/bin/containerd"}},
			"failed":  []interface{}{},
		},
	}
	continuousAggregateSymbolMetadata(agg, refs, map[string]bool{"/usr/bin/containerd": true})
	if agg.GoSymbolPending || !agg.GoSymbolReady {
		t.Fatalf("expected only containerd ready metadata, got %#v", agg)
	}
	if len(agg.SymbolReasons) != 0 {
		t.Fatalf("unexpected unrelated reasons: %#v", agg.SymbolReasons)
	}
}

func TestContinuousFinalizeSymbolStatusFromFrames(t *testing.T) {
	tests := []struct {
		total, unresolved float64
		want              string
	}{
		{0, 0, "not_applicable"},
		{10, 0, "complete"},
		{10, 2, "partial"},
		{10, 10, "missing"},
	}
	for _, tt := range tests {
		agg := &continuousAggregate{TotalFrameWeight: tt.total, UnresolvedFrameWeight: tt.unresolved}
		continuousFinalizeSymbolStatus(agg)
		if agg.SymbolStatus != tt.want {
			t.Fatalf("total=%v unresolved=%v got=%q want=%q", tt.total, tt.unresolved, agg.SymbolStatus, tt.want)
		}
	}
}

func TestContinuousFrameLooksUnresolved(t *testing.T) {
	for _, frame := range []string{"0x5c0d180b8f25 [containerd]", "5c0d180b8f25", "[kernel.kallsyms]", "unknown"} {
		if !continuousFrameLooksUnresolved(frame) {
			t.Fatalf("expected unresolved frame: %q", frame)
		}
	}
	for _, frame := range []string{"runtime.main", "github.com/acme/pkg.Func", "net/http.(*Server).Serve"} {
		if continuousFrameLooksUnresolved(frame) {
			t.Fatalf("expected resolved frame: %q", frame)
		}
	}
}
