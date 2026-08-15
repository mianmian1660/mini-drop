// ============================================================
// server — HTTP 服务核心
// APIServer 结构体持有所有依赖，并注册全部路由
// ============================================================

package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/mini-drop/apiserver/config"
	"github.com/mini-drop/apiserver/middleware"
	"github.com/mini-drop/apiserver/model"
	"github.com/mini-drop/apiserver/pkg/storage"
	pb "github.com/mini-drop/apiserver/proto/control"
	"github.com/mini-drop/apiserver/util"
)

// APIServer 是 HTTP 服务的顶层结构体
// 持有数据库、日志、配置、gRPC 客户端、对象存储等依赖
type APIServer struct {
	DB         *gorm.DB
	Logger     *zap.Logger
	Config     *config.Config
	Router     *gin.Engine
	GRPCConn   *grpc.ClientConn        // gRPC 连接（到 drop_server）
	ControlCli pb.ControlClient        // Control 服务客户端
	Storage    storage.Storage         // 对象存储（MinIO）
	Cron       *cron.Cron              // 定时任务调度器（W5）
	CronJobs   map[string]cron.EntryID // SID → cron EntryID 映射（支持动态停止/删除）
	TaskSvc    *TaskService            // 阶段 2：任务编排服务
	ProfileCli ProfileClient           // Native Continuous Profiling 查询客户端
}

// New 创建一个新的 APIServer 实例
func New(db *gorm.DB, logger *zap.Logger, cfg *config.Config) *APIServer {
	// 设置 Gin 运行模式
	gin.SetMode(cfg.Server.Mode)

	router := gin.New()

	// 全局中间件：RequestID → Recovery → CORS → AccessLog（6.4 节推荐顺序）
	// RequestID 必须排最前面，这样 Recovery/AccessLog 才能拿到同一个 request_id。
	router.Use(middleware.RequestID())
	router.Use(middleware.Recovery(logger))
	router.Use(middleware.CORS())
	router.Use(middleware.AccessLog(logger))

	s := &APIServer{
		DB:     db,
		Logger: logger,
		Config: cfg,
		Router: router,
	}
	s.TaskSvc = NewTaskService(s)
	s.ProfileCli = NewNativeProfileClient()

	// 初始化对象存储（MinIO）
	if err := s.initStorage(); err != nil {
		logger.Warn("MinIO 初始化失败（文件上传/签名功能不可用）",
			zap.String("endpoint", cfg.Storage.Endpoint),
			zap.String("access_key", util.RedactSecret(cfg.Storage.AccessKey)),
			zap.Error(err),
		)
		// 不阻止启动，降级运行
	}

	// 初始化 gRPC 连接（连接失败不阻止启动，后续 CreateTask 会回退到仅写库模式）
	s.initGRPC()

	// A2: 启动一次性对账（4.6 节：Server 重启后从 DB 恢复待调度工作）
	// 等待一段时间让 gRPC 有机会连接上，再扫描"卡在 Created 状态"的孤儿任务。
	go func() {
		time.Sleep(10 * time.Second)
		s.reconcileOrphanedTasks()
	}()

	// A5: 启动 Outbox 分发器（9.6 节 Transactional Outbox）
	// CreateTask/RetryTask/executeScheduledTask 只负责把"需要下发"这件事和任务记录
	// 一起原子落库，真正的 gRPC 下发在这里异步执行。
	go s.dispatchOutboxLoop()

	// 启动任务状态轮询器（W3：定期检查 Running 任务是否应标记为完成）
	go s.startTaskPoller()

	// 启动存储保留期清理器，避免任务产物和 stopped continuous session 长期堆积。
	go s.startRetentionCleaner()

	// B: 启动时补偿历史已完成但未入分析队列的任务，修复回调缺失期间留下的卡住记录。
	go func() {
		time.Sleep(12 * time.Second)
		s.backfillAnalysisQueueForCompletedTasks()
	}()

	// W3: 启动 Agent 自动发现（每 30 秒同步一次）
	go s.startAgentDiscoverer()

	// 初始化定时任务调度器（W5：恢复 DB 中的 cron 任务并启动）
	s.initCron()

	s.registerRoutes()
	return s
}

