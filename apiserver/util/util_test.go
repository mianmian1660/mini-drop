package util

import (
	"testing"
)

type testParams struct {
	TargetPID int32  `json:"target_pid"`
	Duration  uint64 `json:"duration"`
	Frequency uint32 `json:"frequency"`
	Callgraph string `json:"callgraph"`
	Event     string `json:"event"`
}

func TestGenTID(t *testing.T) {
	tid1 := GenTID()
	tid2 := GenTID()
	if tid1 == "" || len(tid1) < 6 {
		t.Error("GenTID() too short or empty")
	}
	if tid1 == tid2 {
		t.Error("GenTID() should be unique")
	}
}

func TestMarshalUnmarshalJSONB(t *testing.T) {
	tests := []testParams{
		{TargetPID: 1234, Duration: 10, Frequency: 99, Callgraph: "fp", Event: ""},
		{TargetPID: 0, Duration: 30, Frequency: 1, Callgraph: "fp", Event: "io"},
		{TargetPID: 5678, Duration: 60, Frequency: 0, Callgraph: "dwarf", Event: "localhost:6060"},
	}
	for _, tc := range tests {
		jb, err := MarshalJSONB(tc)
		if err != nil {
			t.Fatalf("MarshalJSONB error: %v", err)
		}
		if jb == nil {
			t.Error("MarshalJSONB returned nil")
		}
		var restored testParams
		if err := UnmarshalJSONB(jb, &restored); err != nil {
			t.Fatalf("UnmarshalJSONB error: %v", err)
		}
		if restored.TargetPID != tc.TargetPID || restored.Duration != tc.Duration ||
			restored.Frequency != tc.Frequency || restored.Callgraph != tc.Callgraph ||
			restored.Event != tc.Event {
			t.Errorf("roundtrip mismatch: got %+v want %+v", restored, tc)
		}
	}
}

func TestMarshalJSONB_Nil(t *testing.T) {
	result, err := MarshalJSONB(nil)
	if err != nil {
		t.Fatalf("MarshalJSONB(nil) error: %v", err)
	}
	if result == nil {
		t.Error("MarshalJSONB(nil) should return null bytes")
	}
}

func TestMarshalJSONB_Empty(t *testing.T) {
	result, err := MarshalJSONB(struct{}{})
	if err != nil {
		t.Fatalf("MarshalJSONB(empty) error: %v", err)
	}
	if result == nil || len(result) == 0 {
		t.Error("MarshalJSONB(empty) should produce valid JSON")
	}
}
