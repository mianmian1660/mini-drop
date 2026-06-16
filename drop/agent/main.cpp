// ============================================================
// drop_agent (Agent 主程序) W4 版本
// ============================================================
// 这个程序运行在你要监控的 Linux 机器上
// 它的工作循环：
//   1. 启动 → 向 Server 报到
//   2. 每 5 秒发一次心跳（"我还活着"），附带自监控 PidStats
//   3. 如果心跳回复里有任务 → 执行 perf 采集
//   4. 采集完 → 上传 MinIO → 回报 Server（NotifyResult）
//
// W4 新增功能：
//   - PidStats 自监控：采集 Agent 自身和子进程的 CPU/内存/IO
//   - MinIO 上传：采集结果上传到对象存储，返回 cosKey
//   - 超时杀进程：setpgid + killpg 强制终止卡住的采集
//   - 错误处理：PID 不存在时正确返回 errorMessage
// ============================================================

#include <iostream>   // cout, cerr（控制台输出）
#include <string>     // string 类型（字符串）
#include <thread>     // this_thread::sleep_for（让程序暂停）
#include <chrono>     // seconds（时间单位）
#include <unistd.h>   // fork, execvp, getpid
#include <sys/wait.h> // waitpid
#include <sys/stat.h> // stat(), S_ISDIR
#include <signal.h>   // kill, SIGTERM, SIGKILL, signal()
#include <fstream>    // ifstream（读文件）
#include <sstream>    // stringstream
#include <atomic>     // atomic<bool>
#include <vector>     // vector
#include <cstdio>     // popen, pclose（执行 shell 命令并读输出）
#include <cstring>    // strerror
#include <cerrno>     // errno
#include <dirent.h>   // opendir, readdir（遍历 /proc）
#include <cstdlib>    // atoi

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
// 工具函数：去除字符串两端的空白字符
// ============================================================
static string trim(const string &s)
{
    size_t start = 0;
    while (start < s.size() && isspace((unsigned char)s[start]))
        start++;
    size_t end = s.size();
    while (end > start && isspace((unsigned char)s[end - 1]))
        end--;
    return s.substr(start, end - start);
}

// ============================================================
// 工具函数：执行 shell 命令并返回标准输出（最多读 maxLen 字节）
// 用于读取 /proc 文件系统等
// ============================================================
static string shell_exec(const string &cmd, size_t maxLen = 4096)
{
    FILE *fp = popen(cmd.c_str(), "r");
    if (!fp)
        return "";
    string result(maxLen, '\0');
    size_t n = fread(&result[0], 1, maxLen - 1, fp);
    pclose(fp);
    result.resize(n);
    return result;
}

// ============================================================
// 工具函数：读取 /proc/[pid]/stat 的一行
// /proc/pid/stat 是空格分隔的字段，第14个字段是 utime，第15个是 stime
// 第24个是 RSS（页数），字段之间可能被 ) 和空格搞乱
// ============================================================
struct ProcStat
{
    long utime_ticks = 0; // 用户态 CPU 时间（jiffies）
    long stime_ticks = 0; // 内核态 CPU 时间（jiffies）
    long rss_pages = 0;   // 物理内存页数
    string comm;          // 进程名
    bool valid = false;
};

static ProcStat read_proc_stat(int pid)
{
    ProcStat ps;
    string path = "/proc/" + to_string(pid) + "/stat";
    ifstream f(path);
    if (!f.is_open())
        return ps;
    string line;
    getline(f, line);

    // /proc/pid/stat 格式特殊：comm 字段被括号包围，可能含空格
    // 格式: pid (comm) state ppid pgrp session tty_nr tpgid flags minflt cminflt majflt cmajflt utime stime ...
    // 我们需要 utime(stime) = 第14(15)个字段，rss = 第24个字段（1-based）
    // 因为 comm 可能含空格，所以不能简单 split
    size_t rparen = line.rfind(')');
    if (rparen == string::npos)
        return ps;
    // comm 就是括号里的内容
    size_t lparen = line.find('(');
    if (lparen == string::npos || lparen >= rparen)
        return ps;
    ps.comm = line.substr(lparen + 1, rparen - lparen - 1);

    // 括号后的部分用空格分割
    string rest = line.substr(rparen + 2); // 跳过 ") "
    istringstream iss(rest);
    vector<string> fields;
    string field;
    while (iss >> field)
        fields.push_back(field);

    // utime 和 stime 是括号后的第 11 和 12 个字段（从 0 开始是 index 11,12）
    // 因为 rest 以 "state" 开头，然后依次是 ppid, pgrp, session, tty_nr, tpgid,
    // flags, minflt, cminflt, majflt, cmajflt, utime, stime
    if (fields.size() >= 12)
    {
        ps.utime_ticks = atol(fields[11].c_str());
        ps.stime_ticks = atol(fields[12].c_str());
    }
    // rss 是括号后的第 21 个字段（index 21）
    if (fields.size() >= 22)
    {
        ps.rss_pages = atol(fields[21].c_str());
    }
    ps.valid = true;
    return ps;
}

