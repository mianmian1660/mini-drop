#include "agent/WorkerThread.h"

#include "agent/Runner.h"
#include "agent/RunnerRegistry.h"
#include "agent/RunnerUtils.h"
#include "agent/AgentUtils.h"
#include "common/COSClient.h"      // drop::upload_to_minio
#include "common/Utils.h"         // drop::read_file_content
#include "common/SymbolCollector.h" // drop::collect_and_upload_symbols
#include "common/Log.h"           // drop::log_event
#include "common/Process.h"       // drop::collect_self_pidstats, collect_children_pidstats

#include <iostream>
#include <string>
#include <chrono>
#include <vector>
#include <fstream>
#include <sstream>
#include <algorithm>
#include <cctype>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <sys/stat.h>
#include <sys/utsname.h>
#include <unistd.h>

#include <grpcpp/grpcpp.h>
#include "common/proto/hotmethod.grpc.pb.h"

using grpc::ClientContext;
using grpc::Status;
using namespace std;
using namespace std::chrono;

namespace drop_agent
{

    namespace
    {

        int64_t FileSize(const string &path)
        {
            struct stat st;
            if (stat(path.c_str(), &st) != 0)
                return 0;
            return static_cast<int64_t>(st.st_size);
        }

        string Sha256File(const string &path)
        {
            string output;
            int ret = drop::exec_capture({"sha256sum", path}, &output, 512);
            if (ret != 0 || output.empty())
                return "";
            size_t space = output.find(' ');
            return space == string::npos ? output : output.substr(0, space);
        }

