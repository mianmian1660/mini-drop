// ============================================================
// common/SymbolCollector.cpp — 用户态符号按 build-id 去重上传（阶段三）
// ============================================================
#include "common/SymbolCollector.h"
#include "common/BuildId.h"
#include "common/Utils.h"

#include <cstdio>
#include <cstdlib>
#include <fstream>
#include <iostream>
#include <map>
#include <mutex>
#include <set>
#include <sys/stat.h>
#include <thread>
#include <unistd.h>
#include <vector>

using namespace std;

namespace drop
{

    namespace
    {
        // JSON 字符串转义。本文件手写解析/构造 JSON，不引入第三方库——
        // 请求/响应形状固定且简单（build-id 是十六进制串，不会出现需要
        // 转义的字符），没必要为这一处引入依赖。
        string json_escape(const string &s)
        {
            string out;
            out.reserve(s.size());
            for (char c : s)
            {
                switch (c)
                {
                case '"':
                    out += "\\\"";
                    break;
                case '\\':
                    out += "\\\\";
                    break;
                case '\n':
                    out += "\\n";
                    break;
                default:
                    out += c;
                }
            }
            return out;
        }

        // 与 PerfRunner/ContinuousSampler 保持一致：录制用 DROP_PERF_BIN
        // (= perf-real)，这里 buildid-list 也走同一二进制，避免命中 /usr/bin/perf
        // 包装脚本因内核版本不匹配而 rc=2 的问题。
        string resolve_perf_bin()
        {
            const char *configured = getenv("DROP_PERF_BIN");
            if (configured && configured[0] != '\0')
                return string(configured);
            return "perf";
        }

        bool file_readable(const string &path)
        {
            struct stat st;
            return ::stat(path.c_str(), &st) == 0 && S_ISREG(st.st_mode);
        }

        // parse_buildid_list 现在是 common/BuildId.h 里的共享函数
        // （drop::parse_buildid_list，返回 drop::BuildIdEntry），
        // 持续采集(ContinuousSampler.cpp)复用同一份实现。

        // dso_path 是目标进程视角的路径，drop_agent 自己的文件系统里不一定有。
        // 先试字面路径（覆盖 host 上确实共享可见的情况），不行就用
        // /proc/<targetPid>/root/ 这个内核提供的跨容器视图兜底——和阶段二
        // 已验证过的 perf record 自身读取目标二进制走的是同一套机制
        // （见 docs/symbolization-design.md §9.2）。仍读不到就放弃，这是
        // 整机(-a)模式下的已知局限：多个不同进程的二进制无法逐一兜底解析。
        string resolve_readable_path(const string &dsoPath, int targetPid)
        {
            if (file_readable(dsoPath))
                return dsoPath;
            if (targetPid > 0)
            {
                string candidate = "/proc/" + to_string(targetPid) + "/root" + dsoPath;
                if (file_readable(candidate))
                    return candidate;
            }
            return "";
        }

        string build_check_request(const string &tid, const vector<BuildIdEntry> &entries)
        {
            string body = "{\"tid\":\"" + json_escape(tid) + "\",\"entries\":[";
            for (size_t i = 0; i < entries.size(); ++i)
            {
                if (i)
                    body += ",";
                body += "{\"build_id\":\"" + json_escape(entries[i].buildId) +
                        "\",\"dso_path\":\"" + json_escape(entries[i].dsoPath) + "\"}";
            }
            body += "]}";
            return body;
        }

        // 手写提取 {"missing":["id1","id2"]} 里的字符串数组。
        // 不引入通用 JSON 库：响应形状固定，build-id 只会是十六进制字符串，
        // 不含引号/转义，逐字符扫 "..." token 足够。
        vector<string> extract_missing(const string &response)
        {
            vector<string> missing;
            size_t keyPos = response.find("\"missing\"");
            if (keyPos == string::npos)
                return missing;
            size_t bracketStart = response.find('[', keyPos);
            if (bracketStart == string::npos)
                return missing;
            size_t bracketEnd = response.find(']', bracketStart);
            if (bracketEnd == string::npos)
                return missing;
            string inner = response.substr(bracketStart + 1, bracketEnd - bracketStart - 1);
            size_t pos = 0;
            while (pos < inner.size())
            {
                size_t q1 = inner.find('"', pos);
                if (q1 == string::npos)
                    break;
                size_t q2 = inner.find('"', q1 + 1);
                if (q2 == string::npos)
                    break;
                missing.push_back(inner.substr(q1 + 1, q2 - q1 - 1));
                pos = q2 + 1;
            }
            return missing;
        }

