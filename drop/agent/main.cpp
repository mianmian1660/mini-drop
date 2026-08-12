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
#include "common/BpfProfiler.h"           // drop::run_bpf (profilerType=3, eBPF)
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
#include <sstream>
#include <algorithm>
#include <cctype>
#include <cstdlib>
#include <cstring>
#include <utility>
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

static int64_t file_size(const string &path)
{
    struct stat st;
    if (stat(path.c_str(), &st) != 0)
        return 0;
    return static_cast<int64_t>(st.st_size);
}

static string json_escape_local(const string &s)
{
    string out;
    out.reserve(s.size());
    for (char c : s)
    {
        if (c == '"' || c == '\\')
            out += '\\';
        if (c == '\n')
        {
            out += "\\n";
            continue;
        }
        out += c;
    }
    return out;
}

static string sha256_file(const string &path)
{
    string output;
    int ret = drop::exec_capture({"sha256sum", path}, &output, 512);
    if (ret != 0 || output.empty())
        return "";
    size_t space = output.find(' ');
    return space == string::npos ? output : output.substr(0, space);
}

static bool write_manifest_file(const hotmethod::TaskDesc &task,
                                const string &rawKey,
                                const string &rawPath,
                                const string &sha256,
                                const string &manifestPath,
                                bool partial,
                                const string &collector,
                                const string &contentType,
                                int resultCode)
{
    ofstream out(manifestPath, ios::trunc);
    if (!out.is_open())
        return false;
    out << "{";
    out << "\"task_id\":\"" << json_escape_local(task.taskid()) << "\",";
    out << "\"attempt_id\":" << task.attempt_id() << ",";
    out << "\"task_kind\":\"" << json_escape_local(task.task_kind()) << "\",";
    out << "\"collector\":\"" << json_escape_local(collector) << "\",";
    out << "\"object_key\":\"" << json_escape_local(rawKey) << "\",";
    out << "\"raw_key\":\"" << json_escape_local(rawKey) << "\",";
    out << "\"raw_file\":\"" << json_escape_local(rawPath) << "\",";
    out << "\"local_path\":\"" << json_escape_local(rawPath) << "\",";
    out << "\"content_type\":\"" << json_escape_local(contentType) << "\",";
    out << "\"sample_event\":\"" << json_escape_local(task.sampleargv().event()) << "\",";
    out << "\"result_code\":" << resultCode << ",";
    out << "\"size\":" << file_size(rawPath) << ",";
    out << "\"sha256\":\"" << json_escape_local(sha256) << "\",";
    out << "\"partial\":" << (partial ? "true" : "false");
    out << "}\n";
    return true;
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

static bool env_enabled(const char *name)
{
    const char *v = getenv(name);
    if (!v)
        return false;
    string s(v);
    transform(s.begin(), s.end(), s.begin(), [](unsigned char c)
              { return static_cast<char>(tolower(c)); });
    return s == "1" || s == "true" || s == "yes" || s == "on";
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

        if (result != 0)
        {
            // A3: 不再无条件 mock。默认显式返回失败，和 eBPF 走同一套门控，
            // 避免演示/评审现场把"采集失败"悄悄伪装成"采集成功"。
            if (!env_enabled("DROP_ALLOW_EBPF_MOCK"))
            {
                cout << "[agent] perf 采集失败(result=" << result
                     << ")，默认不生成 mock。如仅本地开发要看页面链路，"
                     << "可设置 DROP_ALLOW_EBPF_MOCK=1。" << endl;
                return {result, "perf"};
            }
            cout << "[agent] perf 失败(result=" << result
                 << ")，DROP_ALLOW_EBPF_MOCK=1，启用 mock 模式" << endl;
            generate_mock_collapsed_stacks(path);
            return {0, "perf(mock)"};
        }
        return {result, "perf"};
    }
    case 1: // async-profiler (Java)
    {
        path = outputPath + ".collapsed";
        cout << "[agent] 选择采集器: async-profiler (profilerType=1)" << endl;
        int result = drop::run_async_profiler(task, path);
        if (result != 0)
        {
            if (!env_enabled("DROP_ALLOW_EBPF_MOCK"))
            {
                cout << "[agent] async-profiler 采集失败(result=" << result
                     << ")，默认不生成 mock。如仅本地开发要看页面链路，"
                     << "可设置 DROP_ALLOW_EBPF_MOCK=1。" << endl;
                return {result, "async-profiler"};
            }
            cout << "[agent] async-profiler 不可用，DROP_ALLOW_EBPF_MOCK=1，启用 mock 模式" << endl;
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
            if (!env_enabled("DROP_ALLOW_EBPF_MOCK"))
            {
                cout << "[agent] pprof 采集失败(result=" << result
                     << ")，默认不生成 mock。如仅本地开发要看页面链路，"
                     << "可设置 DROP_ALLOW_EBPF_MOCK=1。" << endl;
                return {result, "pprof"};
            }
            cout << "[agent] pprof 不可用，DROP_ALLOW_EBPF_MOCK=1，启用 mock 模式" << endl;
            generate_mock_collapsed_stacks(outputPath);
            return {0, "pprof(mock)"};
        }
        return {result, "pprof"};
    }
    case 3: // eBPF (bpftrace)
    {
        path = outputPath + ".bpf";
        cout << "[agent] 选择采集器: eBPF/bpftrace (profilerType=3)" << endl;
        int result = drop::run_bpf(task, path);
        if (result != 0)
        {
            if (!env_enabled("DROP_ALLOW_EBPF_MOCK"))
            {
                cout << "[agent] eBPF 采集失败(result=" << result
                     << ")，默认不生成 mock。评分演示要求 eBPF 必须真跑；"
                     << "如仅本地开发要看页面链路，可设置 DROP_ALLOW_EBPF_MOCK=1。" << endl;
                return {result, "eBPF"};
            }

            cout << "[agent] eBPF 不可用，DROP_ALLOW_EBPF_MOCK=1，启用 mock 模式" << endl;
            // eBPF IO 模式生成模拟 IO 直方图
            if (task.sampleargv().event() == "io" || task.sampleargv().event() == "blk")
            {
                ofstream mockIO(path);
                mockIO << "# Mini-Drop eBPF IO Latency (MOCK)\n";
                mockIO << "@io_lat_us:\n";
                mockIO << "[1, 2)        42 |@@@@@@@@@\n";
                mockIO << "[2, 4)        88 |@@@@@@@@@@@@@@@@@@@\n";
                mockIO << "[4, 8)       156 |@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@\n";
                mockIO << "[8, 16)      230 |@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@\n";
                mockIO << "[16, 32)      89 |@@@@@@@@@@@@@@@@@@@@\n";
                mockIO << "[32, 64)      45 |@@@@@@@@@@\n";
                mockIO << "[64, 128)     12 |@@\n";
                mockIO << "[128, 256)     3 |@\n";
                mockIO << "# Total IO: 665\n";
                mockIO.close();
                return {0, "eBPF(mock-io)"};
            }
            if (task.sampleargv().event() == "sched" || task.sampleargv().event() == "schedule")
            {
                ofstream mockSched(path);
                mockSched << "# Mini-Drop eBPF Scheduler Latency (MOCK)\n";
                mockSched << "@sched_lat_us:\n";
                mockSched << "[0, 10)      520 |@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@\n";
                mockSched << "[10, 50)     210 |@@@@@@@@@@@@@@@@@@@@\n";
                mockSched << "[50, 100)     68 |@@@@@@\n";
                mockSched << "[100, 250)    24 |@@\n";
                mockSched << "[250, 500)     7 |@\n";
                mockSched << "[500, 1K)      2 |\n";
                mockSched << "# Total Scheduler Wakeups: 831\n";
                mockSched.close();
                return {0, "eBPF(mock-sched)"};
            }
            generate_mock_collapsed_stacks(outputPath);
            return {0, "eBPF(mock)"};
        }
        return {result, "eBPF"};
    }
    default:
        cerr << "[agent] 未知的 profilerType=" << profilerType << "，回退到 perf" << endl;
        int result = drop::run_perf(task, path);
        if (result != 0)
        {
            if (!env_enabled("DROP_ALLOW_EBPF_MOCK"))
            {
                return {result, "perf(回退)"};
            }
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
        if (profilerName == "pprof")
            return "无法连接 Go pprof：请确认 pprof_url 可从 Agent 访问并已启用 /debug/pprof/profile";
        return "目标 PID " + to_string(task.sampleargv().pid()) + " 不存在";
    case -3:
        return profilerName + " 采集超时（" + to_string(task.timeoutsec()) + "秒）";
    case -6:
        if (profilerName == "pprof")
            return "pprof 返回的不是有效 gzip profile；请确认 /debug/pprof/profile 已启用且未被代理改写";
        return profilerName + " 未生成有效采集文件";
    case -1:
    case -2:
    case -5:
        if (profilerName == "eBPF")
        {
            string event = task.sampleargv().event();
            if (event.empty())
                event = "cpu";
            if (resultCode == -5)
                return "eBPF 采集失败：BPFTRACE_UNAVAILABLE，请确认 Agent 容器内已安装 bpftrace，resultCode=" + to_string(resultCode);
            if (event == "cpu")
                return "eBPF CPU 采集失败：NO_EBPF_SAMPLES，请提高采样频率/加长 duration，或确认 kstack/ustack 权限可用，resultCode=" + to_string(resultCode);
            return "eBPF 采集失败：NO_EBPF_SAMPLES，请确认 bpftrace/tracefs 权限可用，并在采集窗口内制造 " + event + " 负载，resultCode=" + to_string(resultCode);
        }
        return profilerName + " 进程异常, resultCode=" + to_string(resultCode);
    default:
        return profilerName + " 采集失败, exitCode=" + to_string(resultCode);
    }
}

