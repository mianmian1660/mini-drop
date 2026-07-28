// ============================================================
// server/error_codes.go — 结构化错误码分层（Track A2）
// ============================================================
// 落地新复刻指南第 4.10 节的错误码分层：每个错误码声明所属阶段、
// 是否可重试、对应的 HTTP 状态码，以及面向用户的文案。
// ============================================================

package server

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// 错误码常量（与新复刻指南 4.10 节原文一致）
const (
	ErrCodeAuthForbidden           = "AUTH_FORBIDDEN"
	ErrCodeTargetNotFound          = "TARGET_NOT_FOUND"
	ErrCodeAgentOffline            = "AGENT_OFFLINE"
	ErrCodeAgentIncompatible       = "AGENT_INCOMPATIBLE"
	ErrCodeTaskInvalidArgument     = "TASK_INVALID_ARGUMENT"
	ErrCodeTaskQueueFull           = "TASK_QUEUE_FULL"
	ErrCodeRunnerNotAvailable      = "RUNNER_NOT_AVAILABLE"
	ErrCodeRunnerPermissionDenied  = "RUNNER_PERMISSION_DENIED"
	ErrCodeRunnerTimeout           = "RUNNER_TIMEOUT"
	ErrCodeArtifactUploadFailed    = "ARTIFACT_UPLOAD_FAILED"
	ErrCodeAnalysisCorruptInput    = "ANALYSIS_CORRUPT_INPUT"
	ErrCodeAnalysisUnsupportedForm = "ANALYSIS_UNSUPPORTED_FORMAT"
	ErrCodeAnalysisTimeout         = "ANALYSIS_TIMEOUT"
	ErrCodeDependencyUnavailable   = "DEPENDENCY_UNAVAILABLE"
)

// ErrorCode 描述一个结构化错误码及其元数据。
type ErrorCode struct {
	Code       string // 错误码本身
	Stage      string // 所属阶段：auth / dispatch / collection / upload / analysis / infra
	Retryable  bool   // 是否可重试
	HTTPStatus int    // 对应的 HTTP 状态码
	Message    string // 面向用户的文案
}

var errorCodeRegistry = map[string]ErrorCode{
	ErrCodeAuthForbidden:           {ErrCodeAuthForbidden, "auth", false, http.StatusForbidden, "没有权限执行此操作"},
	ErrCodeTargetNotFound:          {ErrCodeTargetNotFound, "dispatch", false, http.StatusNotFound, "目标资源不存在"},
	ErrCodeAgentOffline:            {ErrCodeAgentOffline, "dispatch", true, http.StatusServiceUnavailable, "目标 Agent 当前离线"},
	ErrCodeAgentIncompatible:       {ErrCodeAgentIncompatible, "dispatch", false, http.StatusBadRequest, "Agent 版本不兼容"},
	ErrCodeTaskInvalidArgument:     {ErrCodeTaskInvalidArgument, "dispatch", false, http.StatusBadRequest, "任务参数不合法"},
	ErrCodeTaskQueueFull:           {ErrCodeTaskQueueFull, "dispatch", true, http.StatusServiceUnavailable, "任务队列已满，请稍后重试"},
	ErrCodeRunnerNotAvailable:      {ErrCodeRunnerNotAvailable, "collection", true, http.StatusServiceUnavailable, "采集工具不可用"},
	ErrCodeRunnerPermissionDenied:  {ErrCodeRunnerPermissionDenied, "collection", false, http.StatusForbidden, "采集权限不足"},
	ErrCodeRunnerTimeout:           {ErrCodeRunnerTimeout, "collection", true, http.StatusGatewayTimeout, "采集超时"},
	ErrCodeArtifactUploadFailed:    {ErrCodeArtifactUploadFailed, "upload", true, http.StatusBadGateway, "产物上传失败"},
	ErrCodeAnalysisCorruptInput:    {ErrCodeAnalysisCorruptInput, "analysis", false, http.StatusUnprocessableEntity, "分析输入数据损坏"},
	ErrCodeAnalysisUnsupportedForm: {ErrCodeAnalysisUnsupportedForm, "analysis", false, http.StatusUnprocessableEntity, "不支持的数据格式"},
	ErrCodeAnalysisTimeout:         {ErrCodeAnalysisTimeout, "analysis", true, http.StatusGatewayTimeout, "分析超时"},
	ErrCodeDependencyUnavailable:   {ErrCodeDependencyUnavailable, "infra", true, http.StatusServiceUnavailable, "依赖服务不可用"},
}

// LookupErrorCode 返回错误码的元数据；找不到时 ok=false。
func LookupErrorCode(code string) (ErrorCode, bool) {
	ec, ok := errorCodeRegistry[code]
	return ec, ok
}

// formatErrorReason 把错误码和具体细节拼成落库用的 status_info/reason 文本，
// 形如 "[DEPENDENCY_UNAVAILABLE] 依赖服务不可用：gRPC 未连接，任务无法下发到 drop_server"。
// 未知错误码时退化为只带 detail 的原始文本。
func formatErrorReason(code string, detail string) string {
	ec, ok := LookupErrorCode(code)
	if !ok {
		return detail
	}
	if detail == "" {
		return fmt.Sprintf("[%s] %s", ec.Code, ec.Message)
	}
	return fmt.Sprintf("[%s] %s：%s", ec.Code, ec.Message, detail)
}

// RespondErrorCode 用注册表中的错误码统一构造 JSON 错误响应，
// 响应体带上 error_code 和 retryable，方便前端据此决定是否可以重试。
func (s *APIServer) RespondErrorCode(c *gin.Context, code string, detail string) {
	ec, ok := LookupErrorCode(code)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "未知错误码: " + code})
		return
	}
	msg := ec.Message
	if detail != "" {
		msg = msg + "：" + detail
	}
	c.JSON(ec.HTTPStatus, gin.H{
		"code":       ec.HTTPStatus,
		"error_code": ec.Code,
		"retryable":  ec.Retryable,
		"message":    msg,
	})
}

// isUniqueViolation 判断一个 DB 错误是否是唯一索引冲突（Postgres 错误文本匹配）。
// 用于幂等键并发写入场景：两个请求都通过了查重，只有一个能真正插入成功。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "duplicate key value violates unique constraint")
}
