package server

import (
	"testing"
	"time"

	"github.com/mini-drop/apiserver/model"
)

func TestPQAggregatesAndRestoresRuntimeDiagnostics(t *testing.T) {
	now := time.Date(2026, 8, 26, 8, 34, 0, 0, time.UTC)
	refs := map[string]interface{}{
		"diagnostics_version": 2,
		"language_status": map[string]interface{}{
			"node": map[string]interface{}{
				"runtime_detection": "detected", "collector_status": "missing", "symbol_status": "missing",
				"collector_modes": []interface{}{}, "reasons": []interface{}{"missing --perf-basic-prof flag"},
				"sample_count": 9.0, "frame_weight": 18.0, "semantic_frame_weight": 0.0,
				"unresolved_frame_weight": 18.0, "semantic_sample_weight": 0.0,
				"processes": []interface{}{map[string]interface{}{
					"pid": 42.0, "process_start_ms": 1000.0, "exe": "/usr/bin/node", "mode": "perf-map",
					"status": "missing", "reason": "missing --perf-basic-prof flag",
				}},
			},
		},
	}
	rows := pqAggregateWindowRuntimeDiagnostics([]model.ProfileWindow{{
		SessionSID: "s-node", SignalType: "cpu_profile", WindowStart: now.Add(-time.Minute),
		WindowEnd: now, SymbolRefs: mustJSONBytes(refs),
	}})
	if len(rows) != 1 || rows[0].Runtime != "node" || rows[0].Collector != "missing" {
		t.Fatalf("unexpected persisted diagnostics: %+v", rows)
	}
	agg := continuousAggregate{RuntimeDiagnostics: map[string]*runtimeDiagnosticAccumulator{}}
	pqMergeRuntimeDiagnostics(&agg, rows, "parquet_v2")
	diag := continuousRuntimeDiagnostics(agg)["node"]
	if diag.CollectorStatus != "missing" || diag.Status != "missing" || diag.DiagnosticsVersion != 2 {
		t.Fatalf("unexpected restored diagnostics: %+v", diag)
	}
	if diag.DiagnosticSource != "parquet_v2" || len(diag.Reasons) != 1 || diag.UnresolvedFramePercent != 100 {
		t.Fatalf("diagnostic fidelity lost: %+v", diag)
	}
}

func TestPQLegacyBlockDoesNotInferCollectorReady(t *testing.T) {
	agg := continuousAggregate{
		Top:                           map[string]*ProfileTopItem{},
		Root:                          &continuousTreeNode{Name: "root", Children: map[string]*continuousTreeNode{}},
		LabelValue:                    map[string]map[string]bool{},
		Backends:                      map[string]bool{},
		RuntimeDiagnostics:            map[string]*runtimeDiagnosticAccumulator{},
		DisableLegacyRuntimeInference: true,
	}
	sample := ContinuousStackSample{Stack: []string{"0x123456"}, Count: 5, PID: 7,
		ProcessStartMs: 1234, Exe: "/tmp/node", Runtime: "node", Backend: "perf_rolling"}
	continuousAddSample(&agg, sample, nil)
	pqMarkUnknownRuntimeDiagnostics(&agg, []ContinuousStackSample{sample})
	diag := continuousRuntimeDiagnostics(agg)["node"]
	if diag.Status != "unknown" || diag.CollectorStatus != "unknown" {
		t.Fatalf("legacy parquet must be unknown, got %+v", diag)
	}
	if diag.DiagnosticSource != "legacy_parquet" || diag.ReadyCount != 0 {
		t.Fatalf("legacy parquet inferred ready: %+v", diag)
	}
}