// ============================================================
// 工具函数：读取 /proc/[pid]/io 获取读写的字节数
// ============================================================
struct ProcIO
{
    uint64_t read_bytes = 0;
    uint64_t write_bytes = 0;
    bool valid = false;
};

static ProcIO read_proc_io(int pid)
{
    ProcIO io;
    string path = "/proc/" + to_string(pid) + "/io";
    ifstream f(path);
    if (!f.is_open())
        return io;
    string line;
    while (getline(f, line))
    {
        if (line.find("read_bytes:") == 0)
        {
            io.read_bytes = strtoull(line.c_str() + 11, nullptr, 10);
        }
        else if (line.find("write_bytes:") == 0)
        {
            io.write_bytes = strtoull(line.c_str() + 12, nullptr, 10);
        }
    }
    io.valid = true;
    return io;
}

// ============================================================
// W4: 自监控 — 采集 Agent 自身进程的 PidStats
// 通过两次采样（间隔1秒）的差值来计算 CPU% 和 IO 速率
// ============================================================
static common::PidStats collect_self_pidstats()
{
    common::PidStats ps;
    int mypid = getpid();
    ps.set_pid(mypid);

    // 第一次采样
    ProcStat s1 = read_proc_stat(mypid);
    ProcIO io1 = read_proc_io(mypid);
    long hz = sysconf(_SC_CLK_TCK); // 每秒 jiffies 数（通常 100）

    // 等 1 秒
    this_thread::sleep_for(chrono::seconds(1));

    // 第二次采样
    ProcStat s2 = read_proc_stat(mypid);
    ProcIO io2 = read_proc_io(mypid);

    if (s1.valid && s2.valid)
    {
        long total_ticks = (s2.utime_ticks - s1.utime_ticks) + (s2.stime_ticks - s1.stime_ticks);
        if (total_ticks < 0)
            total_ticks = 0;
        // CPU% = ticks_delta / hz * 100（因为我们等了1秒）
        double cpuPct = (double)total_ticks / (double)hz * 100.0;
        ps.set_cpupercent(cpuPct);
        ps.set_rsskb((uint64_t)s2.rss_pages * 4); // 每页 4KB
        ps.set_comm(s2.comm);
    }
    if (io1.valid && io2.valid)
    {
        // KB/s（因为我们等了1秒）
        uint64_t readDelta = (io2.read_bytes > io1.read_bytes) ? (io2.read_bytes - io1.read_bytes) : 0;
        uint64_t writeDelta = (io2.write_bytes > io1.write_bytes) ? (io2.write_bytes - io1.write_bytes) : 0;
        ps.set_readkbpers(readDelta / 1024);
        ps.set_writekbpers(writeDelta / 1024);
    }
    return ps;
}

// ============================================================
// W4: 自监控 — 遍历 /proc 找到属于当前进程组的子进程并采集
// ============================================================
static vector<common::PidStats> collect_children_pidstats()
{
    vector<common::PidStats> result;
    // 简单实现：遍历 /proc 目录找 PPID 等于自己的进程
    DIR *dir = opendir("/proc");
    if (!dir)
        return result;
    int mypid = getpid();
    struct dirent *entry;
    while ((entry = readdir(dir)) != nullptr)
    {
        int childPid = atoi(entry->d_name);
        if (childPid <= 0)
            continue;
        // 检查 PPID
        ProcStat ps = read_proc_stat(childPid);
        if (!ps.valid)
            continue;
        // 通过 /proc/pid/status 读 PPID
        string statusPath = "/proc/" + to_string(childPid) + "/status";
        ifstream f(statusPath);
        if (!f.is_open())
            continue;
        string line;
        int ppid = 0;
        while (getline(f, line))
        {
            if (line.find("PPid:") == 0)
            {
                ppid = atoi(line.c_str() + 5);
                break;
            }
        }
        if (ppid == mypid)
        {
            common::PidStats childPs;
            childPs.set_pid(childPid);
            childPs.set_comm(ps.comm);
            childPs.set_rsskb((uint64_t)ps.rss_pages * 4);
            result.push_back(childPs);
        }
    }
    closedir(dir);
    return result;
}

