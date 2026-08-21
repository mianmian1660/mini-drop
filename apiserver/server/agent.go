// ============================================================
// server/agent.go — Agent 管理处理器
// 包含：Agent 列表、Agent 资源统计
// W3: 通过 gRPC StatAgent 自动发现 Agent 并同步到 DB
// ============================================================

package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/mini-drop/apiserver/model"
	pb "github.com/mini-drop/apiserver/proto/control"
	"github.com/mini-drop/apiserver/util"
	"gorm.io/gorm"
)

// ListAgents 获取 Agent 列表
// GET /api/v1/agents
// 优先查 DB，DB 为空时尝试通过 gRPC 从 drop_server 发现 Agent
func (s *APIServer) ListAgents(c *gin.Context) {
	var agents []model.AgentInfo
	query := s.DB.Order("last_seen DESC")
	if err := query.Find(&agents).Error; err != nil {
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
	agents = dedupeAgentList(agents)

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
	var online int
	for _, agent := range agents {
		if agent.Online {
			online++
		}
	}
	s.Logger.Info("Agent 指标", zap.String("stage", "agent_metrics"), zap.Int("drop_agents_online", online))

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"agents": agents,
			"total":  len(agents),
		},
	})
}

func agentMetadataFromStat(resp *pb.StatAgentResponse) map[string]interface{} {
	now := time.Now()
	capabilities, _ := util.MarshalJSONB(resp.GetCapabilities())
	labels, _ := util.MarshalJSONB(resp.GetLabels())
	resourceBudget := []byte(resp.GetResourceBudget())
	if len(resourceBudget) == 0 {
		resourceBudget = []byte(`{}`)
	}
	lastSeen := now
	if resp.GetLastSeenUnixMs() > 0 {
		lastSeen = time.UnixMilli(resp.GetLastSeenUnixMs())
	}
	status := "online"
	if !resp.GetOnline() {
		status = "offline"
	}
	updates := map[string]interface{}{
		"online":          resp.GetOnline(),
		"uid":             "",
		"last_seen":       lastSeen,
		"version":         resp.GetVersion(),
		"supported_os":    resp.GetPlatform(),
		"capabilities":    capabilities,
		"labels":          labels,
		"resource_budget": resourceBudget,
		"status":          status,
	}
	if resp.GetHostname() != "" {
		updates["hostname"] = resp.GetHostname()
	}
	if resp.GetAgentId() != "" {
		updates["agent_id"] = resp.GetAgentId()
	}
	return updates
}

func (s *APIServer) upsertAgentFromStat(targetIP string, resp *pb.StatAgentResponse) (model.AgentInfo, bool, error) {
	now := time.Now()
	updates := agentMetadataFromStat(resp)
	agentID := strings.TrimSpace(resp.GetAgentId())

	var existing model.AgentInfo
	var err error
	if agentID != "" {
		err = s.DB.Where("agent_id = ?", agentID).First(&existing).Error
	}
	if agentID == "" || errors.Is(err, gorm.ErrRecordNotFound) {
		err = s.DB.Where("ip_addr = ? AND (agent_id = '' OR agent_id IS NULL)", targetIP).First(&existing).Error
	}
	if err == nil {
		wasOffline := !existing.Online
		updates["ip_addr"] = targetIP
		if err := s.DB.Model(&existing).Updates(updates).Error; err != nil {
			return existing, false, err
		}
		_ = s.DB.Where("id = ?", existing.ID).First(&existing).Error
		s.cleanupDuplicateAgentRows(existing)
		return existing, wasOffline, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return model.AgentInfo{}, false, err
	}

	capabilities, _ := util.MarshalJSONB(resp.GetCapabilities())
	labels, _ := util.MarshalJSONB(resp.GetLabels())
	resourceBudget := []byte(resp.GetResourceBudget())
	if len(resourceBudget) == 0 {
		resourceBudget = []byte(`{}`)
	}
	agent := model.AgentInfo{
		Hostname:       firstNonEmpty(resp.GetHostname(), targetIP),
		IPAddr:         targetIP,
		Online:         resp.GetOnline(),
		AgentID:        agentID,
		Version:        resp.GetVersion(),
		Environment:    s.defaultProfileEnvironment(),
		Capabilities:   capabilities,
		Labels:         labels,
		ResourceBudget: resourceBudget,
		SupportedOS:    resp.GetPlatform(),
		Status:         "online",
		LastSeen:       now,
	}
	if resp.GetLastSeenUnixMs() > 0 {
		agent.LastSeen = time.UnixMilli(resp.GetLastSeenUnixMs())
	}
	if !agent.Online {
		agent.Status = "offline"
	}
	if err := s.DB.Create(&agent).Error; err != nil {
		return agent, false, err
	}
	s.cleanupDuplicateAgentRows(agent)
	return agent, false, nil
}

