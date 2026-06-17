// ============================================================
// drop_agent (Agent 主程序) — 精简入口
// ============================================================
// 工作循环：注册 → 心跳（带 PidStats）→ 拉任务 → perf 采集 → MinIO 上传 → 回报
// 通用逻辑分布在 common/ 库中：
//   common/Perf.cpp      — perf 采集执行
//   common/Process.cpp   — /proc 读取 + PidStats 采集
//   common/COSClient.cpp — MinIO 上传
//   common/Utils.cpp     — 工具函数
// ============================================================

#include "common/Perf.h"      // drop::run_perf
#include "common/Process.h"   // drop::collect_self_pidstats, collect_children_pidstats
#include "common/COSClient.h" // drop::upload_to_minio
#include "common/Utils.h"     // drop::read_file_content

#include <iostream>
#include <string>
#include <thread>
#include <chrono>
#include <vector>
#include <atomic>
#include <csignal>

#include <grpcpp/grpcpp.h>
#include "common/proto/healthcheck.grpc.pb.h"
#include "common/proto/hotmethod.grpc.pb.h"
#include "common/proto/init.grpc.pb.h"

using grpc::Channel;
using grpc::ClientContext;
using grpc::Status;
using namespace std;
using namespace std::chrono;

static atomic<bool> agent_running{true};