static string get_error_code(int resultCode, const string &profilerName)
{
    if (resultCode == 0)
        return "";
    if (resultCode == -3)
        return "TASK_TIMEOUT";
    if (resultCode == -4)
        return "TARGET_NOT_FOUND";
    if (resultCode == -6)
        return "ARTIFACT_MISSING";
    if (profilerName.find("eBPF") != string::npos)
    {
        if (resultCode == -2)
            return "NO_EBPF_SAMPLES";
        if (resultCode == -5)
            return "BPFTRACE_UNAVAILABLE";
        return "EBPF_UNAVAILABLE";
    }
    if (profilerName.find("pprof") != string::npos)
        return "PPROF_UNAVAILABLE";
    return "TASK_EXECUTION_FAILED";
}

struct RunnerOutcome
{
    int resultCode = 0;
    string profilerName;
    string outputPath;
    string remoteKey;
    string contentType;
    bool partial = false;
};

class Runner
{
public:
    Runner(uint32_t profilerType, const hotmethod::TaskDesc &task, string outputPath)
        : profilerType_(profilerType), task_(task), outputPath_(std::move(outputPath)) {}

    RunnerOutcome Run()
    {
        cout << "[runner] stage=Validate taskID=" << task_.taskid()
             << " attemptID=" << task_.attempt_id() << endl;
        RunnerOutcome out;
        out.outputPath = outputPath_;
        if (task_.taskid().empty())
        {
            out.resultCode = -1;
            out.profilerName = "unknown";
            return out;
        }
        if (task_.deadline_unix_ms() > 0)
        {
            int64_t nowMs = duration_cast<milliseconds>(system_clock::now().time_since_epoch()).count();
            if (task_.deadline_unix_ms() <= nowMs)
            {
                out.resultCode = -3;
                out.profilerName = "deadline";
                return out;
            }
        }

        cout << "[runner] stage=Prepare taskID=" << task_.taskid() << endl;
        cout << "[runner] stage=Start taskID=" << task_.taskid() << endl;
        auto profilerResult = run_profiler(profilerType_, task_, outputPath_, "");
        out.resultCode = profilerResult.first;
        out.profilerName = profilerResult.second;
        cout << "[runner] stage=Monitor taskID=" << task_.taskid()
             << " resultCode=" << out.resultCode << endl;
        out.outputPath = ResolveOutputPath();
        out.partial = out.resultCode != 0 && file_exists(out.outputPath);
        cout << "[runner] stage=Collect taskID=" << task_.taskid()
             << " outputPath=" << out.outputPath
             << " partial=" << (out.partial ? "true" : "false") << endl;
        out.remoteKey = RemoteKeyFor(out.outputPath, out.profilerName);
        out.contentType = ContentTypeFor(out.outputPath, out.profilerName);
        return out;
    }

private:
    string ResolveOutputPath() const
    {
        if (file_exists(outputPath_))
            return outputPath_;
        if (file_exists(outputPath_ + ".collapsed"))
            return outputPath_ + ".collapsed";
        if (file_exists(outputPath_ + ".pb.gz"))
            return outputPath_ + ".pb.gz";
        if (file_exists(outputPath_ + ".bpf"))
            return outputPath_ + ".bpf";
        if (file_exists(outputPath_ + ".bpf.raw"))
            return outputPath_ + ".bpf.raw";
        return outputPath_;
    }

