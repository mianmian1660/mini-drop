#include "agent/HeartbeatThread.h"

#include "agent/AgentUtils.h"
#include "common/Process.h"           // drop::collect_self_pidstats, collect_children_pidstats
#include "common/Utils.h"             // drop::exec_capture
#include "common/Log.h"               // drop::log_event

#include <iostream>
#include <cstdlib>
#include <fstream>
#include <string>
#include <vector>

#include "common/proto/init.grpc.pb.h"

using grpc::ClientContext;
using grpc::Status;
using namespace std;
using namespace std::chrono;

namespace drop_agent
{

    // ============================================================
    // 多 Server 故障转移：遍历 serverAddrs 列表，注册到第一个可用 Server
    // ============================================================
    std::shared_ptr<grpc::Channel> ConnectToServer(
        const AgentConfig &cfg,
        common::CosConfig &cosConfig,
        bool &registered)
    {
        registered = false;

        for (const auto &addr : cfg.serverAddrs)
        {
            cout << "[agent] 尝试连接 Server: " << addr << " ..." << endl;

            auto channel = grpc::CreateChannel(addr, grpc::InsecureChannelCredentials());
            auto stub = initpb::InitAgentInfo::NewStub(channel);

            initpb::RegisterAgentRequest req;
            req.set_hostname(cfg.hostname);
            req.set_ipaddr(cfg.ipAddr);
            req.set_uid(cfg.uid);
            req.set_agentversion(cfg.agentVersion);
            req.set_agent_id(cfg.uid);
            req.set_platform(cfg.platform);
            for (const auto &capability : cfg.capabilities)
                req.add_capabilities(capability);
            for (const auto &label : cfg.labels)
                req.add_labels(label);
            req.set_resource_budget(cfg.resourceBudget);

            initpb::RegisterAgentResponse resp;
            ClientContext ctx;
            ctx.set_deadline(system_clock::now() + seconds(cfg.registerTimeoutSec));

            Status status = stub->RegisterAgent(&ctx, req, &resp);

            if (!status.ok())
            {
                cerr << "[agent]   注册失败: " << status.error_message() << endl;
                drop::log_event("drop_agent", "agent_register_failed",
                                 {{"server_addr", addr}, {"error", status.error_message()}});
                continue;
            }

            cout << "[agent]   在 " << addr << " 注册成功!" << endl;
            drop::log_event("drop_agent", "agent_registered", {{"server_addr", addr}});
            registered = true;

            // 拉取 COS 配置
            initpb::FetchConfigRequest cfgReq;
            cfgReq.set_uid(cfg.uid);
            initpb::FetchConfigResponse cfgResp;
            ClientContext cfgCtx;
            cfgCtx.set_deadline(system_clock::now() + seconds(cfg.registerTimeoutSec));
            Status cfgStatus = stub->FetchConfig(&cfgCtx, cfgReq, &cfgResp);

            if (cfgStatus.ok() && cfgResp.has_cosconfig())
            {
                cosConfig.CopyFrom(cfgResp.cosconfig());
                cout << "[agent]   获取 COS 配置: endpoint=" << cosConfig.endpoint()
                     << " bucket=" << cosConfig.bucket()
                     << " insecure_transport=" << (cosConfig.usessl() ? "false" : "true")
                     << " credentials=redacted" << endl;
            }
            else
            {
                // network_mode=host 时使用 localhost
                cosConfig.set_endpoint("localhost:9000");
                cosConfig.set_accesskeyid("drop");
                cosConfig.set_secretaccesskey("dropdrop");
                cosConfig.set_bucket("drop-data");
                cosConfig.set_usessl(false);
                cout << "[agent]   使用默认 MinIO 配置 (localhost:9000, insecure_transport=true, credentials=redacted)" << endl;
            }

            return channel;
        }

        return nullptr;
    }

