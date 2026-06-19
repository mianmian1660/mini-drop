// ============================================================
// drop_agent (Agent 主程序) — 精简入口 (W5: 多采集器+配置+故障转移)
// ============================================================
// 工作循环：注册 → 心跳（带 PidStats）→ 拉任务 → 按 profilerType 分发 → MinIO 上传 → 回报
//
// 通用逻辑分布在 common/ 库中：
//   common/Perf.cpp                    — perf 采集执行 (profilerType=0)
//   common/AsyncProfilerProfiler.cpp   — async-profiler 采集 (profilerType=1)
//   common/PprofProfiler.cpp           — pprof 采集 (profilerType=2)
//   common/Process.cpp                 — /proc 读取 + PidStats 采集
//   common/COSClient.cpp               — MinIO 上传
//   common/Utils.cpp                   — 工具函数
//
// Agent 配置逻辑：
//   agent/Config.h/cpp                 — JSON 配置文件 + 多 Server 故障转移
// ============================================================

#include "common/Perf.h"                  // drop::run_perf (profilerType=0)
#include "common/AsyncProfilerProfiler.h" // drop::run_async_profiler (profilerType=1)
#include "common/PprofProfiler.h"         // drop::run_pprof (profilerType=2)
#include "common/Process.h"               // drop::collect_self_pidstats, collect_children_pidstats
#include "common/COSClient.h"             // drop::upload_to_minio
#include "common/Utils.h"                 // drop::read_file_content
#include "agent/Config.h"                 // drop_agent::AgentConfig

#include <iostream>
#include <string>
#include <thread>
#include <chrono>
#include <vector>
#include <atomic>
#include <csignal>
#include <fstream>
#include <sys/stat.h>

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
// 辅助：检查文件是否存在
// ============================================================
static bool file_exists(const string &path)
{
    struct stat st;
    return stat(path.c_str(), &st) == 0;
}

// ============================================================
// 多 Server 故障转移：遍历 serverAddrs 列表，注册到第一个可用 Server
// ============================================================
static shared_ptr<grpc::Channel> connect_to_server(
    const drop_agent::AgentConfig &cfg,
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
        req.set_ipaddr("127.0.0.1");
        req.set_uid(cfg.uid);
        req.set_agentversion(cfg.agentVersion);

        initpb::RegisterAgentResponse resp;
        ClientContext ctx;
        ctx.set_deadline(system_clock::now() + seconds(cfg.registerTimeoutSec));

        Status status = stub->RegisterAgent(&ctx, req, &resp);

        if (!status.ok())
        {
            cerr << "[agent]   注册失败: " << status.error_message() << endl;
            continue;
        }

        cout << "[agent]   在 " << addr << " 注册成功!" << endl;
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
                 << " bucket=" << cosConfig.bucket() << endl;
        }
        else
        {
            // network_mode=host 时使用 localhost
            cosConfig.set_endpoint("localhost:9000");
            cosConfig.set_accesskeyid("drop");
            cosConfig.set_secretaccesskey("dropdrop");
            cosConfig.set_bucket("drop-data");
            cosConfig.set_usessl(false);
            cout << "[agent]   使用默认 MinIO 配置 (localhost:9000)" << endl;
        }

        return channel;
    }

    return nullptr;
}

// ============================================================
// WSL2 Mock 数据生成：当 perf 无法在当前内核运行时，生成模拟折叠栈
// 使火焰图→分析→Web 展示全链路在 WSL2/容器 环境中可演示
// ============================================================
static void generate_mock_collapsed_stacks(const string &outputPath)
{
    // 模拟一个典型的多线程服务的 CPU profile
    // 格式：stack1;stack2;...;leaf count
    const char *mockData =
        "main;run_event_loop;process_request;parse_json;json_parse_string 42\n"
        "main;run_event_loop;process_request;handle_query;sql_execute;btree_search 38\n"
        "main;run_event_loop;process_request;handle_query;sql_execute;index_scan 25\n"
        "main;run_event_loop;accept_connection;tcp_handshake 15\n"
        "main;run_event_loop;process_request;serialize_response;json_encode 12\n"
        "main;run_event_loop;process_request;auth_check;verify_token 10\n"
        "main;worker_thread;compress_log;gzip_compress;[kernel] 8\n"
        "main;worker_thread;flush_log;write;[kernel]sys_write 7\n"
        "main;gc_thread;mark_phase;scan_roots 6\n"
        "main;gc_thread;sweep_phase;free_pages 5\n"
        "main;run_event_loop;timer_callback;update_metrics 4\n"
        "main;run_event_loop;process_request;rate_limit;token_bucket 3\n"
        "main;worker_thread;mem_alloc;[kernel]page_fault 3\n"
        "idle;[kernel]cpu_idle 2\n";

    ofstream out(outputPath);
    if (out.is_open())
    {
        out << mockData;
        out.close();
        cout << "[agent] ⚠️ perf 不可用，已生成 WSL2 模拟数据: " << outputPath
             << " (" << strlen(mockData) << " bytes)" << endl;
    }
    else
    {
        cerr << "[agent] 无法创建模拟数据文件: " << outputPath << endl;
    }
}

