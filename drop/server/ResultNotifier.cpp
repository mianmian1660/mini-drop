// ============================================================
// server/ResultNotifier.cpp — 结果回调通知 实现（A3）
// ============================================================
// 用 fork+execvp 调用 curl，参数走 argv 数组，不经过 shell 解释，
// 避免 taskID/errorMessage 里出现特殊字符导致命令注入（呼应老指南 1.7 节踩坑提醒）。
// 目标地址可用 APISERVER_CALLBACK_URL 环境变量覆盖，默认指向 docker-compose 里的
// apiserver 服务名。当前 apiserver 侧尚未提供对应的接收端点，通知会失败但不影响
// 主流程——drop_server 只是把它当作"尽力而为"的加速通知。
// ============================================================

#include "server/ResultNotifier.h"

#include <cstdlib>
#include <iostream>
#include <sys/wait.h>
#include <unistd.h>

using namespace std;

namespace drop_server
{

    static string apiserver_callback_url()
    {
        const char *v = getenv("APISERVER_CALLBACK_URL");
        if (v && *v)
            return string(v);
        return "http://apiserver:8191/api/v1/internal/task-notify";
    }

    // 极简 JSON 字符串转义：只处理 curl POST body 会用到的双引号/反斜杠/换行，
    // taskID/cosKey 是内部生成的可控字符串，errorMessage 可能来自采集工具输出。
    static string json_escape(const string &s)
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

    bool notify_apiserver_task_result(const hotmethod::TaskResult &result)
    {
        string url = apiserver_callback_url();

        string body = "{";
        body += "\"task_id\":\"" + json_escape(result.taskid()) + "\",";
        body += "\"error_message\":\"" + json_escape(result.errormessage()) + "\",";
        body += "\"cos_key\":\"" + json_escape(result.coskey()) + "\"";
        body += "}";

        pid_t pid = fork();
        if (pid < 0)
        {
            cerr << "[notify] fork 失败，跳过通知" << endl;
            return false;
        }

        if (pid == 0)
        {
            // 子进程：argv 数组传参给 curl，不经过 shell
            execlp("curl", "curl",
                   "-sS", "-m", "5", // 静默 + 5 秒超时，避免通知拖住主流程
                   "-X", "POST",
                   "-H", "Content-Type: application/json",
                   "-d", body.c_str(),
                   "-o", "/dev/null",
                   "-w", "%{http_code}",
                   url.c_str(),
                   (char *)nullptr);
            _exit(127); // execlp 失败（curl 不存在等）
        }

        int status = 0;
        waitpid(pid, &status, 0);
        bool ok = WIFEXITED(status) && WEXITSTATUS(status) == 0;

        if (ok)
        {
            cout << "[notify] 已通知 apiserver: taskID=" << result.taskid() << endl;
        }
        else
        {
            cerr << "[notify] 通知 apiserver 失败（不影响主流程，apiserver 轮询仍会兜底发现）: "
                 << "taskID=" << result.taskid() << endl;
        }
        return ok;
    }

} // namespace drop_server