func (s *APIServer) taskService() *TaskService {
	if s.TaskSvc == nil {
		s.TaskSvc = NewTaskService(s)
	}
	return s.TaskSvc
}

// initGRPC 初始化到 drop_server 的 gRPC 连接（非阻塞，带重试）
// 连接在后台进行，HTTP 服务立即启动
func (s *APIServer) initGRPC() {
	// 先标记为未连接，后台 goroutine 连接成功后更新
	s.GRPCConn = nil
	s.ControlCli = nil

	go func() {
		transportCreds, insecureTransport, credErr := s.grpcTransportCredentials()
		if credErr != nil {
			s.Logger.Error("gRPC 传输凭证配置失败",
				zap.String("addr", s.Config.GRPC.Addr),
				zap.Error(credErr),
			)
			return
		}
		// 重试连接，最多尝试 10 次，每次间隔 2 秒
		for i := 0; i < 10; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.Config.GRPC.TimeoutSec)*time.Second)

			conn, err := grpc.DialContext(ctx,
				s.Config.GRPC.Addr,
				grpc.WithTransportCredentials(transportCreds),
				grpc.WithBlock(),
			)
			cancel()

			if err != nil {
				s.Logger.Warn("gRPC 连接 drop_server 失败，将重试...",
					zap.String("addr", s.Config.GRPC.Addr),
					zap.Int("attempt", i+1),
					zap.Bool("insecure_transport", insecureTransport),
					zap.Error(err),
				)
				time.Sleep(2 * time.Second)
				continue
			}

			s.GRPCConn = conn
			s.ControlCli = pb.NewControlClient(conn)
			s.Logger.Info("gRPC 连接 drop_server 成功",
				zap.String("addr", s.Config.GRPC.Addr),
				zap.Int("attempts", i+1),
				zap.Bool("insecure_transport", insecureTransport),
			)
			return
		}

		s.Logger.Error("gRPC 连接 drop_server 最终失败（任务将仅写库，不下发）",
			zap.String("addr", s.Config.GRPC.Addr),
		)
	}()
}

func (s *APIServer) grpcTransportCredentials() (credentials.TransportCredentials, bool, error) {
	cfg := s.Config.GRPC
	hasMTLS := cfg.MTLSCertFile != "" && cfg.MTLSKeyFile != "" && cfg.MTLSCAFile != ""
	if !hasMTLS {
		return insecure.NewCredentials(), true, nil
	}
	cert, err := tls.LoadX509KeyPair(cfg.MTLSCertFile, cfg.MTLSKeyFile)
	if err != nil {
		return nil, false, fmt.Errorf("加载 gRPC mTLS client cert/key 失败: %w", err)
	}
	caPEM, err := os.ReadFile(cfg.MTLSCAFile)
	if err != nil {
		return nil, false, fmt.Errorf("读取 gRPC mTLS CA 文件失败: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, false, fmt.Errorf("解析 gRPC mTLS CA 文件失败")
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}), false, nil
}

// Close 关闭资源（gRPC 连接、cron 调度器等）
func (s *APIServer) Close() {
	if s.Cron != nil {
		s.Cron.Stop()
	}
	if s.GRPCConn != nil {
		if err := s.GRPCConn.Close(); err != nil {
			s.Logger.Error("关闭 gRPC 连接失败", zap.Error(err))
		}
	}
}