// ============================================================
// W4: MinIO 上传 — 使用 curl 命令行上传文件到 MinIO/S3
// cosConfig: 从 Init 服务拿到的存储配置
// localPath: 本地文件路径
// remoteKey: 远程对象 key（如 "perf_<tid>.data"）
// 返回: 上传成功返回 true
// ============================================================
static bool upload_to_minio(const common::CosConfig &cosConfig,
                            const string &localPath,
                            const string &remoteKey)
{
    // 构造 MinIO/S3 的 curl 命令
    // curl -X PUT -T <file> "http://<endpoint>/<bucket>/<key>"
    //   -H "Content-Type: application/octet-stream"
    string scheme = cosConfig.usessl() ? "https://" : "http://";
    string endpoint = cosConfig.endpoint();
    if (endpoint.empty())
        endpoint = "minio:9000";
    string bucket = cosConfig.bucket();
    if (bucket.empty())
        bucket = "drop";

    string url = scheme + endpoint + "/" + bucket + "/" + remoteKey;

    // W4: 使用 curl 上传，设置连接超时 5 秒，总超时 10 秒
    // 这样即使 MinIO 不可达，也不会阻塞采集结果的上报
    string cmd = "curl -s --connect-timeout 5 --max-time 10 -o /dev/null -w '%{http_code}' -X PUT -T \"" +
                 localPath + "\" \"" + url + "\"";

    // 添加 MinIO 认证头
    if (!cosConfig.accesskeyid().empty())
    {
        // 使用 AWS Signature V4 太复杂，curl 可以直接用基本认证或预签名
        // 对于本地 MinIO，使用 access key / secret key 作为 basic auth
        string user = cosConfig.accesskeyid();
        string pass = cosConfig.secretaccesskey();
        cmd += " -u \"" + user + ":" + pass + "\"";
    }

    cout << "[agent] 上传 MinIO: " << url << " (key=" << remoteKey << ")" << endl;

    string result = shell_exec(cmd, 128);
    result = trim(result);

    int httpCode = atoi(result.c_str());
    if (httpCode >= 200 && httpCode < 300)
    {
        cout << "[agent] MinIO 上传成功! HTTP " << httpCode << " key=" << remoteKey << endl;
        return true;
    }
    else
    {
        cerr << "[agent] MinIO 上传失败! HTTP " << httpCode << " key=" << remoteKey << endl;
        return false;
    }
}

// ============================================================
// W4: 检查 PID 是否存在（/proc/<pid> 目录是否存在）
// ============================================================
static bool pid_exists(int pid)
{
    if (pid <= 0)
        return false;
    string path = "/proc/" + to_string(pid);
    struct ::stat st;
    return ::stat(path.c_str(), &st) == 0 && S_ISDIR(st.st_mode);
}

