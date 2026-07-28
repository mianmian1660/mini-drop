// ============================================================
// server/auth.go — 鉴权相关处理器
// 包含：健康检查、登录回调、用户信息获取
// ============================================================

package server

import (
	"net/http"
	"strings"

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
		"status":  "ok",
		"service": "apiserver",
	})
}

// AuthCheck 鉴权回调
// GET /api/v1/auth/check
// 检查请求是否携带身份信息（Drop_user_uid 头）。这个接口本身必须留在
// CheckLogin 中间件之外——它的职责就是让前端知道"我现在算不算登录"，
// 如果也被拦在中间件里，前端就永远拿不到"未登录"这个明确信号了。
//
// A4：不再无条件放通、默认造一个 "default-user"。没带身份信息就如实
// 返回 401，前端 axios 拦截器已经在处理 401 自动跳转登录页。
func (s *APIServer) AuthCheck(c *gin.Context) {
	uid := strings.TrimSpace(c.GetHeader("Drop_user_uid"))
	if uid == "" {
		c.JSON(http.StatusUnauthorized, gin.H{
			"code":    401,
			"message": "未登录：请求未携带 Drop_user_uid",
		})
		return
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

// CheckLogin 简化版鉴权中间件（A4，新复刻指南 6.9 节）
//
// 当前系统没有真正的登录态/session/OIDC，身份完全靠客户端自己带的
// Drop_user_uid 头声明。这里能做到的"鉴权"边界很明确：校验请求确实
// 携带了身份信息，不再对空身份默认造一个 "default-user" 蒙混过关；
// 校验"这个身份是否被伪造"超出本次改动范围，需要接入真实登录态才能做到。
//
// 返回 401（不是 A2 错误码表里 403 的 AUTH_FORBIDDEN——那是"登录了但无权限"，
// 这里是"根本没带身份"），和 AuthCheck 保持同样的响应形状，前端 axios
// 拦截器专门监听 401 做登录跳转，用 403 的话跳转逻辑就不会触发。
//
// 必须挂在 /auth/check 之外的所有 /api/v1 路由上。
func (s *APIServer) CheckLogin(c *gin.Context) {
	uid := strings.TrimSpace(c.GetHeader("Drop_user_uid"))
	if uid == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"code":    401,
			"message": "未登录：请求未携带 Drop_user_uid",
		})
		return
	}
	c.Next()
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