func dedupeAgentList(agents []model.AgentInfo) []model.AgentInfo {
	out := make([]model.AgentInfo, 0, len(agents))
	seen := map[uint]bool{}
	for _, agent := range agents {
		if agent.ID != 0 {
			if seen[agent.ID] {
				continue
			}
			seen[agent.ID] = true
		}
		out = append(out, agent)
	}
	return out
}

func (s *APIServer) cleanupDuplicateAgentRows(agent model.AgentInfo) {
	if s == nil || s.DB == nil || agent.ID == 0 {
		return
	}
	agentID := strings.TrimSpace(agent.AgentID)
	if agentID == "" {
		return
	}

	// 注册/心跳以 agent_id 作为稳定身份。同一个远端在旧版本或并发发现期间
	// 可能留下重复行；这里保留当前 upsert 命中的记录，删除相同 agent_id 或相同 IP
	// 的旧行，避免 /api/v1/agents 出现两个看起来相同的远程服务器。
	query := s.DB.Where("id <> ? AND agent_id = ?", agent.ID, agentID)
	if ip := strings.TrimSpace(agent.IPAddr); ip != "" {
		query = s.DB.Where("id <> ? AND (agent_id = ? OR ip_addr = ?)", agent.ID, agentID, ip)
	}
	if err := query.
		Delete(&model.AgentInfo{}).Error; err != nil {
		s.Logger.Warn("清理重复 Agent 记录失败", zap.String("agent_id", agentID), zap.Error(err))
	}
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
		updated, wasOffline, err := s.upsertAgentFromStat(agent.IPAddr, resp)
		if err != nil {
			s.Logger.Warn("更新 Agent 状态失败", zap.String("ip", agent.IPAddr), zap.Error(err))
			return
		}
		if wasOffline {
			s.recordAgentAudit(updated.IPAddr, updated.Hostname, "recovered", "gRPC StatAgent 成功，Agent 恢复在线")
		}
		*agent = updated
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
	configuredIPs := map[string]bool{}
	for _, ip := range s.configuredAgentDiscoveryIPs() {
		configuredIPs[ip] = true
	}

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
	for ip := range configuredIPs {
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
			if configuredIPs[ip] {
				if placeholder, ok := s.ensureAgentDiscoveryPlaceholder(ip); ok {
					discovered = append(discovered, placeholder)
				}
			}
			continue // 该 IP 无响应，跳过
		}

		if resp.GetCode() == 0 {
			agent, wasOffline, err := s.upsertAgentFromStat(ip, resp)
			if err != nil {
				s.Logger.Warn("自动发现 Agent 写库失败", zap.String("ip", ip), zap.Error(err))
				continue
			}
			if wasOffline {
				s.recordAgentAudit(agent.IPAddr, agent.Hostname, "recovered", "自动发现探测成功，Agent 恢复在线")
			} else if agent.CreatedAt.Equal(agent.UpdatedAt) {
				s.recordAgentAudit(agent.IPAddr, agent.Hostname, "registered", "自动发现新 Agent")
			}
			discovered = append(discovered, agent)

			s.Logger.Info("发现 Agent",
				zap.String("ip", ip),
				zap.Float64("cpu", resp.GetCpuPercent()),
				zap.Uint64("mem_kb", resp.GetMemoryKb()),
			)
		} else if configuredIPs[ip] {
			if placeholder, ok := s.ensureAgentDiscoveryPlaceholder(ip); ok {
				discovered = append(discovered, placeholder)
			}
		}
	}

	return discovered
}

var dashedIPPattern = regexp.MustCompile(`(?:\d{1,3}[-.]){3}\d{1,3}`)

