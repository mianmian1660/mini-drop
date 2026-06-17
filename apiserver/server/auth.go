// ============================================================
// server/auth.go — 鉴权相关处理器
// 包含：健康检查、登录回调、用户信息获取
// ============================================================

package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Healthz 健康检查端点
// GET /healthz
// 返回服务状态（Kubernetes/Docker 用这个判断服务是否存活）
func (s *APIServer) Healthz(c *gin.Context) {
	// 顺便检查数据库连通性
	sqlDB, err := s.DB.DB()
	if err != nil {
		s.Logger.Error("健康检查：获取数据库连接失败", zap.Error(err))
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"status": "unhealthy",
			"reason": "数据库连接不可用",
		})
		return
	}

	if err := sqlDB.Ping(); err != nil {
		s.Logger.Error("健康检查：数据库 Ping 失败", zap.Error(err))
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"status": "unhealthy",
			"reason": "数据库 Ping 失败",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"service": "apiserver",
	})
}

// AuthCheck 鉴权回调
// GET /api/v1/auth/check
// 检查用户 Cookie 是否有效，失败返回 302 跳转登录页
// 当前 MVP 阶段暂时放通，后续接入真实鉴权
func (s *APIServer) AuthCheck(c *gin.Context) {
	// MVP 阶段：不做真实鉴权，直接返回通过
	uid := c.GetHeader("Drop_user_uid")
	if uid == "" {
		uid = "default-user"
	}

	userName := c.GetHeader("Drop_user_name")
	if userName == "" {
		userName = "默认用户"
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"uid":       uid,
			"user_name": userName,
			"location":  "", // 生产环境用于 OAuth 跳转
		},
	})
}

// GetCurrentUser 获取当前用户信息
// GET /api/v1/users
func (s *APIServer) GetCurrentUser(c *gin.Context) {
	uid := c.GetHeader("Drop_user_uid")
	if uid == "" {
		uid = "default-user"
	}
	userName := c.GetHeader("Drop_user_name")
	if userName == "" {
		userName = "默认用户"
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"uid":       uid,
			"user_name": userName,
			"name":      userName,
			"groups":    []string{},
		},
	})
}
