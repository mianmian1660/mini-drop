// ============================================================
// server/continuous_symbol.go — internal continuous symbol check API
// ============================================================
// 借鉴 Pyroscope 符号化诊断设计：agent 上传 continuous batch 时可附带
// build-id / kallsyms 引用，apiserver 复用现有 symbols/{build_id} 和
// kernel-symbols storage 做存在性检查，返回缺失符号列表，供 agent 和
// 前端诊断区展示"符号不完整"的原因。不实现 debuginfod，不把 continuous
// 符号引用强塞进 task artifact。
//
// POST /api/v1/internal/continuous/symbol-check
// 请求体:
//   { "build_ids": ["abc123","def456"], "kallsyms_sha256": "..." }
// 响应:
//   { "build_ids": {"abc123": true, "def456": false},
//     "kallsyms": false,
//     "missing": ["def456"],
//     "symbol_status": "partial" }
// ============================================================

package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/mini-drop/apiserver/model"
)

type ContinuousSymbolCheckReq struct {
	BuildIDs       []string `json:"build_ids"`
	KallsymsSHA256 string   `json:"kallsyms_sha256"`
}

type ContinuousSymbolCheckResp struct {
	BuildIDs     map[string]bool `json:"build_ids"`
	Kallsyms     bool            `json:"kallsyms"`
	Missing      []string        `json:"missing"`
	SymbolStatus string          `json:"symbol_status"`
	Reasons      []string        `json:"reasons,omitempty"`
}

func (s *APIServer) ContinuousSymbolCheck(c *gin.Context) {
	var req ContinuousSymbolCheckReq
	if err := c.ShouldBindJSON(&req); err != nil {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "参数错误: "+err.Error())
		return
	}

	resp := ContinuousSymbolCheckResp{
		BuildIDs:     map[string]bool{},
		Missing:      []string{},
		SymbolStatus: "complete",
	}

	anyPresent := false
	anyMissing := false

	for _, bid := range req.BuildIDs {
		if bid == "" {
			continue
		}
		exists := false
		if s.StorageConnected() {
			key := "symbols/" + bid
			if ok, err := s.Storage.ObjectExists(c.Request.Context(), s.Config.Storage.Bucket, key); err == nil {
				exists = ok
			}
		}
		resp.BuildIDs[bid] = exists
		if exists {
			anyPresent = true
		} else {
			anyMissing = true
			resp.Missing = append(resp.Missing, bid)
		}
	}

	if req.KallsymsSHA256 != "" {
		exists := false
		if s.StorageConnected() {
			key := kernelSymbolObjectKey(req.KallsymsSHA256)
			if ok, err := s.Storage.ObjectExists(c.Request.Context(), s.Config.Storage.Bucket, key); err == nil {
				exists = ok
			}
		}
		resp.Kallsyms = exists
		if exists {
			anyPresent = true
		} else {
			anyMissing = true
		}
	}

	switch {
	case !anyPresent && (len(req.BuildIDs) > 0 || req.KallsymsSHA256 != ""):
		resp.SymbolStatus = "missing"
	case anyMissing:
		resp.SymbolStatus = "partial"
	}

	s.RespondOK(c, gin.H{"symbol_check": resp})
}

