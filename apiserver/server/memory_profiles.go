// ============================================================
// server/memory_profiles.go — 阶段七：Memray profile 元数据查询
// ============================================================
// 提供 GET /api/v1/continuous/memory/profiles：返回时间范围内显式
// Mini-Drop/Memray SDK 上报的 allocation profile 列表（profile_id、
// 时间窗口、进程身份、样本数、状态）。前端 Memory 视图据此展示
// "SDK 接入状态 / 完整性 / 时间范围 / 错误状态"，并把每个 profile
// 与 CPU 时间范围关联展示。
//
// 状态语义：
//   - ready：profile 有可解析样本（正常消费）
//   - failed：profile 存在但转换/解析失败（reason 说明）
//   - duplicate：同一 (profile_id + 进程身份) 在查询范围内出现多次，
//     仅首次计 ready，其余计 duplicate（与 v1/v2 查询去重同一语义）
//
// 查询路径：v1 热窗口（profile_windows + 对象存储）与 v2 Parquet
// （CPU 块含 profile_id 列）逐小时混合，与 pqQueryAggregateMixed 相同
// 的规划语义。
// ============================================================

package server

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mini-drop/apiserver/model"
)

// ContinuousMemoryProfileInfo 单个 Memray profile 的元数据。
type ContinuousMemoryProfileInfo struct {
	ProfileID      string    `json:"profile_id"`
	SessionSID     string    `json:"session_sid"`
	WindowStart    time.Time `json:"window_start"`
	WindowEnd      time.Time `json:"window_end"`
	PID            int       `json:"pid"`
	ProcessStartMs int64     `json:"process_start_ms"`
	Comm           string    `json:"comm"`
	Exe            string    `json:"exe"`
	Backend        string    `json:"backend"`
	SampleCount    uint64    `json:"sample_count"`
	Unit           string    `json:"unit"`
	Status         string    `json:"status"`
	Reason         string    `json:"reason"`
}

// GetContinuousMemoryProfiles 返回时间范围内的 Memray profile 元数据列表。
func (s *APIServer) GetContinuousMemoryProfiles(c *gin.Context) {
	q, ok := s.profileQueryFromRequest(c)
	if !ok {
		return
	}
	profiles, found, err := s.queryMemoryProfilesMixed(c.Request.Context(), q)
	if err != nil {
		s.respondProfileDependencyError(c, err)
		return
	}
	if !found {
		profiles = []ContinuousMemoryProfileInfo{}
	}
	stats := s.pqQueryStatsFor(c.Request.Context(), q)
	// 按窗口时间倒序（最新在前）。
	sort.Slice(profiles, func(i, j int) bool {
		if profiles[i].WindowStart.Equal(profiles[j].WindowStart) {
			return profiles[i].ProfileID < profiles[j].ProfileID
		}
		return profiles[i].WindowStart.After(profiles[j].WindowStart)
	})
	s.RespondOK(c, gin.H{
		"profiles": profiles, "total": len(profiles), "empty": len(profiles) == 0,
		"generated_at":   time.Now(),
		"storage_source": stats.StorageSource, "resolution_seconds": stats.ResolutionSeconds,
		"mixed_resolution": stats.MixedResolution, "earliest_available_at": stats.EarliestAvailable,
	})
}

