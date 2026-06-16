// ============================================================
// drop_agent (Agent 主程序) 小白版注释
// ============================================================
// 这个程序运行在你要监控的 Linux 机器上
// 它的工作循环：
//   1. 启动 → 向 Server 报到
//   2. 每 5 秒发一次心跳（"我还活着"）
//   3. 如果心跳回复里有任务 → 执行 perf 采集
//   4. 采集完 → 回报 Server（NotifyResult）
// ============================================================

#include <iostream>   // cout, cerr（控制台输出）
#include <string>     // string 类型（字符串）
#include <thread>     // this_thread::sleep_for（让程序暂停）
#include <chrono>     // seconds（时间单位）
#include <unistd.h>   // fork, execvp, getpid
#include <sys/wait.h> // waitpid
#include <signal.h>   // kill, SIGTERM, SIGKILL, signal()
#include <fstream>    // ifstream（读文件）
#include <sstream>    // stringstream
#include <atomic>     // atomic<bool>

#include <grpcpp/grpcpp.h>                    // gRPC 客户端库
#include "common/proto/healthcheck.grpc.pb.h" // 心跳协议的 C++ 生成代码
#include "common/proto/hotmethod.grpc.pb.h"   // 任务协议的 C++ 生成代码
#include "common/proto/init.grpc.pb.h"        // 初始化协议的 C++ 生成代码

// 只引入 gRPC 相关的几个类（避免名字冲突）
using grpc::Channel;         // 通信通道（相当于"电话线"）
using grpc::ClientContext;   // 请求上下文（设置超时等）
using grpc::Status;          // 操作结果（成功/失败）
using namespace std;         // 省去 std:: 前缀
using namespace std::chrono; // 省去 chrono:: 前缀

// 全局退出标志，信号处理函数会设置为 false
static atomic<bool> agent_running{true};

// ============================================================
// 执行 perf 采集（fork + execvp）
// 参数：
//   taskDesc  - 任务描述（包含PID、频率、时长等）
//   outputPath - 输出文件路径，比如 "/tmp/perf_<taskID>.data"
// 返回：
//   0  = 成功
//   -1 = fork 失败
//   -2 = exec 失败
//   >0 = perf 的退出码（非 0 表示采集失败）
// ============================================================
int run_perf(const hotmethod::TaskDesc &taskDesc, const string &outputPath)
{
    pid_t pid = fork();
    if (pid < 0)
    {
        cerr << "[agent] fork 失败!" << endl;
        return -1;
    }

    if (pid == 0)
    {
        // ===== 子进程 =====
        // 把自己放进独立的进程组，方便超时杀掉整个组
        setpgid(0, 0);

        // 组装 perf record 的命令行参数
        // 例如：perf record -F 99 -g -o /tmp/perf_xxx.data -- sleep 10
        //
        // 注意：必须先把所有字符串放到 args_storage（保证内存稳定），
        // 再构建 char* 指针数组 args。否则 vector 扩容会导致 c_str() 指针悬空！
        vector<string> args_storage;
        args_storage.reserve(16); // 预分配，避免扩容

        args_storage.push_back("perf");
        args_storage.push_back("record");

        // 采样频率：-F 99
        args_storage.push_back("-F");
        args_storage.push_back(to_string(taskDesc.sampleargv().hz()));

        // 调用图：--call-graph fp
        string cg = taskDesc.sampleargv().callgraph();
        if (!cg.empty())
        {
            args_storage.push_back("--call-graph");
            args_storage.push_back(cg);
        }

        // 输出文件：-o /tmp/perf_xxx.data
        args_storage.push_back("-o");
        args_storage.push_back(outputPath);

        // 目标 PID（>0 才指定）
        int targetPid = taskDesc.sampleargv().pid();
        if (targetPid > 0)
        {
            args_storage.push_back("-p");
            args_storage.push_back(to_string(targetPid));
        }

        // 采集时长：-- sleep <duration>
        args_storage.push_back("--");
        args_storage.push_back("sleep");
        uint64_t dur = taskDesc.sampleargv().duration();
        if (dur == 0)
            dur = 10;
        args_storage.push_back(to_string(dur));

        // 所有字符串已就绪，现在构建 char* 指针数组（安全）
        vector<const char *> args;
        args.reserve(args_storage.size() + 1);
        for (const auto &s : args_storage)
        {
            args.push_back(s.c_str());
        }
        args.push_back(nullptr); // execvp 需要 NULL 终止

        // 打印命令行（调试用）
        cout << "[agent] 执行: ";
        for (size_t i = 0; i < args.size() - 1; i++)
            cout << args[i] << " ";
        cout << endl;

        // 执行 perf（execvp 会自动在 PATH 里找 perf）
        execvp("perf", const_cast<char *const *>(args.data()));

        // 如果 execvp 返回了，说明出错
        cerr << "[agent] execvp perf 失败!" << endl;
        _exit(127);
    }

    // ===== 父进程 =====
    cout << "[agent] perf 子进程 PID=" << pid << ", 等待" << taskDesc.sampleargv().duration() << "秒..." << endl;

    // 等待子进程结束（带超时）
    uint32_t timeout = taskDesc.timeoutsec();
    if (timeout == 0)
        timeout = 60; // 默认 60 秒超时

    int status;
    time_t start = time(nullptr);

    while (true)
    {
        pid_t result = waitpid(pid, &status, WNOHANG); // 非阻塞等待
        if (result == pid)
        {
            // 子进程已退出
            break;
        }
        if (result < 0)
        {
            cerr << "[agent] waitpid 出错" << endl;
            return -2;
        }

        // 检查是否超时
        if (time(nullptr) - start > timeout)
        {
            cerr << "[agent] perf 超时（" << timeout << "秒），强制终止" << endl;
            // 先发 SIGTERM 给整个进程组
            killpg(pid, SIGTERM);
            this_thread::sleep_for(seconds(5));
            // 再发 SIGKILL 确保杀掉
            killpg(pid, SIGKILL);
            // 回收僵尸
            waitpid(pid, &status, 0);
            return -3; // 超时
        }

        this_thread::sleep_for(milliseconds(500)); // 每 0.5 秒检查一次
    }

    if (WIFEXITED(status))
    {
        int exitCode = WEXITSTATUS(status);
        cout << "[agent] perf 子进程退出, exitCode=" << exitCode << endl;
        return exitCode;
    }
    if (WIFSIGNALED(status))
    {
        int sig = WTERMSIG(status);
        cerr << "[agent] perf 子进程被信号杀死: " << sig << endl;
        return -4;
    }

    return -5;
}