func (s *APIServer) CheckContinuousSessionSymbols(c *gin.Context) {
	auth := s.AuthContext(c)
	session, ok := s.loadReadableContinuousSession(c, strings.TrimSpace(c.Param("sid")), auth)
	if !ok {
		return
	}
	if !s.StorageConnected() {
		s.RespondHTTPError(c, http.StatusServiceUnavailable, ErrCodeDependencyUnavailable, "对象存储未连接，暂时无法检查符号")
		return
	}
	var req struct {
		From time.Time `json:"from"`
		To   time.Time `json:"to"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "参数错误: "+err.Error())
		return
	}
	now := time.Now()
	from := req.From
	to := req.To
	if to.IsZero() {
		to = now
	}
	if from.IsZero() {
		from = to.Add(-time.Duration(firstNonZeroUint32(session.RetentionHours, 24)) * time.Hour)
	}
	if !from.Before(to) {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "符号检查时间范围不合法")
		return
	}
	var windows []model.ProfileWindow
	if err := s.DB.Where("session_sid = ? AND window_end >= ? AND window_start <= ?", session.SID, from, to).
		Order("window_start ASC").Limit(continuousMaxWindowCount + 1).Find(&windows).Error; err != nil {
		s.RespondHTTPError(c, http.StatusInternalServerError, ErrCodeDependencyUnavailable, "查询 Continuous 符号引用失败")
		return
	}
	if len(windows) > continuousMaxWindowCount {
		s.RespondHTTPError(c, http.StatusBadRequest, ErrCodeTaskInvalidArgument, "符号检查时间范围过大，请缩小范围")
		return
	}
	buildIDs := map[string]bool{}
	kallsyms := ""
	reasons := map[string]bool{}
	seenObjects := map[string]bool{}
	objectOrder, _ := continuousGroupWindowsByObject(windows)
	for _, objectKey := range objectOrder {
		if objectKey == "" || seenObjects[objectKey] {
			continue
		}
		seenObjects[objectKey] = true
		// 阶段三：块只解压一次，收集块内全部成员 batch 的符号引用
		batches, err := s.loadContinuousBatches(c.Request.Context(), objectKey)
		if err != nil {
			continue
		}
		for i := range batches {
			batch := &batches[i]
			collectContinuousSymbolRefs(batch.SymbolRefs, buildIDs, &kallsyms, reasons)
			for _, window := range batch.Windows {
				if windowOverlaps(window.WindowStart, window.WindowEnd, from, to) {
					collectContinuousSymbolRefs(window.SymbolRefs, buildIDs, &kallsyms, reasons)
				}
			}
		}
	}
	buildIDList := make([]string, 0, len(buildIDs))
	for buildID := range buildIDs {
		buildIDList = append(buildIDList, buildID)
	}
	sort.Strings(buildIDList)
	resp := s.checkContinuousSymbols(c.Request.Context(), buildIDList, kallsyms)
	if len(reasons) > 0 {
		resp.Reasons = make([]string, 0, len(reasons))
		for reason := range reasons {
			resp.Reasons = append(resp.Reasons, reason)
		}
		sort.Strings(resp.Reasons)
	}
	s.RespondOK(c, gin.H{
		"symbol_check": resp,
		"session_sid":  session.SID,
		"from":         from,
		"to":           to,
		"object_count": len(seenObjects),
	})
}

func (s *APIServer) checkContinuousSymbols(ctx context.Context, buildIDs []string, kallsymsSHA256 string) ContinuousSymbolCheckResp {
	resp := ContinuousSymbolCheckResp{
		BuildIDs:     map[string]bool{},
		Missing:      []string{},
		SymbolStatus: "complete",
	}
	anyPresent := false
	anyMissing := false
	for _, bid := range buildIDs {
		if bid == "" {
			continue
		}
		exists := false
		if s.StorageConnected() {
			key := "symbols/" + bid
			if ok, err := s.Storage.ObjectExists(ctx, s.Config.Storage.Bucket, key); err == nil {
				exists = ok
			}
		}
		resp.BuildIDs[bid] = exists
		if exists {
			anyPresent = true
		} else {
			anyMissing = true
			resp.Missing = append(resp.Missing, bid)
		}
	}
	if kallsymsSHA256 != "" {
		exists := false
		if s.StorageConnected() {
			key := kernelSymbolObjectKey(kallsymsSHA256)
			if ok, err := s.Storage.ObjectExists(ctx, s.Config.Storage.Bucket, key); err == nil {
				exists = ok
			}
		}
		resp.Kallsyms = exists
		if exists {
			anyPresent = true
		} else {
			anyMissing = true
		}
	}
	switch {
	case !anyPresent && (len(buildIDs) > 0 || kallsymsSHA256 != ""):
		resp.SymbolStatus = "missing"
	case anyMissing:
		resp.SymbolStatus = "partial"
	}
	return resp
}

func collectContinuousSymbolRefs(value interface{}, buildIDs map[string]bool, kallsyms *string, reasons map[string]bool) {
	var walk func(interface{}, string)
	walk = func(raw interface{}, key string) {
		switch item := raw.(type) {
		case map[string]interface{}:
			for childKey, child := range item {
				walk(child, strings.ToLower(strings.TrimSpace(childKey)))
			}
		case []interface{}:
			for _, child := range item {
				walk(child, key)
			}
		case string:
			text := strings.TrimSpace(item)
			if text == "" {
				return
			}
			switch {
			case strings.Contains(key, "build_id") || key == "buildids":
				buildIDs[text] = true
			case strings.Contains(key, "kallsyms") && *kallsyms == "":
				*kallsyms = text
			case key == "reason" && reasons != nil:
				// runtime_maps / python_fallback / python_memory / native_go
				// 里已经采集好的失败原因（perf-map 未生成、权限不足、进程退出
				// 等），key 名不带 build_id/kallsyms，此前递归扫不到。这里单独
				// 捡出来，让诊断接口能直接给出根因文案。
				reasons[text] = true
			}
		}
	}
	walk(value, "")
}
