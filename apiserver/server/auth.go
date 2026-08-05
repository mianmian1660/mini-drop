// ============================================================
// server/auth.go — 鉴权相关处理器
// 包含：健康检查、登录回调、用户信息获取
// ============================================================

package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/mini-drop/apiserver/model"
)

// Healthz 健康检查端点
// GET /healthz
// 返回服务状态（Kubernetes/Docker 用这个判断服务是否存活）
func (s *APIServer) Healthz(c *gin.Context) {
	checks := s.readinessChecks()
	status := "ok"
	if checks["db"] != "ok" {
		status = "unhealthy"
	}
	c.JSON(http.StatusOK, gin.H{
		"request_id": requestIDFromGin(c),
		"status":     status,
		"service":    "apiserver",
		"ready":      status == "ok",
		"checks":     checks,
	})
}

func (s *APIServer) Livez(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"request_id": requestIDFromGin(c),
		"status":     "ok",
		"service":    "apiserver",
	})
}

func (s *APIServer) Readyz(c *gin.Context) {
	checks := s.readinessChecks()
	status := http.StatusOK
	ready := true
	for _, key := range []string{"db", "storage", "grpc", "migration", "outbox"} {
		if checks[key] != "ok" {
			ready = false
			status = http.StatusServiceUnavailable
		}
	}
	c.JSON(status, gin.H{
		"request_id": requestIDFromGin(c),
		"status":     map[bool]string{true: "ready", false: "not_ready"}[ready],
		"ready":      ready,
		"checks":     checks,
	})
}

func (s *APIServer) readinessChecks() map[string]string {
	checks := map[string]string{"db": "ok", "storage": "ok", "grpc": "ok", "migration": "ok", "outbox": "ok"}
	sqlDB, err := s.DB.DB()
	if err != nil {
		s.Logger.Error("健康检查：获取数据库连接失败", zap.Error(err))
		checks["db"] = "unavailable"
		return checks
	}

	if err := sqlDB.Ping(); err != nil {
		s.Logger.Error("健康检查：数据库 Ping 失败", zap.Error(err))
		checks["db"] = "unavailable"
	}
	if !s.StorageConnected() {
		checks["storage"] = "unavailable"
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if _, err := s.Storage.ListObjects(ctx, s.Config.Storage.Bucket, "__readyz__/"); err != nil {
			checks["storage"] = "unavailable"
		}
		cancel()
	}
	if !s.GrpcConnected() {
		checks["grpc"] = "unavailable"
	}
	if latest, err := model.LatestMigrationVersion(); err != nil {
		checks["migration"] = "unknown"
	} else if latest != "" {
		var count int64
		if err := s.DB.Model(&model.SchemaMigration{}).Where("version = ?", latest).Count(&count).Error; err != nil || count == 0 {
			checks["migration"] = "outdated"
		}
	}
	var deadLetters int64
	if err := s.DB.Model(&model.Outbox{}).Where("status = ?", model.OutboxStatusDeadLetter).Count(&deadLetters).Error; err != nil {
		checks["outbox"] = "unknown"
	} else if deadLetters > 0 {
		checks["outbox"] = "has_dead_letters"
	}
	return checks
}

// AuthCheck 鉴权回调
// GET /api/v1/auth/check
// 检查请求是否携带身份信息（Drop-User-Uid 头）。这个接口本身必须留在
// CheckLogin 中间件之外——它的职责就是让前端知道"我现在算不算登录"，
// 如果也被拦在中间件里，前端就永远拿不到"未登录"这个明确信号了。
//
// A4：不再无条件放通、默认造一个 "default-user"。没带身份信息就如实
// 返回 401，前端 axios 拦截器已经在处理 401 自动跳转登录页。
func (s *APIServer) AuthCheck(c *gin.Context) {
	uid := getRequestUID(c)
	if uid == "" {
		s.RespondHTTPError(c, http.StatusUnauthorized, ErrCodeAuthForbidden, "未登录：请求未携带 Drop-User-Uid")
		return
	}

	userName := getRequestUserName(c)

	s.RespondOK(c, gin.H{
		"uid":       uid,
		"user_name": userName,
		"role":      s.AuthContext(c).Role,
		"groups":    s.AuthContext(c).Groups,
		"location":  "", // 生产环境用于 OAuth 跳转
	})
}

// CheckLogin 简化版鉴权中间件（A4，新复刻指南 6.9 节）
//
// 当前系统没有真正的登录态/session/OIDC，身份完全靠客户端自己带的
// Drop-User-Uid 头声明。这里能做到的"鉴权"边界很明确：校验请求确实
// 携带了身份信息，不再对空身份默认造一个 "default-user" 蒙混过关；
// 校验"这个身份是否被伪造"超出本次改动范围，需要接入真实登录态才能做到。
//
// 返回 401（不是 A2 错误码表里 403 的 AUTH_FORBIDDEN——那是"登录了但无权限"，
// 这里是"根本没带身份"），和 AuthCheck 保持同样的响应形状，前端 axios
// 拦截器专门监听 401 做登录跳转，用 403 的话跳转逻辑就不会触发。
//
// 必须挂在 /auth/check 之外的所有 /api/v1 路由上。
func (s *APIServer) CheckLogin(c *gin.Context) {
	uid := getRequestUID(c)
	if uid == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"request_id": requestIDFromGin(c),
			"data":       nil,
			"error":      APIError{Code: ErrCodeAuthForbidden, Message: "未登录：请求未携带 Drop-User-Uid", Retryable: false, Stage: "auth"},
			"code":       http.StatusUnauthorized,
			"message":    "未登录：请求未携带 Drop-User-Uid",
		})
		return
	}
	c.Next()
}

// GetCurrentUser 获取当前用户信息
// GET /api/v1/users
func (s *APIServer) GetCurrentUser(c *gin.Context) {
	uid := getRequestUIDOrDefault(c)
	userName := getRequestUserName(c)

	auth := s.AuthContext(c)
	s.RespondOK(c, gin.H{
		"uid":       uid,
		"user_name": userName,
		"name":      userName,
		"role":      auth.Role,
		"groups":    auth.Groups,
	})
}

func getRequestUID(c *gin.Context) string {
	uid := strings.TrimSpace(c.GetHeader("Drop-User-Uid"))
	if uid != "" {
		return uid
	}
	uid = strings.TrimSpace(c.GetHeader("Drop_user_uid"))
	if uid != "" {
		return uid
	}
	if cookieUID, err := c.Cookie("drop_user_uid"); err == nil {
		return strings.TrimSpace(cookieUID)
	}
	return ""
}

func getRequestUIDOrDefault(c *gin.Context) string {
	if uid := getRequestUID(c); uid != "" {
		return uid
	}
	return "default-user"
}

func getRequestUserName(c *gin.Context) string {
	userName := strings.TrimSpace(c.GetHeader("Drop-User-Name"))
	if userName == "" {
		userName = strings.TrimSpace(c.GetHeader("Drop_user_name"))
	}
	if userName == "" {
		if cookieName, err := c.Cookie("drop_user_name"); err == nil {
			userName = strings.TrimSpace(cookieName)
		}
	}
	if userName == "" {
		return "默认用户"
	}
	return userName
}