// initStorage 初始化 MinIO 对象存储（W4）
// 连接 MinIO 并确保 drop-data 桶存在
// 使用 publicEndpoint 生成浏览器可访问的预签名 URL
func (s *APIServer) initStorage() error {
	cfg := s.Config.Storage
	if cfg.Endpoint == "" {
		return fmt.Errorf("存储 endpoint 未配置")
	}

	// 如果未配置 public_endpoint，默认使用 endpoint
	publicEndpoint := cfg.PublicEndpoint
	if publicEndpoint == "" {
		publicEndpoint = cfg.Endpoint
	}

	minioStore, err := storage.NewMinIOStorageWithPublic(
		cfg.Endpoint,
		publicEndpoint,
		cfg.AccessKey,
		cfg.SecretKey,
		cfg.UseSSL,
	)
	if err != nil {
		return fmt.Errorf("连接 MinIO 失败: %w", err)
	}

	// 确保 bucket 存在
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := minioStore.EnsureBucket(ctx, cfg.Bucket); err != nil {
		return fmt.Errorf("创建 Bucket 失败: %w", err)
	}

	s.Storage = minioStore
	s.Logger.Info("MinIO 存储初始化成功",
		zap.String("endpoint", cfg.Endpoint),
		zap.String("public_endpoint", publicEndpoint),
		zap.String("bucket", cfg.Bucket),
		zap.String("access_key", util.RedactSecret(cfg.AccessKey)),
	)
	return nil
}

// StorageConnected 返回对象存储是否已连接
func (s *APIServer) StorageConnected() bool {
	return s.Storage != nil
}

// GrpcConnected 返回 gRPC 是否已连接
func (s *APIServer) GrpcConnected() bool {
	return s.ControlCli != nil
}

// startTaskPoller 后台任务状态轮询器（W3）
// 定期扫描 RUNNING / UPLOADING 任务，补齐 PENDING -> RUNNING -> UPLOADING -> DONE/FAILED 状态链。
// 生产环境中应替换为 gRPC 回调或消息队列通知
func (s *APIServer) startTaskPoller() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		s.pollRunningTasks()
	}
}

// pollRunningTasks 检查 RUNNING / UPLOADING 任务是否应推进状态。
func (s *APIServer) pollRunningTasks() {
	var tasks []model.HotmethodTask

	if err := s.DB.Where("status IN ?", []int{TaskStatusRunning, TaskStatusUploading}).Find(&tasks).Error; err != nil {
		s.Logger.Error("轮询任务状态失败", zap.Error(err))
		return
	}

	now := time.Now()
	for _, task := range tasks {
		if task.BeginTime == nil {
			continue
		}

		// 解析任务参数获取采集时长
		var params PerfParams
		if err := util.UnmarshalJSONB(task.RequestParams, &params); err != nil {
			s.Logger.Warn("解析任务参数失败", zap.String("tid", task.TID), zap.Error(err))
			continue
		}

		// 计算预期完成时间 = beginTime + duration + 30s 上传缓冲
		expectedDuration := time.Duration(params.Duration) * time.Second
		uploadBuffer := 30 * time.Second
		uploadStart := task.BeginTime.Add(expectedDuration)
		deadline := task.BeginTime.Add(expectedDuration).Add(uploadBuffer)

		if task.Status == TaskStatusRunning && !now.Before(uploadStart) {
			_ = s.transitionTaskStatus(&task, TaskStatusUploading, "采集窗口结束，等待 Agent 上传产物", "task_poller", nil)
			s.Logger.Info("任务进入上传阶段",
				zap.String("tid", task.TID),
				zap.String("name", task.Name),
			)
			continue
		}

		if task.Status == TaskStatusUploading {
			rawKey, rawSize, hasArtifacts := s.findRawCollectionArtifact(task.TID)
			if hasArtifacts || now.After(deadline) {
				reason := "采集产物已上传，任务完成"
				if !hasArtifacts {
					reason = "上传等待窗口结束，任务自动标记完成"
				}
				endTime := now
				_ = s.transitionTaskStatus(&task, TaskStatusDone, reason, "task_poller", map[string]interface{}{"end_time": &endTime})
				if hasArtifacts {
					if err := s.ensureAnalysisQueued(task.TID, rawKey, rawSize); err != nil {
						s.Logger.Warn("轮询补建分析任务失败",
							zap.String("tid", task.TID),
							zap.String("raw_key", rawKey),
							zap.Error(err),
						)
					}
				}
				s.Logger.Info("任务自动标记为完成",
					zap.String("tid", task.TID),
					zap.String("name", task.Name),
					zap.Bool("has_artifacts", hasArtifacts),
				)
			}
		}
	}
}

