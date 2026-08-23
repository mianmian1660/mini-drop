#include "agent/UploadWorker.h"

#include "agent/RunnerUtils.h"
#include "agent/AgentUtils.h"
#include "agent/ResultDelivery.h"
#include "common/COSClient.h"      // drop::upload_to_minio
#include "common/Utils.h"          // drop::read_file_content
#include "common/SymbolCollector.h" // drop::collect_and_upload_symbols
#include "common/Log.h"            // drop::log_event

#include <iostream>
#include <string>
#include <fstream>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <cerrno>
#include <sys/stat.h>
#include <sys/utsname.h>
#include <unistd.h>

#include <grpcpp/grpcpp.h>
#include "common/proto/hotmethod.grpc.pb.h"

using grpc::ClientContext;
using grpc::Status;
using namespace std;

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

                    string remoteKey = outcome.remoteKey.empty()
                        ? drop_agent::RawObjectKey(task.taskid(), task.attempt_id(), "perf.data")
                        : outcome.remoteKey;
                    string manifestKey = drop_agent::ManifestObjectKey(task.taskid(), task.attempt_id());
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

    UploadWorker::UploadWorker(UploadQueue &uploadQueue,
                                AttemptTracker &attemptTracker,
                                ServerChannelHolder &channelHolder)
        : uploadQueue_(uploadQueue),
          attemptTracker_(attemptTracker),
          channelHolder_(channelHolder)
    {
    }

    void UploadWorker::Start()
    {
        thread_ = std::thread(&UploadWorker::Loop, this);
    }

    void UploadWorker::Stop()
    {
        uploadQueue_.Shutdown();
        draining_ = true;
        if (thread_.joinable())
            thread_.join();
    }

    void UploadWorker::Loop()
    {
        while (true)
        {
            UploadJob job;
            if (uploadQueue_.WaitPop(200, &job))
            {
                hotmethod::TaskResult taskResult = BuildTaskResult(
                    job.task, job.outcome, channelHolder_.GetCosConfig(), job.selfPs, job.childrenPs);

                std::string notifyError;
                bool delivered = DeliverResultAndMarkCompleted(
                    job.task.taskid(), job.task.attempt_id(), attemptTracker_,
                    [&](std::string *error)
                    {
                        // Fetch a fresh stub on every attempt so heartbeat failover
                        // can repair an in-flight result notification.
                        auto hotmethodStub = channelHolder_.HotmethodStub();
                        if (!hotmethodStub)
                        {
                            if (error)
                                *error = "server channel is unavailable";
                            return false;
                        }
                        google::protobuf::Empty emptyResp;
                        ClientContext notifyCtx;
                        notifyCtx.set_deadline(std::chrono::system_clock::now() + std::chrono::seconds(5));
                        Status status = hotmethodStub->NotifyResult(&notifyCtx, taskResult, &emptyResp);
                        if (!status.ok() && error)
                            *error = status.error_message();
                        return status.ok();
                    },
                    ResultDeliveryPolicy{}, RetrySleep{}, &notifyError);

                if (delivered)
                {
                    cout << "[upload] NotifyResult 上报成功: taskID=" << job.task.taskid()
                         << " profiler=" << job.outcome.profilerName
                         << " error=\"" << taskResult.errormessage() << "\""
                         << " cosKey=" << taskResult.coskey() << endl;
                    drop::log_event("drop_agent", "notify_result_succeeded",
                                     {{"task_id", job.task.taskid()}, {"cos_key", taskResult.coskey()}});
                }
                else
                {
                    cerr << "[upload] NotifyResult 重试耗尽，保留本地产物且不确认 completed: "
                         << notifyError << endl;
                    drop::log_event("drop_agent", "notify_result_failed",
                                     {{"task_id", job.task.taskid()}, {"error", notifyError}});
                }
                continue;
            }

            // 队列已空：只有在 Stop() 已经进入排空阶段时才真正退出，
            // 否则继续阻塞等待——这样"采集刚结束、还在排队等上传时
            // 收到 SIGTERM"的任务不会被跳过。
            if (draining_)
                break;
        }
    }

} // namespace drop_agent