        string UrlEncode(const string &s)
        {
            static const char *hex = "0123456789ABCDEF";
            string out;
            for (unsigned char c : s)
            {
                if ((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
                    (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~')
                {
                    out += static_cast<char>(c);
                }
                else
                {
                    out += '%';
                    out += hex[c >> 4];
                    out += hex[c & 15];
                }
            }
            return out;
        }

        string KernelRelease()
        {
            struct utsname u {};
            if (uname(&u) == 0)
                return string(u.release);
            return "";
        }

        string AgentHostname()
        {
            const char *envHost = getenv("DROP_AGENT_HOSTNAME");
            if (envHost && *envHost)
                return string(envHost);
            char buf[256] = {0};
            if (gethostname(buf, sizeof(buf) - 1) == 0)
                return string(buf);
            return "";
        }

        string AgentIP()
        {
            const char *envIP = getenv("DROP_AGENT_IP");
            if (envIP && *envIP)
                return string(envIP);
            return "";
        }

        string ApiserverBaseURL()
        {
            const char *symbolBaseEnv = getenv("APISERVER_SYMBOL_BASE_URL");
            if (symbolBaseEnv && *symbolBaseEnv)
                return string(symbolBaseEnv);
            const char *nativeBaseEnv = getenv("DROP_NATIVE_CP_API_BASE_URL");
            if (nativeBaseEnv && *nativeBaseEnv)
                return string(nativeBaseEnv);
            return "http://127.0.0.1:8191";
        }

        // ============================================================
        // snapshot_kallsyms — 快照 /proc/kallsyms 供 analysis 解析内核符号
        // ============================================================
        // 为什么必须由 Agent 做：读到真实内核地址需要 CAP_SYSLOG，光有
        // kptr_restrict=0 不够。analysis 不是特权容器，它本地的 /proc/kallsyms
        // 地址全是 0，内核帧只会显示 [unknown]。Agent 是特权容器，读得到真值。
        //
        // 全零校验是必须的：一张地址全 0 的 kallsyms 在格式上完全合法，传上去
        // 只会让下游拿到一份无效甚至误导性的符号表。宁可不传，也不传有毒的表。
        //
        // 返回 true 表示快照可用并已写入 outPath。
        bool SnapshotKallsyms(const string &outPath)
        {
            std::ifstream in("/proc/kallsyms");
            if (!in.is_open())
            {
                cout << "[agent] 无法读取 /proc/kallsyms，跳过内核符号快照" << endl;
                return false;
            }

            string content;
            string line;
            bool sawNonZeroAddr = false;
            size_t lineNo = 0;
            while (std::getline(in, line))
            {
                // 只需在前若干行里确认地址不是全 0，无需扫全文（文件常有 5~10MB）
                if (!sawNonZeroAddr && lineNo < 64)
                {
                    size_t space = line.find(' ');
                    if (space != string::npos &&
                        line.substr(0, space).find_first_not_of('0') != string::npos)
                    {
                        sawNonZeroAddr = true;
                    }
                }
                ++lineNo;
                content += line;
                content += '\n';
            }
            in.close();

            if (content.empty())
            {
                cout << "[agent] /proc/kallsyms 为空，跳过内核符号快照" << endl;
                return false;
            }
            if (!sawNonZeroAddr)
            {
                cout << "[agent] 警告: /proc/kallsyms 地址全为 0（缺少 CAP_SYSLOG 或 "
                     << "kptr_restrict 受限），拒绝上传无效符号表，内核符号将无法解析" << endl;
                return false;
            }

            std::ofstream out(outPath, std::ios::binary);
            if (!out.is_open())
            {
                cout << "[agent] 无法写入 kallsyms 快照: " << outPath << endl;
                return false;
            }
            out << content;
            out.close();
            return true;
        }

        bool ResponseUploadRequired(const string &response)
        {
            string compact;
            compact.reserve(response.size());
            for (char c : response)
            {
                if (!isspace(static_cast<unsigned char>(c)))
                    compact += c;
            }
            return compact.find("\"upload_required\":true") != string::npos;
        }

        bool ResponseUploadNotRequired(const string &response)
        {
            string compact;
            compact.reserve(response.size());
            for (char c : response)
            {
                if (!isspace(static_cast<unsigned char>(c)))
                    compact += c;
            }
            return compact.find("\"upload_required\":false") != string::npos;
        }

        bool CheckKernelSymbol(const string &baseURL,
                                const string &tid,
                                const string &sha256,
                                int64_t size,
                                string *response)
        {
            string reqPath = "/tmp/" + tid + "_kallsyms_check.json";
            {
                ofstream out(reqPath, ios::binary);
                if (!out.is_open())
                    return false;
                out << "{"
                    << "\"tid\":\"" << JsonEscape(tid) << "\","
                    << "\"sha256\":\"" << JsonEscape(sha256) << "\","
                    << "\"size_bytes\":" << size << ","
                    << "\"kernel_release\":\"" << JsonEscape(KernelRelease()) << "\","
                    << "\"hostname\":\"" << JsonEscape(AgentHostname()) << "\","
                    << "\"target_ip\":\"" << JsonEscape(AgentIP()) << "\""
                    << "}";
            }
            int rc = drop::exec_capture({"curl", "-sS", "-m", "10", "-X", "POST",
                                         "-H", "Content-Type: application/json",
                                         "-d", "@" + reqPath,
                                         baseURL + "/api/v1/kernel-symbols/check"},
                                        response, 4096);
            ::remove(reqPath.c_str());
            return rc == 0;
        }

        bool PutKernelSymbol(const string &baseURL,
                              const string &tid,
                              const string &sha256,
                              const string &path,
                              string *response)
        {
            string url = baseURL + "/api/v1/kernel-symbols/" + sha256 +
                         "?tid=" + UrlEncode(tid) +
                         "&kernel_release=" + UrlEncode(KernelRelease()) +
                         "&hostname=" + UrlEncode(AgentHostname()) +
                         "&target_ip=" + UrlEncode(AgentIP());
            int rc = drop::exec_capture({"curl", "-sS", "-m", "60", "-X", "PUT",
                                         "--data-binary", "@" + path, url},
                                        response, 4096);
            return rc == 0;
        }

        bool EnsureKernelSymbolUploaded(const string &baseURL,
                                         const string &tid,
                                         const string &kallsymsPath)
        {
            string sum = Sha256File(kallsymsPath);
            if (sum.empty())
            {
                cout << "[agent] kallsyms sha256 计算失败，跳过去重上传" << endl;
                return false;
            }

            string checkResp;
            if (!CheckKernelSymbol(baseURL, tid, sum, FileSize(kallsymsPath), &checkResp))
            {
                cout << "[agent] kernel-symbols/check 调用失败，内核符号将降级" << endl;
                return false;
            }
            if (ResponseUploadNotRequired(checkResp))
            {
                cout << "[agent] 服务端已有 kallsyms sha256=" << sum << "，复用共享对象" << endl;
                return true;
            }
            if (!ResponseUploadRequired(checkResp))
            {
                cout << "[agent] kernel-symbols/check 响应不可识别: " << checkResp << endl;
                return false;
            }

            string putResp;
            if (!PutKernelSymbol(baseURL, tid, sum, kallsymsPath, &putResp))
            {
                cout << "[agent] kernel-symbols 上传失败，内核符号将降级" << endl;
                return false;
            }
            cout << "[agent] kallsyms 去重上传成功 sha256=" << sum << endl;
            return true;
        }

        bool WriteManifestFile(const hotmethod::TaskDesc &task,
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
            out << "\"task_id\":\"" << JsonEscape(task.taskid()) << "\",";
            out << "\"attempt_id\":" << task.attempt_id() << ",";
            out << "\"task_kind\":\"" << JsonEscape(task.task_kind()) << "\",";
            out << "\"collector\":\"" << JsonEscape(collector) << "\",";
            out << "\"object_key\":\"" << JsonEscape(rawKey) << "\",";
            out << "\"raw_key\":\"" << JsonEscape(rawKey) << "\",";
            out << "\"raw_file\":\"" << JsonEscape(rawPath) << "\",";
            out << "\"local_path\":\"" << JsonEscape(rawPath) << "\",";
            out << "\"content_type\":\"" << JsonEscape(contentType) << "\",";
            out << "\"sample_event\":\"" << JsonEscape(task.sampleargv().event()) << "\",";
            out << "\"result_code\":" << resultCode << ",";
            out << "\"size\":" << FileSize(rawPath) << ",";
            out << "\"sha256\":\"" << JsonEscape(sha256) << "\",";
            out << "\"partial\":" << (partial ? "true" : "false");
            out << "}\n";
            return true;
        }

        // ============================================================
        // WSL2 Mock 数据生成：当 perf 无法在当前内核运行时，生成模拟折叠栈
        // 使火焰图→分析→Web 展示全链路在 WSL2/容器 环境中可演示
        // ============================================================
        void GenerateMockCollapsedStacks(const string &outputPath)
        {
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

        struct RunnerOutcome
        {
            int resultCode = 0;
            string profilerName;
            string outputPath;
            string remoteKey;
            string contentType;
            bool partial = false;
        };

        // ============================================================
        // run_task_lifecycle — 用 drop_agent::Runner 的
        // Validate->Prepare->Start->(Poll循环)->Collect 生命周期跑一次采集。
        // ============================================================
        RunnerOutcome RunTaskLifecycle(uint32_t profilerType, const hotmethod::TaskDesc &task,
                                        const string &outputPath)
        {
            RunnerOutcome out;
            out.outputPath = outputPath;

            cout << "[runner] stage=Validate taskID=" << task.taskid()
                 << " attemptID=" << task.attempt_id() << endl;
            if (task.taskid().empty())
            {
                out.resultCode = -1;
                out.profilerName = "unknown";
                return out;
            }
            if (task.deadline_unix_ms() > 0)
            {
                int64_t nowMs = duration_cast<milliseconds>(system_clock::now().time_since_epoch()).count();
                if (task.deadline_unix_ms() <= nowMs)
                {
                    out.resultCode = -3;
                    out.profilerName = "deadline";
                    return out;
                }
            }

            string profilerLabel;
            switch (profilerType)
            {
            case 0:
                profilerLabel = "perf";
                cout << "[agent] 选择采集器: perf (profilerType=0)" << endl;
                break;
            case 1:
                profilerLabel = "async-profiler";
                cout << "[agent] 选择采集器: async-profiler (profilerType=1)" << endl;
                break;
            case 2:
                profilerLabel = "pprof";
                cout << "[agent] 选择采集器: pprof (profilerType=2)" << endl;
                break;
            case 3:
                profilerLabel = "eBPF";
                cout << "[agent] 选择采集器: eBPF/bpftrace (profilerType=3)" << endl;
                break;
            default:
                profilerLabel = "perf(回退)";
                cerr << "[agent] 未知的 profilerType=" << profilerType << "，回退到 perf" << endl;
                break;
            }

            static drop::RealProcessExecutor s_executor;
            static drop::RealClock s_clock;
            static drop::MinioObjectStore s_objectStore;
            static drop::RealLogger s_logger;

            drop_agent::TaskContext ctx;
            ctx.task = task;
            ctx.taskDir = outputPath;
            ctx.executor = &s_executor;
            ctx.clock = &s_clock;
            ctx.objectStore = &s_objectStore;
            ctx.logger = &s_logger;

            auto runner = drop_agent::CreateRunner(profilerType);

            cout << "[runner] stage=Prepare taskID=" << task.taskid() << endl;
            int resultCode = 0;
            auto validation = runner->Validate(ctx);
            if (!validation.ok)
            {
                resultCode = validation.resultCode;
            }
            else
            {
                auto prepare = runner->Prepare(ctx);
                if (!prepare.ok)
                {
                    resultCode = prepare.resultCode;
                }
                else
                {
                    cout << "[runner] stage=Start taskID=" << task.taskid() << endl;
                    auto start = runner->Start(ctx);
                    if (!start.ok)
                    {
                        resultCode = start.resultCode;
                    }
                    else
                    {
                        while (true)
                        {
                            auto poll = runner->Poll(ctx);
                            if (poll.status != drop_agent::PollStatus::kRunning)
                            {
                                resultCode = poll.resultCode;
                                break;
                            }
                            this_thread::sleep_for(milliseconds(200));
                        }
                        cout << "[runner] stage=Monitor taskID=" << task.taskid()
                             << " resultCode=" << resultCode << endl;
                        auto collect = runner->Collect(ctx);
                        resultCode = collect.resultCode; // Collect() 可能在文件校验/后处理阶段进一步改判定(如 -6/-2)
                    }
                }
            }

            string profilerName = profilerLabel;
            if (resultCode != 0)
            {
                if (!EnvEnabled("DROP_ALLOW_EBPF_MOCK"))
                {
                    cout << "[agent] " << profilerLabel << " 采集失败(result=" << resultCode
                         << ")，默认不生成 mock。如仅本地开发要看页面链路，"
                         << "可设置 DROP_ALLOW_EBPF_MOCK=1。" << endl;
                }
                else if (profilerType == 3)
                {
                    cout << "[agent] eBPF 不可用，DROP_ALLOW_EBPF_MOCK=1，启用 mock 模式" << endl;
                    string bpfPath = outputPath + ".bpf";
                    if (task.sampleargv().event() == "io" || task.sampleargv().event() == "blk")
                    {
                        ofstream mockIO(bpfPath);
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
                        resultCode = 0;
                        profilerName = "eBPF(mock-io)";
                    }
                    else if (task.sampleargv().event() == "sched" || task.sampleargv().event() == "schedule")
                    {
                        ofstream mockSched(bpfPath);
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
                        resultCode = 0;
                        profilerName = "eBPF(mock-sched)";
                    }
                    else
                    {
                        GenerateMockCollapsedStacks(outputPath);
                        resultCode = 0;
                        profilerName = "eBPF(mock)";
                    }
                }
                else
                {
                    cout << "[agent] " << profilerLabel << " 不可用，DROP_ALLOW_EBPF_MOCK=1，启用 mock 模式" << endl;
                    GenerateMockCollapsedStacks(outputPath);
                    resultCode = 0;
                    profilerName = profilerLabel + "(mock)";
                }
            }

            out.resultCode = resultCode;
            out.profilerName = profilerName;
            out.outputPath = drop_agent::ResolveOutputPath(outputPath);
            out.partial = out.resultCode != 0 && FileExists(out.outputPath);
            cout << "[runner] stage=Collect taskID=" << task.taskid()
                 << " outputPath=" << out.outputPath
                 << " partial=" << (out.partial ? "true" : "false") << endl;
            out.remoteKey = task.taskid() + "/" + drop_agent::RemoteKeyFor(profilerName);
            out.contentType = drop_agent::ContentTypeFor(profilerName);
            return out;
        }

        // ============================================================
        // 采集后处理：读取输出文件 → 上传 MinIO → 构建 TaskResult
        // ============================================================
        hotmethod::TaskResult BuildTaskResult(
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

            string errorMsg = drop_agent::GetErrorMessage(outcome.resultCode, outcome.profilerName, task);
            if (!errorMsg.empty())
            {
                taskResult.set_errormessage(errorMsg);
                taskResult.set_error_code(drop_agent::GetErrorCode(outcome.resultCode, outcome.profilerName));
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
                    string sha256 = Sha256File(actualPath);
                    string manifestPath = "/tmp/" + task.taskid() + "_manifest.json";
                    bool manifestWritten = WriteManifestFile(task, remoteKey, actualPath, sha256, manifestPath, outcome.partial,
                                                              outcome.profilerName, outcome.contentType, outcome.resultCode);

                    cout << "[runner] stage=Upload taskID=" << task.taskid()
                         << " rawKey=" << remoteKey
                         << " manifestKey=" << manifestKey
                         << " sha256=" << sha256 << endl;
                    bool rawUploaded = drop::upload_to_minio(cosConfig, actualPath, remoteKey);
                    bool manifestUploaded = manifestWritten && drop::upload_to_minio(cosConfig, manifestPath, manifestKey);
                    drop::log_event("drop_agent", rawUploaded ? "artifact_upload_succeeded" : "artifact_upload_failed",
                                     {{"task_id", task.taskid()}, {"object_key", remoteKey}});

                    // 内核符号快照：随产物一起上传，analysis 侧靠它解析内核帧。
                    // 失败只降级不影响任务成败——用户态符号和火焰图本身仍然可用。
                    string kallsymsPath = "/tmp/" + task.taskid() + "_kallsyms";
                    if (SnapshotKallsyms(kallsymsPath))
                    {
                        if (EnsureKernelSymbolUploaded(ApiserverBaseURL(), task.taskid(), kallsymsPath))
                            cout << "[runner] stage=Upload taskID=" << task.taskid()
                                 << " size=" << FileSize(kallsymsPath) << endl;
                        else
                            cout << "[agent] kallsyms 去重上传失败，内核符号将无法解析" << endl;
                        ::remove(kallsymsPath.c_str());
                    }

                    // 用户态符号：只有 perf 采集器产出 perf.data，其余采集器
                    // (async-profiler / pprof / eBPF) 不经过 perf script，处理纯属浪费。
                    // 按 build-id 去重上传，不再整包打 tar 直传 MinIO。
                    if (task.profilertype() == 0)
                    {
                        string symbolBaseURL = ApiserverBaseURL();
                        if (!drop::collect_and_upload_symbols(actualPath, task.taskid(),
                                                               task.sampleargv().pid(), symbolBaseURL))
                            cout << "[agent] 符号采集/上传未完全成功，部分用户态符号可能无法解析" << endl;
                    }

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
                    taskResult.set_artifact_size(FileSize(actualPath));
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
                        detail += " bpftrace_raw_tail=\"" + JsonEscape(rawContent) + "\"";
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

    } // namespace

    WorkerThread::WorkerThread(TaskQueue &taskQueue,
                               AttemptTracker &attemptTracker,
                               ServerChannelHolder &channelHolder,
                               std::atomic<bool> &runningFlag)
        : taskQueue_(taskQueue),
          attemptTracker_(attemptTracker),
          channelHolder_(channelHolder),
          running_(runningFlag)
    {
    }

    void WorkerThread::Start()
    {
        thread_ = std::thread(&WorkerThread::Loop, this);
    }

    void WorkerThread::Stop()
    {
        if (thread_.joinable())
            thread_.join();
    }

    void WorkerThread::Loop()
    {
        while (running_)
        {
            hotmethod::TaskDesc task;
            if (!taskQueue_.WaitPop(200, &task))
                continue;

            uint32_t ptype = task.profilertype();
            cout << "[worker] 开始执行任务! taskID=" << task.taskid()
                 << " taskKind=" << task.task_kind()
                 << " requestID=" << task.request_id()
                 << " attemptID=" << task.attempt_id()
                 << " deadlineUnixMs=" << task.deadline_unix_ms()
                 << " profilerType=" << ptype
                 << " pid=" << task.sampleargv().pid()
                 << " hz=" << task.sampleargv().hz()
                 << " duration=" << task.sampleargv().duration() << endl;

            string outputPath = "/tmp/" + to_string(ptype) + "_" + task.taskid() + "_output";

            auto collectStart = steady_clock::now();
            RunnerOutcome outcome = RunTaskLifecycle(ptype, task, outputPath);
            auto collectMs = duration_cast<milliseconds>(steady_clock::now() - collectStart).count();
            drop::log_event("drop_agent",
                             outcome.resultCode == 0 ? "collection_succeeded" : "collection_failed",
                             {{"task_id", task.taskid()},
                              {"profiler", outcome.profilerName},
                              {"result_code", to_string(outcome.resultCode)},
                              {"duration_ms", to_string(collectMs)}});

            common::PidStats selfPs = drop::collect_self_pidstats();
            vector<common::PidStats> childrenPs = drop::collect_children_pidstats();

            hotmethod::TaskResult taskResult = BuildTaskResult(
                task, outcome, channelHolder_.GetCosConfig(), selfPs, childrenPs);

            auto hotmethodStub = channelHolder_.HotmethodStub();
            google::protobuf::Empty emptyResp;
            ClientContext notifyCtx;
            Status notifyStatus = hotmethodStub->NotifyResult(&notifyCtx, taskResult, &emptyResp);

            if (notifyStatus.ok())
            {
                cout << "[worker] NotifyResult 上报成功: taskID=" << task.taskid()
                     << " profiler=" << outcome.profilerName
                     << " error=\"" << taskResult.errormessage() << "\""
                     << " cosKey=" << taskResult.coskey() << endl;
                drop::log_event("drop_agent", "notify_result_succeeded",
                                 {{"task_id", task.taskid()}, {"cos_key", taskResult.coskey()}});
            }
            else
            {
                cerr << "[worker] NotifyResult 上报失败: " << notifyStatus.error_message() << endl;
                drop::log_event("drop_agent", "notify_result_failed",
                                 {{"task_id", task.taskid()}, {"error", notifyStatus.error_message()}});
            }

            attemptTracker_.MarkCompleted(task.taskid(), task.attempt_id());
        }
    }

} // namespace drop_agent