func (s *APIServer) backfillAnalysisQueueForCompletedTasks() {
	var tasks []model.HotmethodTask
	if err := s.DB.Where("status = ? AND analysis_status = ?", TaskStatusDone, 0).
		Order("create_time ASC").
		Limit(100).
		Find(&tasks).Error; err != nil {
		s.Logger.Warn("补偿分析队列查询失败", zap.Error(err))
		return
	}

	for _, task := range tasks {
		rawKey, rawSize, ok := s.findRawCollectionArtifact(task.TID)
		if !ok {
			continue
		}
		if err := s.ensureAnalysisQueued(task.TID, rawKey, rawSize); err != nil {
			s.Logger.Warn("补偿分析队列失败",
				zap.String("tid", task.TID),
				zap.String("raw_key", rawKey),
				zap.Error(err),
			)
			continue
		}
		s.Logger.Info("已补偿历史任务进入分析队列",
			zap.String("tid", task.TID),
			zap.String("raw_key", rawKey),
		)
	}
}

// dispatchOutboxLoop 后台 Outbox 分发器（A5，新复刻指南 9.6 节）。
// 每 2 秒领取一批未发布的 outbox 记录，执行真正的 gRPC 下发。
// 这个间隔比同步下发多了最多 2 秒的延迟，换来的是"任务落库"和"下发意图落库"
// 的原子性——进程崩溃发生在两者之间不会再产生需要靠时间猜的孤儿任务
// （A2 的 reconcileOrphanedTasks 仍然保留作为兜底：outbox 记录本身发布失败、
// 或者更极端的场景下，两套机制不冲突，互相独立）。
func (s *APIServer) dispatchOutboxLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		s.drainOutbox()
	}
}

// drainOutbox 领取一批未发布的 outbox 记录并逐条处理。
func (s *APIServer) drainOutbox() {
	entries, err := s.claimOutboxEntries(20)
	if err != nil {
		s.Logger.Error("领取 outbox 失败", zap.Error(err))
		return
	}
	for i := range entries {
		s.processOutboxEntry(&entries[i])
	}
}

func (s *APIServer) claimOutboxEntries(limit int) ([]model.Outbox, error) {
	var entries []model.Outbox
	now := time.Now()
	lockOwner := fmt.Sprintf("%d-%d", os.Getpid(), now.UnixNano())
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		query := tx.Where("published_at IS NULL AND status <> ? AND (next_attempt_at IS NULL OR next_attempt_at <= ?)", model.OutboxStatusDeadLetter, now).
			Order("created_at ASC").
			Limit(limit)
		if tx.Dialector.Name() == "postgres" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"})
		}
		if err := query.Find(&entries).Error; err != nil {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
		ids := make([]uint, 0, len(entries))
		for _, entry := range entries {
			ids = append(ids, entry.ID)
		}
		return tx.Model(&model.Outbox{}).Where("id IN ?", ids).Updates(map[string]interface{}{
			"status":    model.OutboxStatusProcessing,
			"locked_at": &now,
			"locked_by": lockOwner,
		}).Error
	})
	if err != nil {
		return nil, err
	}
	for i := range entries {
		entries[i].Status = model.OutboxStatusProcessing
		entries[i].LockedAt = &now
		entries[i].LockedBy = lockOwner
	}
	return entries, nil
}

