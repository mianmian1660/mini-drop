// ============================================================
// common/KernelSymbols.cpp — 内核符号(kallsyms)快照与去重上传（共享）
// ============================================================
// 逻辑等价于 drop/agent/UploadWorker.cpp 里一次性任务的 kallsyms 快照/上传，
// 提取成自包含实现供持续采集链路复用。协议与 apiserver 的
// POST /api/v1/kernel-symbols/check + PUT /api/v1/kernel-symbols/:sha256
// 对齐（见 apiserver/server/symbol.go CheckKernelSymbol / UploadKernelSymbol）。
// ============================================================

#include "common/KernelSymbols.h"
#include "common/Utils.h"

#include <cctype>
#include <cstdio>
#include <cstdlib>
#include <fstream>
#include <iostream>
#include <sys/stat.h>
#include <sys/utsname.h>
#include <unistd.h>

using namespace std;

namespace drop
{

    namespace
    {

        int64_t file_size(const string &path)
        {
            struct stat st;
            if (::stat(path.c_str(), &st) != 0)
                return 0;
            return static_cast<int64_t>(st.st_size);
        }

        string sha256_file(const string &path)
        {
            string output;
            int ret = drop::exec_capture({"sha256sum", path}, &output, 512);
            if (ret != 0 || output.empty())
                return "";
            size_t space = output.find(' ');
            return space == string::npos ? output : output.substr(0, space);
        }

        string url_encode(const string &s)
        {
            static const char *hex = "0123456789ABCDEF";
            string out;
            for (unsigned char c : s)
            {
                if ((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
                    (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~')
                    out += static_cast<char>(c);
                else
                {
                    out += '%';
                    out += hex[c >> 4];
                    out += hex[c & 15];
                }
            }
            return out;
        }

        string kernel_release()
        {
            struct utsname u {};
            if (::uname(&u) == 0)
                return string(u.release);
            return "";
        }

        string agent_hostname()
        {
            const char *envHost = getenv("DROP_AGENT_HOSTNAME");
            if (envHost && *envHost)
                return string(envHost);
            char buf[256] = {0};
            if (::gethostname(buf, sizeof(buf) - 1) == 0)
                return string(buf);
            return "";
        }

        string agent_ip()
        {
            const char *envIP = getenv("DROP_AGENT_IP");
            if (envIP && *envIP)
                return string(envIP);
            return "";
        }

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
                case '\r':
                    out += "\\r";
                    break;
                case '\t':
                    out += "\\t";
                    break;
                default:
                    out += c;
                }
            }
            return out;
        }

        bool response_upload_required(const string &response)
        {
            string compact;
            compact.reserve(response.size());
            for (char c : response)
                if (!isspace(static_cast<unsigned char>(c)))
                    compact += c;
            return compact.find("\"upload_required\":true") != string::npos;
        }

        bool response_upload_not_required(const string &response)
        {
            string compact;
            compact.reserve(response.size());
            for (char c : response)
                if (!isspace(static_cast<unsigned char>(c)))
                    compact += c;
            return compact.find("\"upload_required\":false") != string::npos;
        }

        bool check_kernel_symbol(const string &baseURL,
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
                    << "\"tid\":\"" << json_escape(tid) << "\","
                    << "\"sha256\":\"" << json_escape(sha256) << "\","
                    << "\"size_bytes\":" << size << ","
                    << "\"kernel_release\":\"" << json_escape(kernel_release()) << "\","
                    << "\"hostname\":\"" << json_escape(agent_hostname()) << "\","
                    << "\"target_ip\":\"" << json_escape(agent_ip()) << "\""
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

        bool put_kernel_symbol(const string &baseURL,
                               const string &tid,
                               const string &sha256,
                               const string &path,
                               string *response)
        {
            string url = baseURL + "/api/v1/kernel-symbols/" + sha256 +
                         "?tid=" + url_encode(tid) +
                         "&kernel_release=" + url_encode(kernel_release()) +
                         "&hostname=" + url_encode(agent_hostname()) +
                         "&target_ip=" + url_encode(agent_ip());
            int rc = drop::exec_capture({"curl", "-sS", "-m", "60", "-X", "PUT",
                                         "--data-binary", "@" + path, url},
                                        response, 4096);
            return rc == 0;
        }

    } // namespace

    bool snapshot_kallsyms(const string &outPath)
    {
        ifstream in("/proc/kallsyms");
        if (!in.is_open())
        {
            cout << "[cp-kallsyms] 无法读取 /proc/kallsyms，跳过内核符号快照" << endl;
            return false;
        }

        string content;
        string line;
        bool sawNonZeroAddr = false;
        size_t lineNo = 0;
        while (getline(in, line))
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
            cout << "[cp-kallsyms] /proc/kallsyms 为空，跳过内核符号快照" << endl;
            return false;
        }
        if (!sawNonZeroAddr)
        {
            cout << "[cp-kallsyms] /proc/kallsyms 地址全为 0（缺少 CAP_SYSLOG 或 "
                 << "kptr_restrict 受限），拒绝上传无效符号表" << endl;
            return false;
        }

        ofstream out(outPath, ios::binary);
        if (!out.is_open())
        {
            cout << "[cp-kallsyms] 无法写入 kallsyms 快照: " << outPath << endl;
            return false;
        }
        out << content;
        out.close();
        return true;
    }

    string ensure_kernel_symbol_uploaded(const string &apiBaseURL,
                                         const string &tid,
                                         const string &kallsymsPath)
    {
        string sum = sha256_file(kallsymsPath);
        if (sum.empty())
        {
            cout << "[cp-kallsyms] kallsyms sha256 计算失败，跳过去重上传" << endl;
            return "";
        }

        string checkResp;
        if (!check_kernel_symbol(apiBaseURL, tid, sum, file_size(kallsymsPath), &checkResp))
        {
            cout << "[cp-kallsyms] kernel-symbols/check 调用失败，内核符号将降级" << endl;
            return "";
        }
        if (response_upload_not_required(checkResp))
        {
            cout << "[cp-kallsyms] 服务端已有 kallsyms sha256=" << sum << "，复用共享对象" << endl;
            return sum;
        }
        if (!response_upload_required(checkResp))
        {
            cout << "[cp-kallsyms] kernel-symbols/check 响应不可识别: " << checkResp << endl;
            return "";
        }

        string putResp;
        if (!put_kernel_symbol(apiBaseURL, tid, sum, kallsymsPath, &putResp))
        {
            cout << "[cp-kallsyms] kernel-symbols 上传失败，内核符号将降级" << endl;
            return "";
        }
        cout << "[cp-kallsyms] kallsyms 去重上传成功 sha256=" << sum << endl;
        return sum;
    }

} // namespace drop