    HeartbeatThread::HeartbeatThread(const AgentConfig &cfg,
                                     std::shared_ptr<grpc::Channel> initialChannel,
                                     const common::CosConfig &initialCosConfig,
                                     TaskQueue &taskQueue,
                                     AttemptTracker &attemptTracker,
                                     ServerChannelHolder &channelHolder,
                                     std::atomic<bool> &runningFlag)
        : cfg_(cfg),
          channel_(std::move(initialChannel)),
          cosConfig_(initialCosConfig),
          taskQueue_(taskQueue),
          attemptTracker_(attemptTracker),
          channelHolder_(channelHolder),
          running_(runningFlag)
    {
        healthStub_ = healthcheck::HealthCheck::NewStub(channel_);
        hotmethodStub_ = hotmethod::Hotmethod::NewStub(channel_);

		continuousManager_ = std::make_unique<ContinuousSessionManager>(
			cfg_,
			EnvString("DROP_NATIVE_CP_API_BASE_URL", EnvString("APISERVER_SYMBOL_BASE_URL", "http://127.0.0.1:8191")),
			EnvString("DROP_NATIVE_CP_UID", cfg_.uid),
			running_);
    }

    void HeartbeatThread::Start()
    {
		if (continuousManager_)
			continuousManager_->Start();
        thread_ = std::thread(&HeartbeatThread::Loop, this);
    }

    void HeartbeatThread::Stop()
    {
        if (thread_.joinable())
            thread_.join();
    }

    void HeartbeatThread::StopSampler()
    {
		if (continuousManager_)
			continuousManager_->Stop();
    }

