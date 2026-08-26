package server

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/mini-drop/apiserver/model"
)

// pqRuntimeDiagnosticsForNewBlock builds the compact companion metadata for a
// CPU block. Raw blocks aggregate the authoritative v2 language_status stored
// on their source windows; derived blocks copy the already aggregated source.
func (s *APIServer) pqRuntimeDiagnosticsForNewBlock(ctx context.Context, signalType, resolution, sourceBlockID string,
	from, to time.Time, members []model.ContinuousParquetBlockMember) ([]model.ContinuousParquetRuntimeDiagnostic, error) {
	if signalType != model.ContinuousParquetSignalCPU {
		return nil, nil
	}
	if resolution != model.ContinuousParquetResolutionRaw && sourceBlockID != "" {
		var rows []model.ContinuousParquetRuntimeDiagnostic
		if err := s.DB.WithContext(ctx).Where("block_id = ?", sourceBlockID).Find(&rows).Error; err != nil {
			return nil, err
		}
		for i := range rows {
			rows[i].ID = 0
			rows[i].BlockID = ""
			rows[i].CreatedAt = time.Time{}
			rows[i].UpdatedAt = time.Time{}
		}
		return rows, nil
	}

	bids := make([]string, 0, len(members))
	for _, member := range members {
		if member.SourceKind == "batch" && member.SourceRef != "" {
			bids = append(bids, member.SourceRef)
		}
	}
	var windows []model.ProfileWindow
	query := s.DB.WithContext(ctx).Where("window_start >= ? AND window_start < ?", from, to).
		Where("signal_type IN ?", []string{"cpu_profile", "cpu"}).Order("window_start ASC")
	if len(bids) > 0 {
		query = query.Where("batch_bid IN ?", bids)
	}
	if err := query.Find(&windows).Error; err != nil {
		return nil, err
	}
	return pqAggregateWindowRuntimeDiagnostics(windows), nil
}

func pqAggregateWindowRuntimeDiagnostics(windows []model.ProfileWindow) []model.ContinuousParquetRuntimeDiagnostic {
	bySession := map[string]*continuousAggregate{}
	for _, window := range windows {
		if len(window.SymbolRefs) == 0 {
			continue
		}
		refs := map[string]interface{}{}
		if err := json.Unmarshal(window.SymbolRefs, &refs); err != nil {
			continue
		}
		agg := bySession[window.SessionSID]
		if agg == nil {
			agg = &continuousAggregate{RuntimeDiagnostics: map[string]*runtimeDiagnosticAccumulator{}}
			bySession[window.SessionSID] = agg
		}
		continuousAggregateLanguageStatusV2(agg, refs)
		for _, acc := range agg.RuntimeDiagnostics {
			if window.WindowEnd.After(acc.ObservedAt) {
				acc.ObservedAt = window.WindowEnd
			}
		}
	}

	sessions := make([]string, 0, len(bySession))
	for sid := range bySession {
		sessions = append(sessions, sid)
	}
	sort.Strings(sessions)
	out := []model.ContinuousParquetRuntimeDiagnostic{}
	for _, sid := range sessions {
		names := make([]string, 0, len(bySession[sid].RuntimeDiagnostics))
		for name := range bySession[sid].RuntimeDiagnostics {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			acc := bySession[sid].RuntimeDiagnostics[name]
			if !acc.HasV2 || (acc.RuntimeDetection != "detected" && acc.V2SampleCount <= 0 && len(acc.Detected) == 0) {
				continue
			}
			processes := make([]ProfileRuntimeProcessDiagnostic, 0, len(acc.Detected))
			for _, process := range acc.Detected {
				processes = append(processes, process)
			}
			sort.Slice(processes, func(i, j int) bool {
				if processes[i].PID == processes[j].PID {
					return processes[i].ProcessStartMs < processes[j].ProcessStartMs
				}
				return processes[i].PID < processes[j].PID
			})
			out = append(out, model.ContinuousParquetRuntimeDiagnostic{
				SessionSID: sid, Runtime: name, Version: 2, Detection: acc.RuntimeDetection,
				Collector: acc.CollectorStatus, SymbolStatus: acc.SymbolStatusV2,
				Modes: mustJSONBytes(boolMapKeys(acc.Modes)), Reasons: mustJSONBytes(boolMapKeys(acc.Reasons)),
				Processes: mustJSONBytes(processes), LimitedCount: acc.Limited,
				FrameWeight: acc.FrameWeight, SemanticFrameWeight: acc.SemanticFrameWeight,
				UnresolvedFrameWeight: acc.UnresolvedFrameWeight, SampleWeight: acc.V2SampleCount,
				SemanticSampleWeight:         acc.SemanticSampleWeight,
				TargetModuleFrameWeight:      acc.TargetModuleFrameWeight,
				TargetModuleUnresolvedWeight: acc.TargetModuleUnresolvedWeight,
				ObservedAt:                   acc.ObservedAt,
			})
		}
	}
	return out
}

