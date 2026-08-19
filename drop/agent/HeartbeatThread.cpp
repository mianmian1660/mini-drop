#include "agent/HeartbeatThread.h"

#include "agent/AgentUtils.h"
#include "common/Process.h"           // drop::collect_self_pidstats, collect_children_pidstats
#include "common/Utils.h"             // drop::exec_capture
#include "common/Log.h"               // drop::log_event

#include <iostream>
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

    namespace
    {

        string ExtractJsonString(const string &json, const string &key)
        {
            string needle = "\"" + key + "\"";
            size_t pos = json.find(needle);
            if (pos == string::npos)
                return "";
            pos = json.find(':', pos + needle.size());
            if (pos == string::npos)
                return "";
            pos = json.find('"', pos);
            if (pos == string::npos)
                return "";
            size_t end = pos + 1;
            while (end < json.size())
            {
                if (json[end] == '"' && json[end - 1] != '\\')
                    return json.substr(pos + 1, end - pos - 1);
                ++end;
            }
            return "";
        }

        string CreateNativeContinuousSession(const AgentConfig &cfg,
                                              const string &apiBaseURL,
                                              const string &authUID)
        {
            string path = "/tmp/mini_drop_native_cp_create_session.json";
            {
                ofstream out(path, ios::binary);
                if (!out.is_open())
                    return "";
                out << "{"
                    << "\"name\":\"Native Dual-Track Continuous Profiling\","
                    << "\"target_ip\":\"" << JsonEscape(cfg.ipAddr) << "\","
                    << "\"hostname\":\"" << JsonEscape(cfg.hostname) << "\","
                    << "\"service_name\":\"hotmethod\","
                    << "\"sample_rate_hz\":19,"
                    << "\"aggregation_window_sec\":10,"
                    << "\"upload_batch_sec\":60,"
                    << "\"retention_hours\":24,"
                    << "\"labels\":{\"job\":\"hotmethod\",\"instance\":\"" << JsonEscape(cfg.ipAddr)
                    << "\",\"node\":\"" << JsonEscape(cfg.hostname) << "\"},"
                    << "\"capabilities\":{"
                    << "\"sampler\":\"dual_track\","
                    << "\"cpu_backend\":\"core,bpftrace,perf\","
                    << "\"io_backend\":\"bpftrace\","
                    << "\"sched_backend\":\"bpftrace\","
                    << "\"signals\":\"" << JsonEscape(EnvString("DROP_NATIVE_CP_SIGNALS", "cpu,io,sched")) << "\","
                    << "\"unavailable_reason\":\"\""
                    << "}"
                    << "}";
            }
            string response;
            int rc = drop::exec_capture({"curl", "-sS", "-m", "10", "-X", "POST",
                                         "-H", "Content-Type: application/json",
                                         "-H", "Drop-User-Uid: " + authUID,
                                         "-d", "@" + path,
                                         apiBaseURL + "/api/v1/internal/continuous/sessions"},
                                        &response, 8192);
            ::remove(path.c_str());
            if (rc != 0)
            {
                cout << "[native-cp] 创建 ContinuousSession 失败 rc=" << rc << " response=" << response << endl;
                return "";
            }
            string sid = ExtractJsonString(response, "sid");
            if (sid.empty())
                cout << "[native-cp] 创建 ContinuousSession 响应中未找到 sid: " << response << endl;
            return sid;
        }

        void EnsureNativeContinuousSampler(drop::DualTrackContinuousSampler &sampler,
                                            const AgentConfig &cfg,
                                            const string &apiBaseURL,
                                            const string &authUID,
                                            string &sessionSID,
                                            steady_clock::time_point &nextRetryAt)
        {
            if (!EnvEnabled("DROP_NATIVE_CP_ENABLED") || sampler.Running())
                return;
            auto now = steady_clock::now();
            if (now < nextRetryAt)
                return;
            if (sessionSID.empty())
                sessionSID = CreateNativeContinuousSession(cfg, apiBaseURL, authUID);
            if (sessionSID.empty())
            {
                nextRetryAt = now + seconds(30);
                cout << "[native-cp] session not ready, retry in 30s" << endl;
                return;
            }

            drop::ContinuousSamplerConfig nativeCfg;
            nativeCfg.sampleRateHz = 19;
            nativeCfg.aggregationWindowSec = 10;
            nativeCfg.uploadBatchSec = 60;
            nativeCfg.retentionHours = 24;
            nativeCfg.sessionSID = sessionSID;
            nativeCfg.targetIP = cfg.ipAddr;
            nativeCfg.hostname = cfg.hostname;
            nativeCfg.apiBaseURL = apiBaseURL;
            nativeCfg.authUID = authUID;
            string samplerError;
            if (sampler.Start(nativeCfg, &samplerError))
            {
                cout << "[native-cp] DualTrackContinuousSampler started session=" << sessionSID
                     << " api=" << apiBaseURL << endl;
                return;
            }
            cout << "[native-cp] DualTrackContinuousSampler not started: " << samplerError << endl;
            nextRetryAt = now + seconds(30);
        }

    } // namespace

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

        nativeCPAPIBaseURL_ = EnvString("DROP_NATIVE_CP_API_BASE_URL", EnvString("APISERVER_SYMBOL_BASE_URL", "http://127.0.0.1:8191"));
        nativeCPAuthUID_ = EnvString("DROP_NATIVE_CP_UID", cfg_.uid);
        nativeCPSessionSID_ = EnvString("DROP_NATIVE_CP_SESSION_ID");
    }

    void HeartbeatThread::Start()
    {
        thread_ = std::thread(&HeartbeatThread::Loop, this);
    }

    void HeartbeatThread::Stop()
    {
        if (thread_.joinable())
            thread_.join();
    }

    void HeartbeatThread::StopSampler()
    {
        nativeSampler_.Stop();
    }

    void HeartbeatThread::Loop()
    {
        uint32_t intervalMs = cfg_.heartbeatIntervalSec * 1000;

        while (running_)
        {
            EnsureNativeContinuousSampler(nativeSampler_, cfg_, nativeCPAPIBaseURL_, nativeCPAuthUID_,
                                          nativeCPSessionSID_, nativeCPNextRetryAt_);

            // 自监控
            common::PidStats selfPs = drop::collect_self_pidstats();
            vector<common::PidStats> childrenPs = drop::collect_children_pidstats();

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
            if (!childrenPs.empty())
                *req.mutable_childrenpstats() = childrenPs[0];
            for (const auto &attempt : attemptTracker_.RunningSnapshot())
                *req.add_running_attempts() = attempt;
            for (const auto &attempt : attemptTracker_.CompletedSnapshot())
                *req.add_completed_attempts() = attempt;

            cout << "[heartbeat] 自监控: CPU=" << selfPs.cpupercent()
                 << "% RSS=" << selfPs.rsskb() << "KB"
                 << " 子进程=" << childrenPs.size() << endl;

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
            }

            for (uint32_t i = 0; i < intervalMs / 100 && running_; i++)
                this_thread::sleep_for(milliseconds(100));
        }
    }

} // namespace drop_agent