// ============================================================
// 读取文件内容（用于把 perf.data 读到内存，通过 gRPC 传回）
// ============================================================
string read_file_content(const string &path)
{
    ifstream file(path, ios::binary | ios::ate);
    if (!file.is_open())
        return "";
    streamsize size = file.tellg();
    file.seekg(0, ios::beg);
    string content(size, '\0');
    file.read(&content[0], size);
    return content;
}

// ============================================================
// main：Agent 入口
// ============================================================
int main(int argc, char **argv)
{
    // ---------- 1. 确定连接地址 ----------
    string server_addr = "localhost:50051"; // 默认连本地
    if (argc > 1)
    {
        server_addr = argv[1]; // 命令行可以指定地址
    }

    // 信号处理：Ctrl+C 或 kill 时优雅退出
    signal(SIGINT, [](int)
           { agent_running = false; });
    signal(SIGTERM, [](int)
           { agent_running = false; });

    cout << "[agent] drop_agent 启动，连接 server: " << server_addr << endl;

    // 创建到 Server 的"电话线"（不加密的连接）
    auto channel = grpc::CreateChannel(server_addr, grpc::InsecureChannelCredentials());

    // ---------- 2. 注册自己 ----------
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

        if (status.ok())
        {
            cout << "[agent] 注册成功" << endl;
        }
        else
        {
            cerr << "[agent] 注册失败: " << status.error_message() << endl;
            return 1;
        }
    }

    // ---------- 3. 心跳循环 ----------
    auto health_stub = healthcheck::HealthCheck::NewStub(channel);
    auto hotmethod_stub = hotmethod::Hotmethod::NewStub(channel);

    while (agent_running)
    {
        // 填写心跳请求
        healthcheck::HealthCheckRequest req;
        req.set_hostname("demo-host");
        req.set_ipaddr("127.0.0.1");
        req.set_uid("agent-001");
        req.set_agentversion("1.0.0");

        // 发送心跳
        healthcheck::HealthCheckResponse resp;
        ClientContext ctx;
        Status status = health_stub->Do(&ctx, req, &resp);

        if (status.ok())
        {
            cout << "[agent] 心跳 OK, pending=" << resp.pending() << endl;

            // ----- 如果有任务，执行 perf 采集 -----
            if (resp.pending() && resp.has_taskdesc())
            {
                const auto &task = resp.taskdesc();
                cout << "[agent] 收到任务! taskID=" << task.taskid()
                     << " profilerType=" << task.profilertype()
                     << " pid=" << task.sampleargv().pid()
                     << " hz=" << task.sampleargv().hz()
                     << " duration=" << task.sampleargv().duration() << endl;

                // 输出文件路径：/tmp/perf_<taskID>.data
                string outputPath = "/tmp/perf_" + task.taskid() + ".data";

                // 执行 perf 采集
                int result = run_perf(task, outputPath);

                // 构造 NotifyResult 请求
                hotmethod::TaskResult taskResult;
                taskResult.set_taskid(task.taskid());

                if (result == 0)
                {
                    // 成功：读取 perf.data 内容，填入 TaskResult
                    string fileContent = read_file_content(outputPath);
                    if (!fileContent.empty())
                    {
                        auto *file = taskResult.mutable_file();
                        file->set_name("perf.data");
                        file->set_content(fileContent);
                        file->set_size(fileContent.size());
                        cout << "[agent] 采集成功，文件大小=" << fileContent.size() << " bytes, 路径=" << outputPath << endl;
                    }
                    else
                    {
                        taskResult.set_errormessage("采集完成但无法读取输出文件");
                        cerr << "[agent] 无法读取输出文件: " << outputPath << endl;
                    }
                }
                else if (result == -3)
                {
                    taskResult.set_errormessage("perf 采集超时");
                    cerr << "[agent] 采集超时" << endl;
                }
                else
                {
                    taskResult.set_errormessage("perf 采集失败, exitCode=" + to_string(result));
                    cerr << "[agent] 采集失败, result=" << result << endl;
                }

                // 上报结果给 Server
                google::protobuf::Empty emptyResp;
                ClientContext notifyCtx;
                Status notifyStatus = hotmethod_stub->NotifyResult(&notifyCtx, taskResult, &emptyResp);

                if (notifyStatus.ok())
                {
                    cout << "[agent] NotifyResult 上报成功: taskID=" << task.taskid() << endl;
                }
                else
                {
                    cerr << "[agent] NotifyResult 上报失败: " << notifyStatus.error_message() << endl;
                }
            }
        }
        else
        {
            cerr << "[agent] 心跳失败: " << status.error_message() << endl;
        }

        // 等 5 秒再发下一次心跳（响应退出信号）
        for (int i = 0; i < 50 && agent_running; i++)
        {
            this_thread::sleep_for(milliseconds(100));
        }
    }

    cout << "[agent] 收到退出信号，正在关闭..." << endl;
    return 0;
}