func pqPersistRuntimeDiagnosticsTx(tx *gorm.DB, blockID string, rows []model.ContinuousParquetRuntimeDiagnostic) error {
	now := time.Now()
	for i := range rows {
		rows[i].ID = 0
		rows[i].BlockID = blockID
		rows[i].CreatedAt = now
		rows[i].UpdatedAt = now
		if err := tx.Create(&rows[i]).Error; err != nil {
			return err
		}
	}
	return nil
}

// pqMergeRuntimeDiagnostics merges persisted raw weights into the same
// accumulator used by v1 queries, preserving the newest state while summing
// coverage weights across blocks/hours.
func pqMergeRuntimeDiagnostics(agg *continuousAggregate, rows []model.ContinuousParquetRuntimeDiagnostic, source string) {
	for _, row := range rows {
		acc := continuousRuntimeAccumulator(agg, row.Runtime)
		if acc.ObservedAt.IsZero() || !row.ObservedAt.Before(acc.ObservedAt) {
			acc.Modes = map[string]bool{}
			acc.Detected = map[string]ProfileRuntimeProcessDiagnostic{}
			acc.Ready = map[string]ProfileRuntimeProcessDiagnostic{}
			acc.Missing = map[string]ProfileRuntimeProcessDiagnostic{}
			acc.Reasons = map[string]bool{}
			for _, value := range decodeJSONStringSlice(row.Modes) {
				acc.Modes[value] = true
			}
			for _, value := range decodeJSONStringSlice(row.Reasons) {
				acc.Reasons[value] = true
			}
			var processes []ProfileRuntimeProcessDiagnostic
			_ = json.Unmarshal(row.Processes, &processes)
			for _, process := range processes {
				key := "pid|" + strconv.Itoa(process.PID) + "|" + strconv.FormatInt(process.ProcessStartMs, 10) + "|" + process.Exe
				acc.Detected[key] = process
				switch process.Status {
				case "ready":
					acc.Ready[key] = process
				case "missing", "failed", "pending":
					acc.Missing[key] = process
				}
			}
			acc.RuntimeDetection = row.Detection
			acc.CollectorStatus = row.Collector
			acc.Limited = row.LimitedCount
			acc.ObservedAt = row.ObservedAt
		}
		if symbolStatusRank(row.SymbolStatus) > symbolStatusRank(acc.SymbolStatusV2) {
			acc.SymbolStatusV2 = row.SymbolStatus
		}
		acc.FrameWeight += row.FrameWeight
		acc.SemanticFrameWeight += row.SemanticFrameWeight
		acc.UnresolvedFrameWeight += row.UnresolvedFrameWeight
		acc.V2SampleCount += row.SampleWeight
		acc.SemanticSampleWeight += row.SemanticSampleWeight
		acc.TargetModuleFrameWeight += row.TargetModuleFrameWeight
		acc.TargetModuleUnresolvedWeight += row.TargetModuleUnresolvedWeight
		acc.HasV2 = true
		acc.DiagnosticsVersion = row.Version
		if acc.DiagnosticsVersion == 0 {
			acc.DiagnosticsVersion = 2
		}
		acc.DiagnosticSource = mergeDiagnosticSource(acc.DiagnosticSource, source)
	}
}

func decodeJSONStringSlice(raw []byte) []string {
	var values []string
	_ = json.Unmarshal(raw, &values)
	return values
}