// queryMemoryProfilesMixed 逐小时混合查询 Memray profile 元数据：
// v2 小时读 Parquet CPU 块（profile_id 列），无 v2 覆盖的小时回退 v1 热窗口。
func (s *APIServer) queryMemoryProfilesMixed(ctx context.Context, q ProfileQuery) ([]ContinuousMemoryProfileInfo, bool, error) {
	if !pqModeQueryV2(s.pqModeOf()) {
		return s.queryNativeMemoryProfiles(ctx, q)
	}
	authorized, err := s.pqAuthorizedSIDs(ctx, q)
	if err != nil {
		return nil, false, err
	}
	tenant := s.Config.ContinuousParquet.Tenant
	seen := map[string]bool{}
	var out []ContinuousMemoryProfileInfo
	anyData := false

	hours := pqHourlyRange(q.From, q.To)
	for _, hour := range hours {
		hFrom, hTo := pqHourBoundary(hour, q)
		if !hFrom.Before(hTo) {
			continue
		}
		block, err := s.pqFindBestBlock(ctx, tenant, hour, model.ContinuousParquetSignalCPU)
		if err != nil {
			incParquetQueryError()
			return nil, true, err
		}
		if block == nil {
			incParquetV1Fallback()
			sub := q
			sub.From, sub.To = hFrom, hTo
			profiles, found, err := s.queryNativeMemoryProfiles(ctx, sub)
			if err != nil {
				incParquetQueryError()
				return nil, true, err
			}
			if found {
				anyData = true
			}
			for _, profile := range profiles {
				key := memoryProfileDedupeKey(profile)
				if seen[key] {
					profile.Status = "duplicate"
				} else {
					seen[key] = true
				}
				out = append(out, profile)
			}
			continue
		}
		rows, _, readErr := pqReadBlockRows[pqCPURow](s, ctx, block.BlockID, hFrom.UnixMilli(), hTo.UnixMilli())
		if readErr == nil {
			byProfile := map[string]*ContinuousMemoryProfileInfo{}
			order := []string{}
			for i := range rows {
				row := &rows[i]
				if _, ok := authorized[row.SessionSID]; !ok {
					continue
				}
				if row.Timestamp < hFrom.UnixMilli() || row.Timestamp >= hTo.UnixMilli() {
					continue
				}
				if row.ProfileType != "python_memory" && row.ProfileType != "memory" {
					continue
				}
				if row.ProfileID == "" {
					continue // 旧 Parquet 无 profile_id，无法关联
				}
				sample := pqSampleFromCPURow(*row)
				if (row.ProfileStatus == "failed" && len(q.Filters) > 0) ||
					(row.ProfileStatus != "failed" && !continuousSampleMatches(sample, pqLabelsInterface(row.Labels), q.Filters)) {
					continue
				}
				key := row.ProfileID + "|" + strconv.Itoa(int(row.PID)) + "|" + strconv.FormatInt(row.ProcessStartMs, 10) + "|" + row.Exe
				item := byProfile[key]
				if item == nil {
					item = &ContinuousMemoryProfileInfo{
						ProfileID: row.ProfileID, SessionSID: row.SessionSID,
						WindowStart: time.UnixMilli(row.Timestamp), WindowEnd: time.UnixMilli(row.Timestamp).Add(time.Minute),
						PID: int(row.PID), ProcessStartMs: row.ProcessStartMs,
						Comm: row.Comm, Exe: row.Exe, Backend: row.Backend,
						Unit: firstNonEmpty(row.Unit, "bytes"), Status: "ready",
					}
					byProfile[key] = item
					order = append(order, key)
				}
				if row.ProfileStatus == "failed" {
					item.Status = "failed"
					item.Reason = row.ProfileReason
				}
				item.SampleCount = addContinuousCount(item.SampleCount, row.Value)
			}
			for _, key := range order {
				profile := byProfile[key]
				if seen[key] {
					profile.Status = "duplicate"
				} else {
					seen[key] = true
				}
				out = append(out, *profile)
			}
			anyData = true
		}
		if readErr != nil {
			incParquetQueryError()
			sub := q
			sub.From, sub.To = hFrom, hTo
			profiles, found, v1Err := s.queryNativeMemoryProfiles(ctx, sub)
			if v1Err == nil && found {
				incParquetV1Fallback()
				anyData = true
				for _, profile := range profiles {
					key := memoryProfileDedupeKey(profile)
					if seen[key] {
						profile.Status = "duplicate"
					} else {
						seen[key] = true
					}
					out = append(out, profile)
				}
				continue
			}
			return nil, true, errPartialCoverage
		}
	}
	return out, anyData, nil
}