// processOutboxEntry 处理单条 outbox 记录。只有成功下发、内容不可恢复损坏，或
// 达到有限重试上限才标记已发布；短暂的 gRPC 故障保留记录供后续重试。
func (s *APIServer) processOutboxEntry(entry *model.Outbox) {
	if entry.Event != model.OutboxEventDispatchTask {
		s.Logger.Warn("未知的 outbox 事件类型，跳过", zap.Uint("id", entry.ID), zap.String("event", entry.Event))
		s.markOutboxPublished(entry)
		return
	}

	var req CreateTaskReq
	if err := util.UnmarshalJSONB(entry.Payload, &req); err != nil {
		s.Logger.Error("解析 outbox payload 失败", zap.Uint("id", entry.ID), zap.Error(err))
		s.markOutboxPublished(entry)
		return
	}

	var task model.HotmethodTask
	if err := s.DB.Where("tid = ?", entry.AggregateID).First(&task).Error; err != nil {
		s.Logger.Warn("outbox 对应的任务不存在，跳过", zap.String("tid", entry.AggregateID))
		s.markOutboxPublished(entry)
		return
	}

	// 任务已经被别的机制（比如 A2 的启动对账）推进过，不重复下发
	if task.Status != TaskStatusCreated {
		s.markOutboxPublished(entry)
		return
	}

	trigger := model.AttemptTriggerInitial
	if task.MasterTaskTID != "" {
		trigger = model.AttemptTriggerRetry
	}
	if s.GrpcConnected() {
		if err := s.dispatchTask(&task, req, trigger); err == nil {
			s.markOutboxPublished(entry)
		} else {
			s.retryOrFailOutbox(entry, &task, err.Error())
		}
	} else {
		s.recordTaskAttemptFailure(&task, trigger, ErrCodeDependencyUnavailable, "gRPC 未连接，任务无法下发到 drop_server")
		s.retryOrFailOutbox(entry, &task, "gRPC 未连接，任务无法下发到 drop_server")
	}
}

func (s *APIServer) markOutboxPublished(entry *model.Outbox) {
	now := time.Now()
	if err := s.DB.Model(entry).Updates(map[string]interface{}{"published_at": &now, "next_attempt_at": nil, "status": model.OutboxStatusPending, "locked_at": nil, "locked_by": ""}).Error; err != nil {
		s.Logger.Error("标记 outbox 已发布失败", zap.Uint("id", entry.ID), zap.Error(err))
	}
}

func (s *APIServer) retryOrFailOutbox(entry *model.Outbox, task *model.HotmethodTask, reason string) {
	const maxAttempts = 3
	nextAttempt := entry.Attempt + 1
	if nextAttempt >= maxAttempts {
		s.finishLatestTaskAttempt(task.TID, ErrCodeDependencyUnavailable, reason, nil)
		_ = s.transitionTaskStatus(task, TaskStatusFailed, formatErrorReason(ErrCodeDependencyUnavailable, reason), "outbox_dispatcher", nil)
		now := time.Now()
		if err := s.DB.Model(entry).Updates(map[string]interface{}{
			"attempt": nextAttempt, "last_error": reason, "status": model.OutboxStatusDeadLetter, "dead_letter_at": &now, "locked_at": nil, "locked_by": "",
		}).Error; err != nil {
			s.Logger.Error("标记 outbox 死信失败", zap.Uint("id", entry.ID), zap.Error(err))
		}
		return
	}
	delay := time.Duration(1<<uint(entry.Attempt)) * 2 * time.Second
	next := time.Now().Add(delay)
	if err := s.DB.Model(entry).Updates(map[string]interface{}{
		"attempt": nextAttempt, "last_error": reason, "next_attempt_at": &next, "status": model.OutboxStatusPending, "locked_at": nil, "locked_by": "",
	}).Error; err != nil {
		s.Logger.Error("更新 outbox 重试状态失败", zap.Uint("id", entry.ID), zap.Error(err))
	}
	s.Logger.Warn("outbox 下发失败，稍后重试", zap.Uint("id", entry.ID), zap.Int("attempt", nextAttempt), zap.String("error_code", ErrCodeDependencyUnavailable), zap.String("stage", "outbox_retry"))
}

