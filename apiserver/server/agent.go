// ============================================================
// server/agent.go — Agent 管理处理器
// 包含：Agent 列表、Agent 资源统计
// W3: 通过 gRPC StatAgent 自动发现 Agent 并同步到 DB
// ============================================================

package server

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/mini-drop/apiserver/model"
	pb "github.com/mini-drop/apiserver/proto/control"
)

// ListAgents 获取 Agent 列表
// GET /api/v1/agents
// 优先查 DB，DB 为空时尝试通过 gRPC 从 drop_server 发现 Agent
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

	// W3: 尝试通过 gRPC 刷新每个 Agent 的在线状态
	if s.ControlCli != nil {
		for i := range agents {
			s.ensureAgentAudited(&agents[i])
			s.markAgentOfflineIfStale(&agents[i])
			s.refreshAgentStatus(&agents[i])
		}
	}

	// W3: DB 为空时尝试自动发现 Agent（探测常见 IP）
	if len(agents) == 0 && s.ControlCli != nil {
		discovered := s.discoverAgents()
		if len(discovered) > 0 {
			agents = discovered
		}
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

// refreshAgentStatus 通过 gRPC StatAgent 更新单个 Agent 的在线状态
func (s *APIServer) refreshAgentStatus(agent *model.AgentInfo) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req := &pb.StatAgentRequest{TargetIP: agent.IPAddr}
	resp, err := s.ControlCli.StatAgent(ctx, req)
	if err != nil {
		// gRPC 不可达 → 标记离线
		if agent.Online {
			reason := "gRPC StatAgent 失败，30s 心跳检查判定离线"
			agent.Online = false
			s.DB.Model(agent).Update("online", false)
			s.recordAgentAudit(agent.IPAddr, agent.Hostname, "offline", reason)
		}
		return
	}

	if resp.GetCode() == 0 {
		// Agent 在线 → 更新状态
		if !agent.Online {
			agent.Online = true
			s.DB.Model(agent).Update("online", true)
			s.recordAgentAudit(agent.IPAddr, agent.Hostname, "recovered", "gRPC StatAgent 成功，Agent 恢复在线")
		}
		agent.LastSeen = time.Now()
		s.DB.Model(agent).Update("last_seen", time.Now())
	}
}

func (s *APIServer) markAgentOfflineIfStale(agent *model.AgentInfo) {
	if agent == nil || !agent.Online || agent.LastSeen.IsZero() {
		return
	}
	if time.Since(agent.LastSeen) <= 30*time.Second {
		return
	}
	reason := "超过 30s 未收到 Agent 心跳"
	agent.Online = false
	s.DB.Model(agent).Update("online", false)
	s.recordAgentAudit(agent.IPAddr, agent.Hostname, "offline", reason)
}

func (s *APIServer) ensureAgentAudited(agent *model.AgentInfo) {
	if agent == nil || agent.IPAddr == "" {
		return
	}
	var count int64
	if err := s.DB.Model(&model.AgentAuditLog{}).Where("ip_addr = ?", agent.IPAddr).Count(&count).Error; err != nil {
		return
	}
	if count > 0 {
		return
	}
	event := "registered"
	reason := "已有 Agent 首次纳入审计"
	if agent.Online {
		reason = "已有在线 Agent 首次纳入审计"
	}
	s.recordAgentAudit(agent.IPAddr, agent.Hostname, event, reason)
}

