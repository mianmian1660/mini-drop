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
	"net/http"

	"github.com/gin-gonic/gin"
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