// reconcileOrphanedTasks 启动时执行一次性对账（A2，新复刻指南 4.6 节）。
//
// CreateTask 的实现是"写库 -> 同步调 gRPC 下发"，如果进程恰好在写库成功、
// 下发完成之前崩溃/重启，任务会永久停留在 Created(0) 状态，没有任何机制
// 让它继续往前走。这里扫描这类"孤儿任务"，重新尝试下发；gRPC 仍不可用
// 就显式标记失败，不允许任务无声无息地卡住。
func (s *APIServer) reconcileOrphanedTasks() {
	cutoff := time.Now().Add(-30 * time.Second)

	var tasks []model.HotmethodTask
	if err := s.DB.Where("status = ? AND create_time < ?", TaskStatusCreated, cutoff).Find(&tasks).Error; err != nil {
		s.Logger.Error("启动对账查询失败", zap.Error(err))
		return
	}
	if len(tasks) == 0 {
		s.Logger.Info("启动对账：没有发现停留在 Created 状态的孤儿任务")
		return
	}

	s.Logger.Info("启动对账：发现停留在 Created 状态的孤儿任务", zap.Int("count", len(tasks)))

	for i := range tasks {
		task := &tasks[i]

		var params PerfParams
		if err := util.UnmarshalJSONB(task.RequestParams, &params); err != nil {
			s.Logger.Warn("启动对账：解析任务参数失败", zap.String("tid", task.TID), zap.Error(err))
			_ = s.transitionTaskStatus(task, TaskStatusFailed, formatErrorReason(ErrCodeTaskInvalidArgument, "Server 重启对账：任务参数无法解析"), "startup_reconciler", nil)
			continue
		}

		req := CreateTaskReq{
			Name: task.Name, TaskKind: task.TaskKind, RequestID: task.RequestID, TaskType: task.Type, ProfilerType: task.ProfilerType,
			TargetIP: task.TargetIP, TargetPID: params.TargetPID, Duration: params.Duration,
			Frequency: params.Frequency, Callgraph: params.Callgraph, Event: params.Event,
			Subprocess: params.Subprocess, PprofURL: params.PprofURL,
			ResourceBudget: task.ResourceBudget,
		}

		if s.GrpcConnected() {
			s.Logger.Info("启动对账：重新下发孤儿任务", zap.String("tid", task.TID))
			s.dispatchTask(task, req, model.AttemptTriggerRecovery)
		} else {
			_ = s.transitionTaskStatus(task, TaskStatusFailed, formatErrorReason(ErrCodeDependencyUnavailable, "Server 重启对账：gRPC 仍未连接，任务无法下发"), "startup_reconciler", nil)
		}
	}
}

func (s *APIServer) taskHasArtifacts(tid string) bool {
	if tid == "" {
		return false
	}
	if s.StorageConnected() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		objects, err := s.Storage.ListObjects(ctx, s.Config.Storage.Bucket, tid+"/")
		if err == nil && len(objects) > 0 {
			return true
		}
		if err != nil {
			s.Logger.Warn("检查任务产物失败", zap.String("tid", tid), zap.Error(err))
		}
	}
	return len(s.listLocalFiles(tid)) > 0
}