// ============================================================
// 执行 perf 采集（fork + execvp）
// 参数：
//   taskDesc  - 任务描述（包含PID、频率、时长等）
//   outputPath - 输出文件路径，比如 "/tmp/perf_<taskID>.data"
// 返回：
//   0  = 成功
//   -1 = fork 失败
//   -2 = exec 失败
//   -3 = 超时
//   -4 = 目标 PID 不存在
//   >0 = perf 的退出码（非 0 表示采集失败）
// ============================================================
int run_perf(const hotmethod::TaskDesc &taskDesc, const string &outputPath)
{
    // W4: 检查目标 PID 是否存在
    int targetPid = taskDesc.sampleargv().pid();
    if (targetPid > 0 && !pid_exists(targetPid))
    {
        cerr << "[agent] 目标 PID " << targetPid << " 不存在!" << endl;
        return -4; // PID 不存在
    }

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
        vector<string> args_storage;
        args_storage.reserve(16);

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
            cerr << "[agent] waitpid 出错: " << strerror(errno) << endl;
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
        return -5;
    }

    return -6;
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
// main：Agent 入口（W4 增强版）
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

    // ---------- 2. 注册自己并拉取 COS 配置 ----------
    common::CosConfig cosConfig; // W4: 存储 MinIO/COS 配置
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

        // W4: 拉取 COS/MinIO 配置
        initpb::FetchConfigRequest cfgReq;
        cfgReq.set_uid("agent-001");
        initpb::FetchConfigResponse cfgResp;
        ClientContext cfgCtx;
        Status cfgStatus = stub->FetchConfig(&cfgCtx, cfgReq, &cfgResp);

        if (cfgStatus.ok() && cfgResp.has_cosconfig())
        {
            cosConfig.CopyFrom(cfgResp.cosconfig());
            cout << "[agent] 获取 COS 配置成功: endpoint=" << cosConfig.endpoint()
                 << " bucket=" << cosConfig.bucket() << endl;
        }
        else
        {
            // 设置默认 MinIO 配置（本地开发用）
            cosConfig.set_endpoint("minio:9000");
            cosConfig.set_accesskeyid("drop");
            cosConfig.set_secretaccesskey("dropdrop");
            cosConfig.set_bucket("drop");
            cosConfig.set_usessl(false);
            cout << "[agent] 使用默认 MinIO 配置: " << cosConfig.endpoint() << endl;
        }
    }

    // ---------- 3. 心跳循环 ----------
    auto health_stub = healthcheck::HealthCheck::NewStub(channel);
    auto hotmethod_stub = hotmethod::Hotmethod::NewStub(channel);

    while (agent_running)
    {
        // W4: 采集自监控 PidStats
        common::PidStats selfPs = collect_self_pidstats();
        vector<common::PidStats> childrenPs = collect_children_pidstats();

        // 填写心跳请求（附带 PidStats）
        healthcheck::HealthCheckRequest req;
        req.set_hostname("demo-host");
        req.set_ipaddr("127.0.0.1");
        req.set_uid("agent-001");
        req.set_agentversion("1.0.0");
        // W4: 附加自监控数据（proto 中 childrenPstats 是单数，只取第一个子进程）
        *req.mutable_selfpstats() = selfPs;
        if (!childrenPs.empty())
        {
            *req.mutable_childrenpstats() = childrenPs[0];
        }

        cout << "[agent] 自监控: CPU=" << selfPs.cpupercent()
             << "% RSS=" << selfPs.rsskb() << "KB"
             << " 子进程数=" << childrenPs.size() << endl;

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

                // 执行 perf 采集（会先检查 PID 是否存在）
                int result = run_perf(task, outputPath);

                // 构造 NotifyResult 请求
                hotmethod::TaskResult taskResult;
                taskResult.set_taskid(task.taskid());

                if (result == 0)
                {
                    // 成功：读取 perf.data 内容
                    string fileContent = read_file_content(outputPath);
                    if (!fileContent.empty())
                    {
                        // W4: 尝试上传到 MinIO，上传成功则设置 cosKey
                        string remoteKey = "perf_" + task.taskid() + ".data";
                        if (upload_to_minio(cosConfig, outputPath, remoteKey))
                        {
                            taskResult.set_coskey(remoteKey);
                            cout << "[agent] 文件已上传 MinIO, cosKey=" << remoteKey << endl;
                        }
                        // 无论上传成功与否，都通过 gRPC 附带小文件内容
                        auto *file = taskResult.mutable_file();
                        file->set_name("perf.data");
                        file->set_content(fileContent);
                        file->set_size(fileContent.size());
                        cout << "[agent] 采集成功，文件大小=" << fileContent.size() << " bytes" << endl;
                    }
                    else
                    {
                        taskResult.set_errormessage("采集完成但无法读取输出文件");
                        cerr << "[agent] 无法读取输出文件: " << outputPath << endl;
                    }
                }
                else if (result == -4)
                {
                    // W4: PID 不存在
                    taskResult.set_errormessage("目标 PID " + to_string(task.sampleargv().pid()) + " 不存在");
                    cerr << "[agent] " << taskResult.errormessage() << endl;
                }
                else if (result == -3)
                {
                    taskResult.set_errormessage("perf 采集超时（" + to_string(task.timeoutsec()) + "秒）");
                    cerr << "[agent] 采集超时" << endl;
                }
                else if (result == -1 || result == -2 || result == -5 || result == -6)
                {
                    taskResult.set_errormessage("perf 进程异常, resultCode=" + to_string(result));
                    cerr << "[agent] perf 进程异常, result=" << result << endl;
                }
                else
                {
                    taskResult.set_errormessage("perf 采集失败, exitCode=" + to_string(result));
                    cerr << "[agent] 采集失败, result=" << result << endl;
                }

                // W4: 附加 PidStats 到任务结果
                *taskResult.add_selfpstats() = selfPs;
                for (const auto &c : childrenPs)
                {
                    *taskResult.add_childrenpstats() = c;
                }

                // 上报结果给 Server
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