    void HeartbeatThread::Loop()
    {
        uint32_t intervalMs = cfg_.heartbeatIntervalSec * 1000;

        while (running_)
        {
            // 自监控（复用同一个 1 秒采样窗口，同时填充整机 HostStats，
            // 不额外拖慢心跳）
            common::HostStats hostStats;
            common::PidStats selfPs = drop::collect_self_pidstats(&hostStats, cfg_.hostDiskMount);
            vector<common::PidStats> childrenPs = drop::collect_children_pidstats();
            // 主机身份与系统信息（/etc/os-release、uname、/proc/cpuinfo、/proc/uptime）
            common::HostMetadata hostMetadata;
            drop::collect_host_metadata(&hostMetadata);

            healthcheck::HealthCheckRequest req;
            req.set_hostname(cfg_.hostname);
            req.set_ipaddr(cfg_.ipAddr);
            req.set_uid(cfg_.uid);
            req.set_agentversion(cfg_.agentVersion);
            req.set_agent_id(cfg_.uid);
            req.set_platform(cfg_.platform);
            for (const auto &capability : cfg_.capabilities)
                req.add_capabilities(capability);
            for (const auto &label : cfg_.labels)
                req.add_labels(label);
            req.set_resource_budget(cfg_.resourceBudget);
            *req.mutable_selfpstats() = selfPs;
            *req.mutable_host_stats() = hostStats;
            *req.mutable_host_metadata() = hostMetadata;
            if (!childrenPs.empty())
                *req.mutable_childrenpstats() = childrenPs[0];
            for (const auto &attempt : attemptTracker_.RunningSnapshot())
                *req.add_running_attempts() = attempt;
            for (const auto &attempt : attemptTracker_.CompletedSnapshot())
                *req.add_completed_attempts() = attempt;

            cout << "[heartbeat] 自监控: CPU=" << selfPs.cpupercent()
                 << "% RSS=" << selfPs.rsskb() << "KB"
                 << " 子进程=" << childrenPs.size()
                 << " 整机CPU=" << (hostStats.cpu_available() ? hostStats.cpu_percent() : -1.0) << "%"
                 << " 内存=" << (hostStats.memory_available() ? hostStats.memory_percent() : -1.0) << "%"
                 << " 磁盘=" << (hostStats.disk_available() ? hostStats.disk_percent() : -1.0) << "%"
                 << " 主机=" << hostMetadata.os_name() << " " << hostMetadata.os_version()
                 << " 内核=" << hostMetadata.kernel_version()
                 << " 架构=" << hostMetadata.architecture()
                 << " 核数=" << hostMetadata.cpu_cores() << endl;

            healthcheck::HealthCheckResponse resp;
            ClientContext ctx;
            ctx.set_deadline(system_clock::now() + seconds(cfg_.heartbeatTimeoutSec));
            Status status = healthStub_->Do(&ctx, req, &resp);

            if (!status.ok())
            {
                cerr << "[heartbeat] 心跳失败: " << status.error_message()
                     << " — 尝试重连..." << endl;
                drop::log_event("drop_agent", "heartbeat_failed", {{"error", status.error_message()}});

                // 故障转移：重新尝试连接
                auto newChannel = ConnectToServer(cfg_, cosConfig_, registered_);
                if (newChannel && registered_)
                {
                    channel_ = newChannel;
                    healthStub_ = healthcheck::HealthCheck::NewStub(channel_);
                    hotmethodStub_ = hotmethod::Hotmethod::NewStub(channel_);
                    channelHolder_.Update(channel_, hotmethodStub_, cosConfig_);
                    cout << "[heartbeat] 已切换到备用 Server" << endl;
                }
            }
            else
            {
                cout << "[heartbeat] 心跳 OK, pending=" << resp.pending() << endl;

                if (resp.pending() && resp.has_taskdesc())
                {
                    const auto &task = resp.taskdesc();
                    cout << "[heartbeat] 收到任务! taskID=" << task.taskid()
                         << " taskKind=" << task.task_kind()
                         << " requestID=" << task.request_id()
                         << " attemptID=" << task.attempt_id()
                         << " deadlineUnixMs=" << task.deadline_unix_ms()
                         << " profilerType=" << task.profilertype()
                         << " pid=" << task.sampleargv().pid()
                         << " hz=" << task.sampleargv().hz()
                         << " duration=" << task.sampleargv().duration() << endl;
                    drop::log_event("drop_agent", "task_received",
                                     {{"task_id", task.taskid()},
                                      {"task_kind", task.task_kind()},
                                      {"attempt_id", to_string(task.attempt_id())},
                                      {"profiler_type", to_string(task.profilertype())}});

                    attemptTracker_.MarkRunning(task.taskid(), task.attempt_id());
                    taskQueue_.Push(task);
                }

                // Phase 7：处理 Server 下发的取消指令。每次心跳都会重发直到
                // Agent 通过 completed_attempts 明确确认，所以这里的处理必须
                // 幂等——重复收到同一个 taskID/attemptID 不会有副作用。
                for (const auto &cancelReq : resp.cancel_attempts())
                {
                    const string &taskID = cancelReq.taskid();
                    uint64_t attemptID = cancelReq.attempt_id();
                    if (taskQueue_.CancelQueued(taskID, attemptID))
                    {
                        // 命中还在排队、Worker 线程根本没开始跑的任务：直接摘除，
                        // 不会产生任何采集/上传行为。已知限制：这条路径不会走
                        // NotifyResult 上报一个带 TASK_CANCELED 错误码的
                        // TaskResult——只是心跳快照里不再算 running/直接算
                        // completed，analysis 侧看不到这次 attempt 的记录。
                        cout << "[heartbeat] 取消指令命中排队中任务(尚未开始采集)，直接摘除: taskID="
                             << taskID << " attemptID=" << attemptID << endl;
                        attemptTracker_.MarkCompleted(taskID, attemptID);
                        drop::log_event("drop_agent", "task_canceled_before_start",
                                         {{"task_id", taskID}, {"attempt_id", to_string(attemptID)}});
                    }
                    else if (attemptTracker_.IsRunning(taskID, attemptID))
                    {
                        // 已被 Worker 取走或正在上传：登记取消标记；运行中的
                        // Runner 会在下一轮 Poll 感知并触发 Stop(kCancel)。
                        attemptTracker_.RequestCancel(taskID, attemptID);
                        cout << "[heartbeat] 取消指令已登记，等待 Worker 线程下一轮 Poll 感知: taskID="
                             << taskID << " attemptID=" << attemptID << endl;
                    }
                    else
                    {
                        // Server 会持续重发直到 completed_attempts 明确确认。
                        // 本机既不在队列也不在 running，说明任务从未到达、已经
                        // 完成或状态已过期；确认完成可让 Server 清理取消意图，
                        // 避免无效请求永久占用 pending_cancels_。
                        attemptTracker_.MarkCompleted(taskID, attemptID);
                        cout << "[heartbeat] 取消指令对应任务不在本机，确认完成: taskID="
                             << taskID << " attemptID=" << attemptID << endl;
                    }
                }
            }

            for (uint32_t i = 0; i < intervalMs / 100 && running_; i++)
                this_thread::sleep_for(milliseconds(100));
        }
    }

} // namespace drop_agent