// startAgentDiscoverer 后台 Agent 自动发现（W3: 每 30 秒通过 gRPC 探测在线 Agent）
func (s *APIServer) startAgentDiscoverer() {
	// 启动后等待 gRPC 连接建立
	time.Sleep(5 * time.Second)

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		if s.ControlCli == nil {
			continue
		}

		// 获取 DB 中已有的 Agent 身份。
		// 远程 Agent 早期可能因隧道/host network 被记录成 127.0.0.1；
		// 如果 hostname/agent_id 里带有 111-230-29-115 这类云主机标识，
		// 也提取成候选 IP 探测，让 Agent 修正 DROP_AGENT_IP 后可以自动“长出”远程主机行。
		var agents []model.AgentInfo
		if err := s.DB.Select("ip_addr, hostname, agent_id").Find(&agents).Error; err != nil {
			continue
		}

		ips := map[string]bool{"127.0.0.1": true}
		for _, a := range agents {
			ips[a.IPAddr] = true
			for _, ip := range candidateIPsFromAgentIdentity(a) {
				ips[ip] = true
			}
		}
		// 额外发现 IP 由部署/演示环境显式配置。它解决的是“数据库里还没有远端行”
		// 的冷启动问题：只要远端 drop_agent 已用 DROP_AGENT_IP=公网 IP 注册，
		// 这里就会主动向 drop_server 查询该 IP，并把它写成独立主机行。
		configuredIPs := map[string]bool{}
		for _, ip := range s.configuredAgentDiscoveryIPs() {
			ips[ip] = true
			configuredIPs[ip] = true
		}

		for ip := range ips {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			req := &pb.StatAgentRequest{TargetIP: ip}
			resp, err := s.ControlCli.StatAgent(ctx, req)
			cancel()

			if err != nil || resp.GetCode() != 0 {
				// Agent 不可达 → 标记离线并写审计
				var existing model.AgentInfo
				if s.DB.Where("ip_addr = ?", ip).First(&existing).Error == nil && existing.Online {
					s.DB.Model(&existing).Update("online", false)
					s.recordAgentAudit(existing.IPAddr, existing.Hostname, "offline", "30s 自动发现探测失败，判定 Agent 离线")
				} else if configuredIPs[ip] {
					s.ensureAgentDiscoveryPlaceholder(ip)
				}
				continue
			}

			// Agent 在线 → 按 agent_id 优先 upsert，避免本机和远程同为 127.0.0.1 时互相覆盖。
			agent, wasOffline, err := s.upsertAgentFromStat(ip, resp)
			if err != nil {
				s.Logger.Warn("自动发现 Agent 写库失败", zap.String("ip", ip), zap.Error(err))
				continue
			}
			if wasOffline {
				s.recordAgentAudit(agent.IPAddr, agent.Hostname, "recovered", "30s 自动发现探测成功，Agent 恢复在线")
			} else if agent.CreatedAt.Equal(agent.UpdatedAt) {
				s.recordAgentAudit(agent.IPAddr, agent.Hostname, "registered", "30s 自动发现发现新 Agent")
				s.Logger.Info("自动发现新 Agent", zap.String("ip", ip), zap.String("agent_id", agent.AgentID))
			}
		}
	}
}