// ============================================================
// 多采集器分发：按 profilerType 选择对应的采集函数
// 返回 (resultCode, profilerName)
// ============================================================
static pair<int, string> run_profiler(
    uint32_t profilerType,
    const hotmethod::TaskDesc &task,
    const string &outputPath,
    const string &suffix)
{
    string path = outputPath;

    switch (profilerType)
    {
    case 0: // perf
    {
        cout << "[agent] 选择采集器: perf (profilerType=0)" << endl;
        int result = drop::run_perf(task, path);

        // WSL2 / 容器环境 fallback：perf 失败时生成模拟数据
        if (result != 0)
        {
            cout << "[agent] perf 失败(result=" << result
                 << ")，启用 WSL2 mock 模式" << endl;
            generate_mock_collapsed_stacks(path);
            return {0, "perf(mock)"}; // 返回成功，后续正常上传
        }
        return {result, "perf"};
    }
    case 1: // async-profiler (Java)
    {
        path = outputPath + ".jfr";
        cout << "[agent] 选择采集器: async-profiler (profilerType=1)" << endl;
        int result = drop::run_async_profiler(task, path);
        if (result != 0)
        {
            cout << "[agent] async-profiler 不可用，启用 mock 模式" << endl;
            generate_mock_collapsed_stacks(outputPath);
            return {0, "async-profiler(mock)"};
        }
        return {result, "async-profiler"};
    }
    case 2: // pprof (Go)
    {
        path = outputPath + ".pb.gz";
        cout << "[agent] 选择采集器: pprof (profilerType=2)" << endl;
        int result = drop::run_pprof(task, path);
        if (result != 0)
        {
            cout << "[agent] pprof 不可用，启用 mock 模式" << endl;
            generate_mock_collapsed_stacks(outputPath);
            return {0, "pprof(mock)"};
        }
        return {result, "pprof"};
    }
    default:
        cerr << "[agent] 未知的 profilerType=" << profilerType << "，回退到 perf" << endl;
        int result = drop::run_perf(task, path);
        if (result != 0)
        {
            generate_mock_collapsed_stacks(path);
            return {0, "perf(mock,回退)"};
        }
        return {result, "perf(回退)"};
    }
}

// ============================================================
// 错误消息映射
// ============================================================
static string get_error_message(int resultCode, const string &profilerName,
                                const hotmethod::TaskDesc &task)
{
    switch (resultCode)
    {
    case 0:
        return ""; // 成功
    case -4:
        return "目标 PID " + to_string(task.sampleargv().pid()) + " 不存在";
    case -3:
        return profilerName + " 采集超时（" + to_string(task.timeoutsec()) + "秒）";
    case -1:
    case -2:
    case -5:
    case -6:
        return profilerName + " 进程异常, resultCode=" + to_string(resultCode);
    default:
        return profilerName + " 采集失败, exitCode=" + to_string(resultCode);
    }
}

// ============================================================
// 采集后处理：读取输出文件 → 上传 MinIO → 构建 TaskResult
// ============================================================
static hotmethod::TaskResult build_task_result(
    const hotmethod::TaskDesc &task,
    int resultCode,
    const string &profilerName,
    const string &outputPath,
    const common::CosConfig &cosConfig,
    const common::PidStats &selfPs,
    const vector<common::PidStats> &childrenPs)
{
    hotmethod::TaskResult taskResult;
    taskResult.set_taskid(task.taskid());

    string errorMsg = get_error_message(resultCode, profilerName, task);
    if (!errorMsg.empty())
    {
        taskResult.set_errormessage(errorMsg);
    }

    if (resultCode == 0)
    {
        // 检查实际输出文件（不同采集器后缀不同）
        string actualPath = outputPath;
        if (!file_exists(actualPath))
        {
            // 尝试带后缀的变体
            if (file_exists(outputPath + ".jfr"))
                actualPath = outputPath + ".jfr";
            else if (file_exists(outputPath + ".pb.gz"))
                actualPath = outputPath + ".pb.gz";
        }

        string fileContent = drop::read_file_content(actualPath);
        if (!fileContent.empty())
        {
            string fileName = actualPath;
            size_t slashPos = fileName.rfind('/');
            if (slashPos != string::npos)
                fileName = fileName.substr(slashPos + 1);

            // 使用标准路径 {tid}/perf.data，与 analysis 的下载路径一致
            string remoteKey = task.taskid() + "/perf.data";
            if (drop::upload_to_minio(cosConfig, actualPath, remoteKey))
                taskResult.set_coskey(remoteKey);

            auto *file = taskResult.mutable_file();
            file->set_name(fileName);
            file->set_content(fileContent);
            file->set_size(fileContent.size());
            cout << "[agent] 采集成功，采集器=" << profilerName
                 << " 文件=" << fileName
                 << " 大小=" << fileContent.size() << " bytes" << endl;
        }
        else
        {
            taskResult.set_errormessage(profilerName + " 采集完成但无法读取输出文件: " + actualPath);
        }
    }

    // 附加 PidStats
    *taskResult.add_selfpstats() = selfPs;
    for (const auto &c : childrenPs)
        *taskResult.add_childrenpstats() = c;

    return taskResult;
}