// queryNativeMemoryProfiles 从 v1 热窗口（profile_windows + 对象存储）收集
// Memray profile 元数据。
func (s *APIServer) queryNativeMemoryProfiles(ctx context.Context, q ProfileQuery) ([]ContinuousMemoryProfileInfo, bool, error) {
	var windows []model.ProfileWindow
	sessionQuery := s.continuousSessionSelection(q)
	err := s.DB.WithContext(ctx).Where("session_sid IN (?)", sessionQuery).
		Where("signal_type = ?", "python_memory").
		Where("window_end >= ? AND window_start <= ?", q.From, q.To).
		Order("window_start ASC").
		Limit(continuousMaxWindowCount + 1).
		Find(&windows).Error
	if err != nil {
		return nil, false, err
	}
	if len(windows) > continuousMaxWindowCount {
		return nil, true, errContinuousWindowLimit
	}
	if len(windows) == 0 {
		return []ContinuousMemoryProfileInfo{}, false, nil
	}
	if !s.StorageConnected() {
		return nil, true, errProfileUnavailable
	}

	seen := map[string]bool{}
	var out []ContinuousMemoryProfileInfo
	objectOrder, byObject := continuousGroupWindowsByObject(windows)
	for _, objectKey := range objectOrder {
		batches, err := s.loadContinuousBatches(ctx, objectKey)
		if err != nil {
			return nil, true, err
		}
		batchByID := continuousBatchIndex(batches)
		seenBatch := map[string]bool{}
		for _, row := range byObject[objectKey] {
			batch, rowKey, ok := continuousResolveBatch(row, batches, batchByID)
			if !ok || seenBatch[rowKey] {
				continue
			}
			seenBatch[rowKey] = true
			for _, window := range batch.Windows {
				if !windowOverlaps(window.WindowStart, window.WindowEnd, q.From, q.To) {
					continue
				}
				for _, profile := range window.Profiles {
					if strings.ToLower(strings.TrimSpace(firstNonEmpty(profile.SignalType, "cpu_profile"))) != "python_memory" {
						continue
					}
					info := ContinuousMemoryProfileInfo{
						ProfileID: profile.ProfileID, SessionSID: row.SessionSID,
						WindowStart: window.WindowStart, WindowEnd: window.WindowEnd,
						Backend: firstNonEmpty(profile.Backend, window.Backend),
						Unit:    firstNonEmpty(profile.Unit, "bytes"),
						Status:  "ready",
					}
					matched := len(q.Filters) == 0
					for _, sample := range profile.Samples {
						if !continuousSampleMatches(sample, window.Labels, q.Filters) {
							continue
						}
						matched = true
						if info.PID == 0 {
							info.PID = sample.PID
							info.ProcessStartMs = sample.ProcessStartMs
							info.Comm = sample.Comm
							info.Exe = sample.Exe
						}
						info.SampleCount = addContinuousCount(info.SampleCount, firstNonZeroUint64(sample.Count, 1))
					}
					if !matched {
						continue
					}
					if info.ProfileID == "" {
						info.ProfileID = "anonymous-" + strconv.FormatInt(window.WindowStart.UnixMilli(), 10)
					}
					if info.SampleCount == 0 {
						// 诊断-only 窗口（转换失败/无峰值分配）：标记 failed。
						info.Status = "failed"
						info.Reason = "profile 无可用样本（转换失败或无峰值分配）"
					}
					key := memoryProfileDedupeKey(info)
					if seen[key] {
						info.Status = "duplicate"
					} else {
						seen[key] = true
					}
					out = append(out, info)
				}
			}
		}
	}
	return out, true, nil
}

// memoryProfileDedupeKey 与查询聚合同一去重键（profile_id + 进程身份）。
func memoryProfileDedupeKey(info ContinuousMemoryProfileInfo) string {
	if info.PID > 0 {
		return info.ProfileID + "|" + strconv.Itoa(info.PID) + "|" + strconv.FormatInt(info.ProcessStartMs, 10) + "|" + info.Exe
	}
	return info.ProfileID
}