// registerRoutes 注册所有 API 路由
//
// A4: /api/v1 下除 /auth/check 外的所有路由都挂 CheckLogin 中间件，
// 缺失 Drop-User-Uid 时统一 401，不再无条件放通。
// /auth/check 必须留在鉴权组之外——它自己就是用来告诉前端"有没有登录"的，
// 如果也被拦住，前端永远拿不到"未登录"这个明确信号。
func (s *APIServer) registerRoutes() {
	// 健康检查（不需要鉴权）
	s.Router.GET("/healthz", s.Healthz)
	s.Router.GET("/livez", s.Livez)
	s.Router.GET("/readyz", s.Readyz)
	if s.Config == nil || s.Config.Observability.MetricsEnabled {
		s.Router.GET("/metrics", s.Metrics)
	}

	// 鉴权回调（不挂 CheckLogin，这是唯一的例外）
	s.Router.GET("/api/v1/auth/check", s.AuthCheck)
	s.Router.POST("/api/v1/internal/task-notify", s.NotifyTaskResult)

	// 符号库账本（阶段三）：只服务 drop_agent，没有用户身份，
	// 和 task-notify 一样直接挂在 s.Router 上，不进 CheckLogin 分组。
	s.Router.POST("/api/v1/symbols/check", s.CheckSymbols)
	s.Router.PUT("/api/v1/symbols/:build_id", s.UploadSymbol)
	s.Router.POST("/api/v1/kernel-symbols/check", s.CheckKernelSymbol)
	s.Router.PUT("/api/v1/kernel-symbols/:sha256", s.UploadKernelSymbol)

	// 其余 /api/v1 路由：统一要求带身份信息
	api := s.Router.Group("/api/v1")
	api.Use(s.CheckLogin)
	{
		// 用户信息
		api.GET("/users", s.GetCurrentUser)

		// Agent 管理
		api.GET("/agents", s.ListAgents)
		api.GET("/agents/audits", s.ListAgentAudits)
		api.GET("/agent/detail", s.GetAgentDetail)
		api.GET("/agent/stat", s.StatAgent)

		// 任务管理
		api.GET("/task-kinds", s.ListTaskKinds)
		api.POST("/tasks", s.CreateTask)
		api.POST("/tasks/composite", s.CreateCompositeTask)
		api.GET("/tasks", s.ListTasks)
		api.GET("/tasks/:tid/children", s.ListTaskChildren)
		api.GET("/tasks/:tid", s.GetTaskDetail)
		api.DELETE("/tasks/:tid", s.DeleteTask)
		api.POST("/tasks/:tid/retry", s.RetryTask)
		api.POST("/tasks/:tid/cancel", s.CancelTask)
		api.GET("/tasks/:tid/artifacts", s.ListTaskArtifacts)
		api.GET("/tasks/:tid/artifacts/:artifact_id/download", s.DownloadTaskArtifact)
		api.GET("/tasks/:tid/events/stream", s.StreamTaskEvents)
		api.GET("/tasks/:tid/suggestions/stream", s.StreamTaskSuggestions)

		// Periodic Deep Sampling 时间轴（旧 schedule 兼容入口）
		api.GET("/tasks/timeline", s.GetTimeline)
		api.GET("/tasks/diff", s.GetTaskDiff)

		// Native Continuous Profiling 查询
		api.GET("/profile/targets", s.ListProfileTargets)
		api.GET("/profile/query", s.GetProfileFlamegraph)
		api.GET("/profile/flamegraph", s.GetProfileFlamegraph)
		api.GET("/profile/topn", s.GetProfileTopN)
		api.GET("/profile/diff", s.GetProfileDiff)
		api.GET("/profile/label-values", s.GetProfileLabelValues)
		api.POST("/continuous/sessions", s.CreateContinuousSession)
		api.GET("/continuous/sessions", s.ListContinuousSessions)
		api.POST("/continuous/sessions/:sid/stop", s.StopContinuousSession)
		api.POST("/internal/continuous/sessions", s.CreateInternalContinuousSession)
		api.POST("/internal/continuous/batches", s.IngestContinuousBatch)
		api.GET("/continuous/raw", s.ViewContinuousProfileObject)
		api.GET("/continuous/sessions/:sid/timeline", s.GetContinuousTimeline)
		api.GET("/continuous/query", s.QueryContinuousProfile)

		// 文件管理（W4: MinIO 存储集成 + 本地文件降级）
		api.GET("/cosfiles", s.ListCOSFiles)
		api.GET("/cosfiles/view", s.ViewCOSFile)
		api.GET("/cosfiles/download", s.DownloadCOSFile)
		api.POST("/cosfiles/upload", s.UploadTestFile)
		api.GET("/files/:filename", s.ServeLocalFile) // W4: 本地文件服务

		// 用户组管理（W5）
		api.POST("/groups", s.CreateGroup)
		api.GET("/groups", s.ListGroups)
		api.GET("/groups/:gid", s.GetGroupDetail)
		api.PUT("/groups/:gid", s.UpdateGroup)
		api.DELETE("/groups/:gid", s.DeleteGroup)
		api.POST("/groups/:gid/members", s.AddGroupMember)
		api.DELETE("/groups/:gid/members/:uid", s.RemoveGroupMember)

		// 定时任务管理（W5）
		api.POST("/schedule/task", s.CreateSchedule)
		api.GET("/schedule/tasks", s.ListSchedules)
		api.DELETE("/schedule/:sid", s.DeleteSchedule)
		api.POST("/schedule/:sid/toggle", s.ToggleSchedule)
	}

	s.Logger.Info("路由注册完成",
		zap.Int("api_count", 30),
	)
}