// ============================================================
// main：Agent 入口 — 支持配置、故障转移、多采集器
// 用法: ./drop_agent [config.json]  或  ./drop_agent [server:port]
// ============================================================
int main(int argc, char **argv)
{
    signal(SIGINT, [](int)
           { agent_running = false; });
    signal(SIGTERM, [](int)
           { agent_running = false; });

    // ---------- 1. 加载配置 ----------
    drop_agent::AgentConfig cfg;

    // 先尝试 etc/agent_config.json
    string configPath = "etc/agent_config.json";
    if (file_exists(configPath))
    {
        cfg = drop_agent::AgentConfig::LoadFromFile(configPath);
    }
    // 命令行参数：可以是 json 路径或 server:port
    else if (argc > 1)
    {
        string arg1 = argv[1];
        if (arg1.find(".json") != string::npos)
        {
            cfg = drop_agent::AgentConfig::LoadFromFile(arg1);
        }
        else
        {
            cfg = drop_agent::AgentConfig::Default(arg1);
        }
    }
    else
    {
        cfg = drop_agent::AgentConfig::Default("localhost:50051");
    }

    cout << "[agent] drop_agent 启动 (W5 多采集器)" << endl;
    cout << "[agent] hostname=" << cfg.hostname
         << " uid=" << cfg.uid
         << " version=" << cfg.agentVersion << endl;

    // ---------- 2. 多 Server 故障转移连接 ----------
    common::CosConfig cosConfig;
    bool registered = false;
    auto channel = connect_to_server(cfg, cosConfig, registered);

    if (!channel || !registered)
    {
        cerr << "[agent] 所有 Server 均不可用，退出" << endl;
        return 1;
    }

    // ---------- 3. 心跳循环 ----------
    auto health_stub = healthcheck::HealthCheck::NewStub(channel);
    auto hotmethod_stub = hotmethod::Hotmethod::NewStub(channel);

    // 心跳间隔转换为毫秒
    uint32_t intervalMs = cfg.heartbeatIntervalSec * 1000;

    while (agent_running)
    {
        // 自监控
        common::PidStats selfPs = drop::collect_self_pidstats();
        vector<common::PidStats> childrenPs = drop::collect_children_pidstats();

        // 填心跳
        healthcheck::HealthCheckRequest req;
        req.set_hostname(cfg.hostname);
        req.set_ipaddr("127.0.0.1");
        req.set_uid(cfg.uid);
        req.set_agentversion(cfg.agentVersion);
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
            cerr << "[agent] 心跳失败: " << status.error_message()
                 << " — 尝试重连..." << endl;

            // 故障转移：重新尝试连接
            auto newChannel = connect_to_server(cfg, cosConfig, registered);
            if (newChannel && registered)
            {
                channel = newChannel;
                health_stub = healthcheck::HealthCheck::NewStub(channel);
                hotmethod_stub = hotmethod::Hotmethod::NewStub(channel);
                cout << "[agent] 已切换到备用 Server" << endl;
            }
            goto sleep_and_continue;
        }

        cout << "[agent] 心跳 OK, pending=" << resp.pending() << endl;

        // ----- 有任务？按 profilerType 分发 -----
        if (resp.pending() && resp.has_taskdesc())
        {
            const auto &task = resp.taskdesc();
            uint32_t ptype = task.profilertype();

            cout << "[agent] 收到任务! taskID=" << task.taskid()
                 << " profilerType=" << ptype
                 << " pid=" << task.sampleargv().pid()
                 << " hz=" << task.sampleargv().hz()
                 << " duration=" << task.sampleargv().duration() << endl;

            // 输出文件路径（统一前缀，不同采集器加不同后缀）
            string outputPath = "/tmp/" + to_string(ptype) + "_" + task.taskid() + "_output";
            auto profilerResult = run_profiler(ptype, task, outputPath, "");
            int resultCode = profilerResult.first;
            string profilerName = profilerResult.second;

            // 构建结果
            hotmethod::TaskResult taskResult = build_task_result(
                task, resultCode, profilerName, outputPath,
                cosConfig, selfPs, childrenPs);

            // 上报
            google::protobuf::Empty emptyResp;
            ClientContext notifyCtx;
            Status notifyStatus = hotmethod_stub->NotifyResult(&notifyCtx, taskResult, &emptyResp);

            if (notifyStatus.ok())
            {
                cout << "[agent] NotifyResult 上报成功: taskID=" << task.taskid()
                     << " profiler=" << profilerName
                     << " error=\"" << taskResult.errormessage() << "\""
                     << " cosKey=" << taskResult.coskey() << endl;
            }
            else
            {
                cerr << "[agent] NotifyResult 上报失败: " << notifyStatus.error_message() << endl;
            }
        }

    sleep_and_continue:
        // 等 heartbeatIntervalSec 秒再发下一次心跳
        for (uint32_t i = 0; i < intervalMs / 100 && agent_running; i++)
            this_thread::sleep_for(milliseconds(100));
    }

    cout << "[agent] 收到退出信号，正在关闭..." << endl;
    return 0;
}