    string RemoteKeyFor(const string &path, const string &profilerName) const
    {
        string tid = task_.taskid();
        if (profilerName.find("eBPF") != string::npos || path.find(".bpf") != string::npos)
            return tid + "/raw.bpf";
        if (profilerName.find("async-profiler") != string::npos || path.find(".collapsed") != string::npos)
            return tid + "/profile.collapsed";
        if (profilerName.find("pprof") != string::npos || path.find(".pb.gz") != string::npos)
            return tid + "/profile.pb.gz";
        return tid + "/perf.data";
    }

    string ContentTypeFor(const string &path, const string &profilerName) const
    {
        if (profilerName.find("eBPF") != string::npos || path.find(".bpf") != string::npos || path.find(".collapsed") != string::npos)
            return "text/plain; charset=utf-8";
        if (profilerName.find("pprof") != string::npos || path.find(".pb.gz") != string::npos)
            return "application/gzip";
        return "application/octet-stream";
    }

    uint32_t profilerType_;
    const hotmethod::TaskDesc &task_;
    string outputPath_;
};

// ============================================================
// 采集后处理：读取输出文件 → 上传 MinIO → 构建 TaskResult
// ============================================================
static hotmethod::TaskResult build_task_result(
    const hotmethod::TaskDesc &task,
    const RunnerOutcome &outcome,
    const common::CosConfig &cosConfig,
    const common::PidStats &selfPs,
    const vector<common::PidStats> &childrenPs)
{
    hotmethod::TaskResult taskResult;
    taskResult.set_taskid(task.taskid());
    taskResult.set_attempt_id(task.attempt_id());
    taskResult.set_partial(outcome.partial);

    string errorMsg = get_error_message(outcome.resultCode, outcome.profilerName, task);
    if (!errorMsg.empty())
    {
        taskResult.set_errormessage(errorMsg);
        taskResult.set_error_code(get_error_code(outcome.resultCode, outcome.profilerName));
    }

    if (outcome.resultCode == 0 || outcome.partial)
    {
        string actualPath = outcome.outputPath;
        string fileContent = drop::read_file_content(actualPath);
        if (!fileContent.empty())
        {
            string fileName = actualPath;
            size_t slashPos = fileName.rfind('/');
            if (slashPos != string::npos)
                fileName = fileName.substr(slashPos + 1);

            string remoteKey = outcome.remoteKey.empty() ? task.taskid() + "/perf.data" : outcome.remoteKey;
            string manifestKey = task.taskid() + "/manifest.json";
            string sha256 = sha256_file(actualPath);
            string manifestPath = "/tmp/" + task.taskid() + "_manifest.json";
            bool manifestWritten = write_manifest_file(task, remoteKey, actualPath, sha256, manifestPath, outcome.partial,
                                                       outcome.profilerName, outcome.contentType, outcome.resultCode);

            cout << "[runner] stage=Upload taskID=" << task.taskid()
                 << " rawKey=" << remoteKey
                 << " manifestKey=" << manifestKey
                 << " sha256=" << sha256 << endl;
            bool rawUploaded = drop::upload_to_minio(cosConfig, actualPath, remoteKey);
            bool manifestUploaded = manifestWritten && drop::upload_to_minio(cosConfig, manifestPath, manifestKey);
            if (rawUploaded)
                taskResult.set_coskey(remoteKey);
            if (manifestUploaded)
                taskResult.set_manifest_key(manifestKey);
            if (!rawUploaded)
            {
                taskResult.set_errormessage("产物上传失败，Agent 将保留本地文件等待人工/后续补偿: " + actualPath);
                taskResult.set_error_code("ARTIFACT_UPLOAD_FAILED");
                taskResult.set_partial(true);
            }
            taskResult.set_artifact_size(file_size(actualPath));
            taskResult.set_artifact_sha256(sha256);

            auto *file = taskResult.mutable_file();
            file->set_name(fileName);
            file->set_content(fileContent);
            file->set_size(fileContent.size());
            cout << "[agent] 采集成功，采集器=" << outcome.profilerName
                 << " 文件=" << fileName
                 << " 大小=" << fileContent.size() << " bytes" << endl;
        }
        else
        {
            string detail;
            struct stat st;
            if (stat(actualPath.c_str(), &st) != 0)
                detail = " (stat: " + string(strerror(errno)) + ")";
            else
                detail = " (size=" + to_string(static_cast<long long>(st.st_size)) + ")";
            string rawPath = actualPath + ".raw";
            string rawContent = drop::read_file_content(rawPath);
            if (!rawContent.empty())
            {
                if (rawContent.size() > 600)
                    rawContent = rawContent.substr(rawContent.size() - 600);
                detail += " bpftrace_raw_tail=\"" + json_escape_local(rawContent) + "\"";
            }
            taskResult.set_errormessage(outcome.profilerName + " 采集完成但无法读取输出文件: " + actualPath + detail);
            taskResult.set_error_code("ARTIFACT_MISSING");
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
         << " ip=" << cfg.ipAddr
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
    vector<common::AttemptStatus> runningAttempts;
    vector<common::AttemptStatus> completedAttempts;

    while (agent_running)
    {
        // 自监控
        common::PidStats selfPs = drop::collect_self_pidstats();
        vector<common::PidStats> childrenPs = drop::collect_children_pidstats();

        // 填心跳
        healthcheck::HealthCheckRequest req;
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
        *req.mutable_selfpstats() = selfPs;
        if (!childrenPs.empty())
            *req.mutable_childrenpstats() = childrenPs[0];
        for (const auto &attempt : runningAttempts)
            *req.add_running_attempts() = attempt;
        for (const auto &attempt : completedAttempts)
            *req.add_completed_attempts() = attempt;

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
                 << " taskKind=" << task.task_kind()
                 << " requestID=" << task.request_id()
                 << " attemptID=" << task.attempt_id()
                 << " deadlineUnixMs=" << task.deadline_unix_ms()
                 << " profilerType=" << ptype
                 << " pid=" << task.sampleargv().pid()
                 << " hz=" << task.sampleargv().hz()
                 << " duration=" << task.sampleargv().duration() << endl;

            // 输出文件路径（统一前缀，不同采集器加不同后缀）
            string outputPath = "/tmp/" + to_string(ptype) + "_" + task.taskid() + "_output";
            common::AttemptStatus currentAttempt;
            currentAttempt.set_taskid(task.taskid());
            currentAttempt.set_attempt_id(task.attempt_id());
            runningAttempts.push_back(currentAttempt);

            Runner runner(ptype, task, outputPath);
            RunnerOutcome outcome = runner.Run();

            // 构建结果
            hotmethod::TaskResult taskResult = build_task_result(
                task, outcome,
                cosConfig, selfPs, childrenPs);

            // 上报
            google::protobuf::Empty emptyResp;
            ClientContext notifyCtx;
            Status notifyStatus = hotmethod_stub->NotifyResult(&notifyCtx, taskResult, &emptyResp);

            if (notifyStatus.ok())
            {
                cout << "[agent] NotifyResult 上报成功: taskID=" << task.taskid()
                     << " profiler=" << outcome.profilerName
                     << " error=\"" << taskResult.errormessage() << "\""
                     << " cosKey=" << taskResult.coskey() << endl;
            }
            else
            {
                cerr << "[agent] NotifyResult 上报失败: " << notifyStatus.error_message() << endl;
            }

            runningAttempts.erase(remove_if(runningAttempts.begin(), runningAttempts.end(),
                                            [&](const common::AttemptStatus &attempt)
                                            {
                                                return attempt.taskid() == task.taskid() &&
                                                       attempt.attempt_id() == task.attempt_id();
                                            }),
                                  runningAttempts.end());
            completedAttempts.push_back(currentAttempt);
            if (completedAttempts.size() > 50)
                completedAttempts.erase(completedAttempts.begin());
        }

    sleep_and_continue:
        // 等 heartbeatIntervalSec 秒再发下一次心跳
        for (uint32_t i = 0; i < intervalMs / 100 && agent_running; i++)
            this_thread::sleep_for(milliseconds(100));
    }

    cout << "[agent] 收到退出信号，正在关闭..." << endl;
    return 0;
}