func candidateIPsFromAgentIdentity(agent model.AgentInfo) []string {
	values := []string{agent.IPAddr, agent.Hostname, agent.AgentID}
	out := []string{}
	seen := map[string]bool{}
	for _, value := range values {
		for _, match := range dashedIPPattern.FindAllString(value, -1) {
			ip := strings.ReplaceAll(match, "-", ".")
			if parsed := net.ParseIP(ip); parsed != nil && !seen[ip] {
				seen[ip] = true
				out = append(out, ip)
			}
		}
	}
	return out
}

func (s *APIServer) configuredAgentDiscoveryIPs() []string {
	if s == nil || s.Config == nil {
		return nil
	}
	raw := strings.TrimSpace(s.Config.AgentDiscovery.ExtraIPs)
	if raw == "" {
		return nil
	}

	// AGENT_DISCOVERY_EXTRA_IPS 只接收 IP 列表，不接收用户名、密码或 SSH 参数。
	// 远端 drop_agent 自己连回 drop_server 后，apiserver 再用这些 IP 调 StatAgent；
	// 因此这里的地址必须和远端 DROP_AGENT_IP 保持一致，例如 111.230.29.115。
	out := []string{}
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		ip := strings.TrimSpace(part)
		if ip == "" || seen[ip] {
			continue
		}
		if parsed := net.ParseIP(ip); parsed == nil {
			continue
		}
		seen[ip] = true
		out = append(out, ip)
	}
	return out
}

func (s *APIServer) ensureAgentDiscoveryPlaceholder(ip string) (model.AgentInfo, bool) {
	ip = strings.TrimSpace(ip)
	if s == nil || s.DB == nil || ip == "" {
		return model.AgentInfo{}, false
	}

	var existing model.AgentInfo
	err := s.DB.Where("ip_addr = ?", ip).First(&existing).Error
	if err == nil {
		return existing, false
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		s.Logger.Warn("查询远端 Agent 占位记录失败", zap.String("ip", ip), zap.Error(err))
		return model.AgentInfo{}, false
	}

	// 对显式配置的远端 IP，即使当前 StatAgent 还不可达，也先写一条 offline 行。
	// 这样首页能清楚区分“远端主机已配置但 drop_agent 未连回”和“本机在线”；
	// 后续远端 drop_agent 用 DROP_AGENT_IP=同一 IP 注册后，upsertAgentFromStat 会更新这条记录。
	agent := model.AgentInfo{
		Hostname:       ip,
		IPAddr:         ip,
		Online:         false,
		Environment:    "production",
		Capabilities:   []byte(`[]`),
		Labels:         []byte(`[]`),
		ResourceBudget: []byte(`{}`),
		Status:         "offline",
		LastSeen:       time.Now(),
	}
	if err := s.DB.Create(&agent).Error; err != nil {
		s.Logger.Warn("创建远端 Agent 占位记录失败", zap.String("ip", ip), zap.Error(err))
		return model.AgentInfo{}, false
	}
	s.recordAgentAudit(agent.IPAddr, agent.Hostname, "registered", "配置的远端 Agent 首次纳入首页展示，等待 drop_agent 连回")
	return agent, true
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
			// 同时按 agent_id 优先写库，保证实时资源、首页发现、详情页看到的是同一台 Agent。
			agent, wasOffline, err := s.upsertAgentFromStat(ip, resp)
			if err != nil {
				s.Logger.Warn("实时资源查询写库失败", zap.String("ip", ip), zap.Error(err))
			} else if wasOffline {
				s.recordAgentAudit(agent.IPAddr, agent.Hostname, "recovered", "实时资源查询成功，Agent 恢复在线")
			}

			c.JSON(http.StatusOK, gin.H{
				"code": 0,
				"data": gin.H{
					"hostname":        agent.Hostname,
					"ip_addr":         ip,
					"online":          true,
					"version":         agent.Version,
					"environment":     agent.Environment,
					"supported_os":    agent.SupportedOS,
					"capabilities":    agent.Capabilities,
					"labels":          agent.Labels,
					"resource_budget": agent.ResourceBudget,
					"status":          agent.Status,
					"last_seen":       agent.LastSeen,
					"cpu_percent":     resp.GetCpuPercent(),
					"memory_kb":       resp.GetMemoryKb(),
					"read_kb_per_s":   resp.GetReadKbPerS(),
					"write_kb_per_s":  resp.GetWriteKbPerS(),
				},
			})
			return
		}
	}

	// gRPC 不可达 → 回退到 DB 查询
	var agent model.AgentInfo
	if err := s.DB.Where("ip_addr = ?", ip).First(&agent).Error; err != nil {
		s.Logger.Warn("查询 Agent 资源统计失败", zap.String("ip", ip), zap.Error(err))
		c.JSON(http.StatusNotFound, gin.H{
			"code":    404,
			"message": "Agent 不存在: " + ip,
		})
		return
	}
	if !s.canReadAgent(agent, s.AuthContext(c)) {
		s.forbid(c)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"hostname":        agent.Hostname,
			"ip_addr":         agent.IPAddr,
			"online":          agent.Online,
			"version":         agent.Version,
			"environment":     agent.Environment,
			"supported_os":    agent.SupportedOS,
			"capabilities":    agent.Capabilities,
			"labels":          agent.Labels,
			"resource_budget": agent.ResourceBudget,
			"status":          agent.Status,
			"last_seen":       agent.LastSeen,
			"cpu_percent":     0.0,
			"memory_kb":       0,
			"read_kb_per_s":   0.0,
			"write_kb_per_s":  0.0,
		},
	})
}