// discoverAgents 自动发现 Agent（探测已知 IP 列表）
func (s *APIServer) discoverAgents() []model.AgentInfo {
	// 先尝试常见 IP
	candidateIPs := []string{"127.0.0.1"}

	// 也从已有任务的 target_ip 中获取
	var taskIPs []string
	s.DB.Model(&model.HotmethodTask{}).
		Distinct("target_ip").
		Where("target_ip != ''").
		Pluck("target_ip", &taskIPs)

	// 合并去重
	seen := map[string]bool{}
	for _, ip := range candidateIPs {
		seen[ip] = true
	}
	for _, ip := range taskIPs {
		if !seen[ip] {
			candidateIPs = append(candidateIPs, ip)
			seen[ip] = true
		}
	}

	var discovered []model.AgentInfo

	for _, ip := range candidateIPs {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req := &pb.StatAgentRequest{TargetIP: ip}
		resp, err := s.ControlCli.StatAgent(ctx, req)
		cancel()

		if err != nil {
			continue // 该 IP 无响应，跳过
		}

		if resp.GetCode() == 0 {
			// Agent 存在！写入 DB 并添加到结果
			now := time.Now()
			agent := model.AgentInfo{
				Hostname:    ip, // 暂时用 IP 作为主机名
				IPAddr:      ip,
				Online:      true,
				Version:     "1.0.0",
				Environment: "production",
				LastSeen:    now,
			}

			// Upsert：如果已存在则更新，否则创建
			var existing model.AgentInfo
			result := s.DB.Where("ip_addr = ?", ip).First(&existing)
			if result.Error == nil {
				// 已存在 → 更新在线状态
				if !existing.Online {
					s.recordAgentAudit(existing.IPAddr, existing.Hostname, "recovered", "自动发现探测成功，Agent 恢复在线")
				}
				s.DB.Model(&existing).Updates(map[string]interface{}{
					"online":    true,
					"last_seen": now,
				})
				existing.Online = true
				existing.LastSeen = now
				discovered = append(discovered, existing)
			} else {
				// 不存在 → 创建
				s.DB.Create(&agent)
				s.recordAgentAudit(agent.IPAddr, agent.Hostname, "registered", "自动发现新 Agent")
				discovered = append(discovered, agent)
			}

			s.Logger.Info("发现 Agent",
				zap.String("ip", ip),
				zap.Float64("cpu", resp.GetCpuPercent()),
				zap.Uint64("mem_kb", resp.GetMemoryKb()),
			)
		}
	}

	return discovered
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

	// W3: 优先通过 gRPC 获取实时资源
	if s.ControlCli != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		req := &pb.StatAgentRequest{TargetIP: ip}
		resp, err := s.ControlCli.StatAgent(ctx, req)

		if err == nil && resp.GetCode() == 0 {
			// 同时更新 DB
			var agent model.AgentInfo
			if s.DB.Where("ip_addr = ?", ip).First(&agent).Error == nil {
				if !agent.Online {
					s.recordAgentAudit(agent.IPAddr, agent.Hostname, "recovered", "实时资源查询成功，Agent 恢复在线")
				}
				agent.Online = true
				agent.LastSeen = time.Now()
				s.DB.Save(&agent)
			}

			c.JSON(http.StatusOK, gin.H{
				"code": 0,
				"data": gin.H{
					"hostname":       agent.Hostname,
					"ip_addr":        ip,
					"online":         true,
					"version":        agent.Version,
					"environment":    agent.Environment,
					"last_seen":      agent.LastSeen,
					"cpu_percent":    resp.GetCpuPercent(),
					"memory_kb":      resp.GetMemoryKb(),
					"read_kb_per_s":  resp.GetReadKbPerS(),
					"write_kb_per_s": resp.GetWriteKbPerS(),
				},
			})
			return
		}
	}

	// gRPC 不可达 → 回退到 DB 查询
	var agent model.AgentInfo
	if err := s.DB.Where("ip_addr = ?", ip).First(&agent).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"code":    404,
			"message": "Agent 不存在: " + ip,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"hostname":       agent.Hostname,
			"ip_addr":        agent.IPAddr,
			"online":         agent.Online,
			"version":        agent.Version,
			"environment":    agent.Environment,
			"last_seen":      agent.LastSeen,
			"cpu_percent":    0.0,
			"memory_kb":      0,
			"read_kb_per_s":  0.0,
			"write_kb_per_s": 0.0,
		},
	})
}

// ListAgentAudits 获取 Agent 在线/离线/恢复审计日志
// GET /api/v1/agents/audits?limit=20
func (s *APIServer) ListAgentAudits(c *gin.Context) {
	limit := 20
	if raw := c.Query("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}

	var audits []model.AgentAuditLog
	if err := s.DB.Order("created_at DESC, id DESC").Limit(limit).Find(&audits).Error; err != nil {
		s.Logger.Error("查询 Agent 审计日志失败", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    500,
			"message": "查询 Agent 审计日志失败",
		})
		return
	}
	if audits == nil {
		audits = []model.AgentAuditLog{}
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"audits": audits,
			"total":  len(audits),
		},
	})
}
