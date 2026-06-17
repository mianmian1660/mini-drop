// ============================================================
// server/agent.go — Agent 管理处理器
// 包含：Agent 列表、Agent 资源统计
// ============================================================

package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/mini-drop/apiserver/model"
)

// ListAgents 获取 Agent 列表
// GET /api/v1/agents
func (s *APIServer) ListAgents(c *gin.Context) {
	var agents []model.AgentInfo

	if err := s.DB.Order("last_seen DESC").Find(&agents).Error; err != nil {
		s.Logger.Error("查询 Agent 列表失败", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    500,
			"message": "查询 Agent 列表失败",
		})
		return
	}

	// 确保返回空数组而不是 null
	if agents == nil {
		agents = []model.AgentInfo{}
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"agents": agents,
			"total":  len(agents),
		},
	})
}

// StatAgent 查询单个 Agent 的资源占用
// GET /api/v1/agent/stat?ip=xxx
func (s *APIServer) StatAgent(c *gin.Context) {
	ip := c.Query("ip")
	if ip == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"message": "缺少 ip 参数",
		})
		return
	}

	// MVP 阶段：返回数据库中的 Agent 信息
	var agent model.AgentInfo
	if err := s.DB.Where("ip_addr = ?", ip).First(&agent).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"code":    404,
			"message": "Agent 不存在: " + ip,
		})
		return
	}

	// 后续 W3 通过 gRPC StatAgent 获取实时资源
	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"hostname":    agent.Hostname,
			"ip_addr":     agent.IPAddr,
			"online":      agent.Online,
			"version":     agent.Version,
			"environment": agent.Environment,
			"last_seen":   agent.LastSeen,
			// 以下为占位数据，W3 对接 gRPC 后替换为真实值
			"cpu_percent":  0.0,
			"memory_mb":    0.0,
			"disk_io_kbps": 0.0,
		},
	})
}