        // POST 请求体走临时文件 + `curl -d @文件`，不直接拼进 argv——
        // 一个任务可能引用几十个 DSO，拼进单个 argv 元素有撞
        // Linux MAX_ARG_STRLEN(128KB) 的风险，那样会是 execvp 的硬失败
        // (E2BIG)，不是能降级处理的错误。写文件从根源上避免这个问题。
        //
        // exec_capture 会把子进程 stdout/stderr 合并进同一个 buffer，
        // 所以这里必须用 -sS（成功时静默、只在失败时打印错误），保证
        // 成功路径下 buffer 里只有纯 JSON，不会混进 curl 自己的提示文字。
        bool http_post_json(const string &url, const string &bodyFilePath, string *response)
        {
            int rc = drop::exec_capture(
                {"curl", "-sS", "-m", "10", "-X", "POST",
                 "-H", "Content-Type: application/json",
                 "-d", "@" + bodyFilePath, url},
                response, 65536);
            return rc == 0;
        }

        bool http_put_file(const string &url, const string &filePath, string *response)
        {
            int rc = drop::exec_capture(
                {"curl", "-sS", "-m", "30", "-X", "PUT",
                 "--data-binary", "@" + filePath, url},
                response, 4096);
            return rc == 0;
        }

        // ---- 持续采集链路的 build-id 上报（无 tid，异步去重） ----
        // 跨窗口去重：同一个 build-id 一旦确认服务端已有/已上传成功，就不再
        // 每窗口重复 check+upload。进程重启即清空，代价只是重启后重查一次。
        std::mutex g_uploadedBuildIdsMutex;
        std::set<string> g_uploadedBuildIds;

        string build_ids_check_request(const vector<BuildIdEntry> &entries)
        {
            string body = "{\"build_ids\":[";
            for (size_t i = 0; i < entries.size(); ++i)
            {
                if (i)
                    body += ",";
                body += "\"" + json_escape(entries[i].buildId) + "\"";
            }
            body += "]}";
            return body;
        }

        void mark_build_ids_uploaded(const vector<BuildIdEntry> &entries)
        {
            std::lock_guard<std::mutex> lock(g_uploadedBuildIdsMutex);
            for (const auto &e : entries)
                g_uploadedBuildIds.insert(e.buildId);
        }

        // 同步执行一次"检查缺失 -> 上传缺失"的往返。只从本地 build-id 缓存读
        // 二进制（warm_build_id_cache 已把能读到的预热进去），读不到就跳过，
        // 下个窗口预热成功后自然重试。整个函数失败只降级，不影响采集。
        void upload_build_ids_sync(const vector<BuildIdEntry> &entries, const string &apiBaseURL)
        {
            string reqPath = "/tmp/mini_drop_cp_symcheck_" + std::to_string(::getpid()) + ".json";
            {
                ofstream out(reqPath, ios::binary);
                if (!out.is_open())
                    return;
                out << build_ids_check_request(entries);
            }

            string checkResp;
            bool checkOk = http_post_json(apiBaseURL + "/api/v1/internal/continuous/symbol-check", reqPath, &checkResp);
            ::remove(reqPath.c_str());
            if (!checkOk)
            {
                cout << "[cp-symbols] continuous symbol-check 调用失败，符号上报降级" << endl;
                return;
            }

            vector<string> missing = extract_missing(checkResp);
            if (missing.empty())
            {
                // 服务端已有全部 build-id，标记为已上报，避免下个窗口重复查询。
                mark_build_ids_uploaded(entries);
                return;
            }

            map<string, string> cacheByBuildId;
            for (const auto &e : entries)
                cacheByBuildId[e.buildId] = drop::build_id_local_cache_path(e.buildId);

            int uploaded = 0;
            vector<BuildIdEntry> uploadedEntries;
            for (const auto &buildId : missing)
            {
                auto it = cacheByBuildId.find(buildId);
                if (it == cacheByBuildId.end())
                    continue;
                const string &cachedPath = it->second;
                if (cachedPath.empty() || !file_readable(cachedPath))
                    continue; // 本地缓存尚未预热好，下个窗口重试
                string putResp;
                if (http_put_file(apiBaseURL + "/api/v1/symbols/" + buildId, cachedPath, &putResp))
                {
                    ++uploaded;
                    uploadedEntries.push_back({buildId, cachedPath});
                    cout << "[cp-symbols] 上传成功 build_id=" << buildId << endl;
                }
            }
            // 只标记成功上传的；失败的保留待下个窗口重试。
            if (uploaded > 0)
                mark_build_ids_uploaded(uploadedEntries);
        }

    } // namespace