// GetAgentDetail 获取单个 Agent 的心跳策略、实时资源和最近审计日志。
// GET /api/v1/agent/detail?ip=xxx
func (s *APIServer) GetAgentDetail(c *gin.Context) {
	ip := c.Query("ip")
	if ip == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    400,
			"message": "缺少 ip 参数",
		})
		return
	}

	var agent model.AgentInfo
	if err := s.DB.Where("ip_addr = ?", ip).First(&agent).Error; err != nil {
		s.Logger.Warn("查询 Agent 详情失败", zap.String("ip", ip), zap.Error(err))
		c.JSON(http.StatusNotFound, gin.H{
			"code":    404,
			"message": "Agent 不存在: " + ip,
		})
		return
	}
	if !s.canReadAgent(agent, s.AuthContext(c)) {
		s.forbid(c)
		return
	}

	s.ensureAgentAudited(&agent)
	s.markAgentOfflineIfStale(&agent)

	stat := gin.H{
		"cpu_percent":    0.0,
		"memory_kb":      0,
		"read_kb_per_s":  0.0,
		"write_kb_per_s": 0.0,
		"source":         "db",
	}

	if s.ControlCli != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		resp, err := s.ControlCli.StatAgent(ctx, &pb.StatAgentRequest{TargetIP: ip})
		cancel()
		if err == nil && resp.GetCode() == 0 {
			if !agent.Online {
				s.recordAgentAudit(agent.IPAddr, agent.Hostname, "recovered", "Agent 详情页实时探测成功，Agent 恢复在线")
			}
			agent.Online = true
			agent.LastSeen = time.Now()
			if err := s.DB.Model(&agent).Updates(agentMetadataFromStat(resp)).Error; err != nil {
				s.Logger.Warn("更新 Agent 详情心跳失败", zap.String("ip", ip), zap.Error(err))
			}
			_ = s.DB.Where("ip_addr = ?", ip).First(&agent).Error
			stat = gin.H{
				"cpu_percent":    resp.GetCpuPercent(),
				"memory_kb":      resp.GetMemoryKb(),
				"read_kb_per_s":  resp.GetReadKbPerS(),
				"write_kb_per_s": resp.GetWriteKbPerS(),
				"source":         "grpc",
			}
		} else if err != nil {
			s.Logger.Warn("Agent 详情实时探测失败", zap.String("ip", ip), zap.Error(err))
		} else {
			s.Logger.Warn("Agent 详情实时探测返回失败状态", zap.String("ip", ip), zap.Int32("code", resp.GetCode()), zap.String("msg", resp.GetMsg()))
		}
	}

	var audits []model.AgentAuditLog
	if err := s.DB.Where("ip_addr = ?", ip).
		Order("created_at DESC, id DESC").
		Limit(10).
		Find(&audits).Error; err != nil {
		s.Logger.Warn("查询 Agent 详情审计日志失败", zap.String("ip", ip), zap.Error(err))
		audits = []model.AgentAuditLog{}
	}
	if audits == nil {
		audits = []model.AgentAuditLog{}
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"agent":                  agent,
			"stat":                   stat,
			"heartbeat_interval_sec": 5,
			"offline_after_sec":      30,
			"server_time":            time.Now(),
			"audits":                 audits,
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
