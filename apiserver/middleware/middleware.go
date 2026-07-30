// ============================================================
// middleware — HTTP 中间件
// ============================================================

package middleware

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// RequestID 请求 ID 中间件（A4，6.4 节推荐的中间件顺序里贯穿全链路的 request_id）
// 优先复用客户端传入的 X-Request-ID（方便前端/上游把一次操作串联起来排障），
// 没有则生成一个；写回响应头，并存进 gin.Context 供后续 handler/日志取用。
// 必须排在其他中间件最前面，这样 AccessLog 等才能拿到同一个 request_id。
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		rid := c.GetHeader("X-Request-ID")
		if rid == "" {
			rid = uuid.New().String()
		}
		c.Set("request_id", rid)
		c.Header("X-Request-ID", rid)
		c.Next()
	}
}

// requestIDFromContext 取出 RequestID 中间件写入的 request_id；
// 理论上不会缺失（RequestID 应排在最前面），缺失时返回空字符串而不是 panic。
func requestIDFromContext(c *gin.Context) string {
	if v, ok := c.Get("request_id"); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// AccessLog 访问日志中间件
// 记录每个 HTTP 请求的：request_id、方法、路径、状态码、耗时、客户端 IP
func AccessLog(logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		// 处理请求
		c.Next()

		// 记录日志
		latency := time.Since(start)
		logger.Info("HTTP 请求",
			zap.String("request_id", requestIDFromContext(c)),
			zap.String("method", c.Request.Method),
			zap.String("path", c.Request.URL.Path),
			zap.Int("status", c.Writer.Status()),
			zap.Duration("latency", latency),
			zap.String("client_ip", c.ClientIP()),
			zap.String("user_agent", c.Request.UserAgent()),
		)
	}
}

// CORS 跨域中间件
// 允许前端跨域请求（开发环境用，生产环境建议 nginx 处理）
func CORS() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		if origin == "" {
			origin = "*"
		}

		c.Header("Access-Control-Allow-Origin", origin)
		c.Header("Access-Control-Allow-Credentials", "true")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization, Drop-User-Uid, Drop-User-Name, Drop_user_uid, Drop_user_name")
		c.Header("Access-Control-Max-Age", "86400")

		// 预检请求直接返回
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	}
}

// Recovery 自定义 panic 恢复中间件
func Recovery(logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				logger.Error("服务 panic 恢复",
					zap.String("request_id", requestIDFromContext(c)),
					zap.Any("error", err),
					zap.String("path", c.Request.URL.Path),
				)
				c.AbortWithStatusJSON(500, gin.H{
					"code":    500,
					"message": "服务器内部错误",
				})
			}
		}()
		c.Next()
	}
}