    void upload_build_ids_async(vector<BuildIdEntry> entries, const string &apiBaseURL)
    {
        if (entries.empty() || apiBaseURL.empty())
            return;

        vector<BuildIdEntry> pending;
        {
            std::lock_guard<std::mutex> lock(g_uploadedBuildIdsMutex);
            for (const auto &e : entries)
            {
                if (g_uploadedBuildIds.count(e.buildId))
                    continue;
                pending.push_back(e);
            }
        }
        if (pending.empty())
            return;

        // detach 后台线程，不阻塞当次 perf script 解析。entries/pending 是
        // 值拷贝，detach 后生命周期独立。
        std::thread([pending, apiBaseURL]() {
            upload_build_ids_sync(pending, apiBaseURL);
        }).detach();
    }

    bool collect_and_upload_symbols(const string &perfDataPath,
                                     const string &tid,
                                     int targetPid,
                                     const string &apiBaseURL)
    {
        string listOutput;
        int rc = drop::exec_capture({resolve_perf_bin(), "buildid-list", "-i", perfDataPath}, &listOutput, 65536);
        if (rc != 0)
        {
            cout << "[symbols] perf buildid-list 失败(rc=" << rc << ")，跳过符号上传" << endl;
            return false;
        }

        vector<BuildIdEntry> entries = parse_buildid_list(listOutput);
        if (entries.empty())
        {
            cout << "[symbols] 未发现可上传的用户态符号" << endl;
            return true; // 没有需要处理的条目，不算失败
        }

        string reqPath = "/tmp/" + tid + "_symcheck_req.json";
        {
            ofstream out(reqPath, ios::binary);
            if (!out.is_open())
            {
                cout << "[symbols] 无法写入符号检查请求文件: " << reqPath << endl;
                return false;
            }
            out << build_check_request(tid, entries);
        }

        string checkResp;
        bool checkOk = http_post_json(apiBaseURL + "/api/v1/symbols/check", reqPath, &checkResp);
        ::remove(reqPath.c_str());

        if (!checkOk)
        {
            cout << "[symbols] /symbols/check 调用失败，用户态符号可能无法解析" << endl;
            return false;
        }

        vector<string> missing = extract_missing(checkResp);
        if (missing.empty())
        {
            cout << "[symbols] 服务端已有全部 " << entries.size() << " 个符号，无需上传" << endl;
            return true;
        }

        map<string, string> pathByBuildId;
        for (const auto &e : entries)
            pathByBuildId[e.buildId] = e.dsoPath;

        int uploaded = 0, skipped = 0;
        for (const auto &buildId : missing)
        {
            auto it = pathByBuildId.find(buildId);
            if (it == pathByBuildId.end())
                continue; // 服务端返回了本次没发过的 build-id，忽略

            string resolved = resolve_readable_path(it->second, targetPid);
            if (resolved.empty())
            {
                cout << "[symbols] 无法读取 build_id=" << buildId
                     << " 对应的二进制(" << it->second << ")，跳过" << endl;
                ++skipped;
                continue;
            }

            string putResp;
            string url = apiBaseURL + "/api/v1/symbols/" + buildId;
            if (http_put_file(url, resolved, &putResp))
            {
                cout << "[symbols] 上传成功 build_id=" << buildId << " path=" << resolved << endl;
                ++uploaded;
            }
            else
            {
                cout << "[symbols] 上传失败 build_id=" << buildId << " path=" << resolved << endl;
            }
        }

        cout << "[symbols] 检查完成: 共" << entries.size() << " 个 build-id, 缺失 " << missing.size()
             << " 个, 成功上传 " << uploaded << " 个, 跳过 " << skipped << " 个" << endl;
        return true;
    }

} // namespace drop