func symbolStatusRank(value string) int {
	return map[string]int{"": -1, "complete": 0, "partial": 1, "missing": 2, "unknown": 3, "not_applicable": -1}[value]
}

func mergeDiagnosticSource(current, next string) string {
	if current == "" || current == next {
		return next
	}
	if next == "" {
		return current
	}
	return "mixed"
}

func pqMarkUnknownRuntimeDiagnostics(agg *continuousAggregate, samples []ContinuousStackSample) {
	const reason = "历史 Parquet 块未保存语言诊断，无法确认运行时采集器状态"
	for _, sample := range samples {
		runtimeName := firstNonEmpty(sample.Runtime, labelString(sample.Labels, "runtime"), "unknown")
		acc := continuousRuntimeAccumulator(agg, runtimeName)
		if acc.HasV2 {
			continue
		}
		key := "pid|" + strconv.Itoa(sample.PID) + "|" + strconv.FormatInt(sample.ProcessStartMs, 10) + "|" + sample.Exe
		process := ProfileRuntimeProcessDiagnostic{PID: sample.PID, ProcessStartMs: sample.ProcessStartMs,
			Comm: sample.Comm, Exe: sample.Exe, Mode: sample.Backend, Status: "unknown", Reason: reason}
		acc.Detected[key] = process
		if sample.Backend != "" {
			acc.Modes[sample.Backend] = true
		}
		acc.Reasons[reason] = true
		acc.RuntimeDetection = "detected"
		acc.CollectorStatus = "unknown"
		acc.SymbolStatusV2 = "unknown"
		acc.HasV2 = true
		acc.DiagnosticsVersion = 1
		acc.DiagnosticSource = "legacy_parquet"
	}
}

// pqMergeDiagnosticsForQuery applies companion rows for a selected block. Old
// blocks are recovered from still-retained v1 window metadata; unrecoverable
// sessions are explicitly marked unknown instead of being inferred ready.
func (s *APIServer) pqMergeDiagnosticsForQuery(ctx context.Context, block *model.ContinuousParquetBlock,
	from, to time.Time, samplesBySession map[string][]ContinuousStackSample, agg *continuousAggregate) error {
	if block == nil || len(samplesBySession) == 0 {
		return nil
	}
	sids := make([]string, 0, len(samplesBySession))
	for sid := range samplesBySession {
		sids = append(sids, sid)
	}
	var persisted []model.ContinuousParquetRuntimeDiagnostic
	if err := s.DB.WithContext(ctx).Where("block_id = ? AND session_sid IN ?", block.BlockID, sids).
		Find(&persisted).Error; err != nil {
		return err
	}
	persistedSessions := map[string]bool{}
	for _, row := range persisted {
		persistedSessions[row.SessionSID] = true
	}
	pqMergeRuntimeDiagnostics(agg, persisted, "parquet_v2")

	missing := make([]string, 0, len(sids))
	for _, sid := range sids {
		if !persistedSessions[sid] {
			missing = append(missing, sid)
		}
	}
	if len(missing) > 0 {
		var windows []model.ProfileWindow
		if err := s.DB.WithContext(ctx).Where("session_sid IN ?", missing).
			Where("signal_type IN ?", []string{"cpu_profile", "cpu"}).
			Where("window_start >= ? AND window_start < ?", from, to).
			Order("window_start ASC").Find(&windows).Error; err != nil {
			return err
		}
		recovered := pqAggregateWindowRuntimeDiagnostics(windows)
		recoveredSessions := map[string]bool{}
		for i := range recovered {
			recoveredSessions[recovered[i].SessionSID] = true
			recovered[i].BlockID = block.BlockID
		}
		pqMergeRuntimeDiagnostics(agg, recovered, "profile_window_v1")
		// Best-effort compatibility backfill. Query correctness does not depend on
		// this write; a concurrent request/build may win the unique-key race.
		if len(recovered) > 0 {
			now := time.Now()
			for i := range recovered {
				recovered[i].CreatedAt = now
				recovered[i].UpdatedAt = now
			}
			_ = s.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&recovered).Error
		}
		for _, sid := range missing {
			if !recoveredSessions[sid] {
				pqMarkUnknownRuntimeDiagnostics(agg, samplesBySession[sid])
			}
		}
	}
	return nil
}