// ============================================================
// main：Agent 入口 — 使用 common/ 库实现
// ============================================================
int main(int argc, char **argv)
{
    // ---------- 1. 连接地址 ----------
    string server_addr = "localhost:50051";
    if (argc > 1)
        server_addr = argv[1];

    signal(SIGINT, [](int)
           { agent_running = false; });
    signal(SIGTERM, [](int)
           { agent_running = false; });

    cout << "[agent] drop_agent 启动，连接 server: " << server_addr << endl;

    auto channel = grpc::CreateChannel(server_addr, grpc::InsecureChannelCredentials());

    // ---------- 2. 注册 + 拉取 COS 配置 ----------
    common::CosConfig cosConfig;
    {
        auto stub = initpb::InitAgentInfo::NewStub(channel);

        initpb::RegisterAgentRequest req;
        req.set_hostname("demo-host");
        req.set_ipaddr("127.0.0.1");
        req.set_uid("agent-001");
        req.set_agentversion("1.0.0");

        initpb::RegisterAgentResponse resp;
        ClientContext ctx;
        Status status = stub->RegisterAgent(&ctx, req, &resp);

        if (!status.ok())
        {
            cerr << "[agent] 注册失败: " << status.error_message() << endl;
            return 1;
        }
        cout << "[agent] 注册成功" << endl;

        // 拉取 COS 配置
        initpb::FetchConfigRequest cfgReq;
        cfgReq.set_uid("agent-001");
        initpb::FetchConfigResponse cfgResp;
        ClientContext cfgCtx;
        Status cfgStatus = stub->FetchConfig(&cfgCtx, cfgReq, &cfgResp);

        if (cfgStatus.ok() && cfgResp.has_cosconfig())
        {
            cosConfig.CopyFrom(cfgResp.cosconfig());
            cout << "[agent] 获取 COS 配置: endpoint=" << cosConfig.endpoint()
                 << " bucket=" << cosConfig.bucket() << endl;
        }
        else
        {
            cosConfig.set_endpoint("minio:9000");
            cosConfig.set_accesskeyid("drop");
            cosConfig.set_secretaccesskey("dropdrop");
            cosConfig.set_bucket("drop");
            cosConfig.set_usessl(false);
            cout << "[agent] 使用默认 MinIO 配置" << endl;
        }
    }

    // ---------- 3. 心跳循环 ----------
    auto health_stub = healthcheck::HealthCheck::NewStub(channel);
    auto hotmethod_stub = hotmethod::Hotmethod::NewStub(channel);

    while (agent_running)
    {
        // 自监控
        common::PidStats selfPs = drop::collect_self_pidstats();
        vector<common::PidStats> childrenPs = drop::collect_children_pidstats();

        // 填心跳
        healthcheck::HealthCheckRequest req;
        req.set_hostname("demo-host");
        req.set_ipaddr("127.0.0.1");
        req.set_uid("agent-001");
        req.set_agentversion("1.0.0");
        *req.mutable_selfpstats() = selfPs;
        if (!childrenPs.empty())
            *req.mutable_childrenpstats() = childrenPs[0];

        cout << "[agent] 自监控: CPU=" << selfPs.cpupercent()
             << "% RSS=" << selfPs.rsskb() << "KB"
             << " 子进程=" << childrenPs.size() << endl;

        healthcheck::HealthCheckResponse resp;
        ClientContext ctx;
        Status status = health_stub->Do(&ctx, req, &resp);

        if (!status.ok())
        {
            cerr << "[agent] 心跳失败: " << status.error_message() << endl;
            goto sleep_and_continue;
        }

        cout << "[agent] 心跳 OK, pending=" << resp.pending() << endl;

        // ----- 有任务？执行采集 -----
        if (resp.pending() && resp.has_taskdesc())
        {
            const auto &task = resp.taskdesc();
            cout << "[agent] 收到任务! taskID=" << task.taskid()
                 << " profilerType=" << task.profilertype()
                 << " pid=" << task.sampleargv().pid()
                 << " hz=" << task.sampleargv().hz()
                 << " duration=" << task.sampleargv().duration() << endl;

            string outputPath = "/tmp/perf_" + task.taskid() + ".data";
            int result = drop::run_perf(task, outputPath);

            hotmethod::TaskResult taskResult;
            taskResult.set_taskid(task.taskid());

            if (result == 0)
            {
                string fileContent = drop::read_file_content(outputPath);
                if (!fileContent.empty())
                {
                    string remoteKey = "perf_" + task.taskid() + ".data";
                    if (drop::upload_to_minio(cosConfig, outputPath, remoteKey))
                        taskResult.set_coskey(remoteKey);

                    auto *file = taskResult.mutable_file();
                    file->set_name("perf.data");
                    file->set_content(fileContent);
                    file->set_size(fileContent.size());
                    cout << "[agent] 采集成功，大小=" << fileContent.size() << " bytes" << endl;
                }
                else
                {
                    taskResult.set_errormessage("采集完成但无法读取输出文件");
                }
            }
            else if (result == -4)
            {
                taskResult.set_errormessage("目标 PID " + to_string(task.sampleargv().pid()) + " 不存在");
            }
            else if (result == -3)
            {
                taskResult.set_errormessage("perf 采集超时（" + to_string(task.timeoutsec()) + "秒）");
            }
            else if (result == -1 || result == -2 || result == -5 || result == -6)
            {
                taskResult.set_errormessage("perf 进程异常, resultCode=" + to_string(result));
            }
            else
            {
                taskResult.set_errormessage("perf 采集失败, exitCode=" + to_string(result));
            }

            // 附加 PidStats
            *taskResult.add_selfpstats() = selfPs;
            for (const auto &c : childrenPs)
                *taskResult.add_childrenpstats() = c;

            // 上报
            google::protobuf::Empty emptyResp;
            ClientContext notifyCtx;
            Status notifyStatus = hotmethod_stub->NotifyResult(&notifyCtx, taskResult, &emptyResp);

            if (notifyStatus.ok())
            {
                cout << "[agent] NotifyResult 上报成功: taskID=" << task.taskid()
                     << " error=" << taskResult.errormessage()
                     << " cosKey=" << taskResult.coskey() << endl;
            }
            else
            {
                cerr << "[agent] NotifyResult 上报失败: " << notifyStatus.error_message() << endl;
            }
        }

    sleep_and_continue:
        // 等 5 秒再发下一次心跳
        for (int i = 0; i < 50 && agent_running; i++)
            this_thread::sleep_for(milliseconds(100));
    }

    cout << "[agent] 收到退出信号，正在关闭..." << endl;
    return 0;
}
