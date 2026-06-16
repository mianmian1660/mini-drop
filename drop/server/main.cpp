// ============================================================
// drop_server (Server 主程序) W4 版本
// ============================================================
// 这个程序是"调度中心"，运行在一台中心服务器上
// 它同时启动 4 个 gRPC 服务，各司其职：
//   1. HealthCheck 服务 — 收心跳、派任务、记录 Agent 资源状态
//   2. Hotmethod 服务  — 收采集结果
//   3. Control 服务     — 收 API 后台的指令
//   4. Init 服务        — 处理 Agent 注册
//
// W4 新增功能：
//   - 记录 Agent 自监控 PidStats
//   - Agent 离线检测（30 秒无心跳判离线）
//   - 返回默认 COS 配置
// ============================================================

#include <iostream>      // cout（控制台输出）
#include <string>        // string
#include <thread>        // thread
#include <chrono>        // 时间相关
#include <memory>        // unique_ptr（智能指针，自动释放内存）
#include <mutex>         // mutex（保护共享队列）
#include <queue>         // queue（任务队列）
#include <unordered_map> // unordered_map（按 IP 保存任务队列）
#include <cstdlib>       // rand(), getenv()
#include <csignal>       // signal()
#include <atomic>        // atomic<bool>

#include <grpcpp/grpcpp.h>                             // gRPC 服务端库
#include <grpcpp/ext/proto_server_reflection_plugin.h> // gRPC 反射（让 grpcurl 能发现服务）
#include "common/proto/healthcheck.grpc.pb.h"          // 心跳协议
#include "common/proto/hotmethod.grpc.pb.h"            // 任务协议
#include "common/proto/control.grpc.pb.h"              // 控制协议
#include "common/proto/init.grpc.pb.h"                 // 初始化协议

// 引入 gRPC 服务端需要的类
using grpc::Server;        // gRPC 服务器
using grpc::ServerBuilder; // 服务器构建器
using grpc::ServerContext; // 请求上下文
using grpc::Status;        // 返回状态
using namespace std;

// W4: Agent 信息结构 — 记录每个 Agent 的心跳时间、状态和资源
struct AgentInfo
{
    string hostname;
    string ipAddr;
    string uid;
    string version;
    bool online = false;
    chrono::steady_clock::time_point lastHeartbeat;
    common::PidStats lastSelfPstats;
    vector<common::PidStats> lastChildrenPstats;
};

static mutex tasks_mutex;
static unordered_map<string, queue<hotmethod::TaskDesc>> tasks_;

// W4: Agent 信息表（按 uid 索引）
static mutex agents_mutex;
static unordered_map<string, AgentInfo> agents_;

static atomic<bool> server_running{true};
static const int AGENT_TIMEOUT_SEC = 30; // 30 秒无心跳判离线

// ============================================================
// 第1个服务：HealthCheck（心跳）— W4 增强版
// ============================================================
class HealthCheckServiceImpl final : public healthcheck::HealthCheck::Service
{
    Status Do(ServerContext *context,
              const healthcheck::HealthCheckRequest *request,
              healthcheck::HealthCheckResponse *response) override
    {
        string uid = request->uid();
        auto now = chrono::steady_clock::now();

        // W4: 更新 Agent 信息
        {
            lock_guard<mutex> lock(agents_mutex);
            AgentInfo &info = agents_[uid];
            bool wasOffline = !info.online;

            info.hostname = request->hostname();
            info.ipAddr = request->ipaddr();
            info.uid = uid;
            info.version = request->agentversion();
            info.online = true;
            info.lastHeartbeat = now;
            if (request->has_selfpstats())
            {
                info.lastSelfPstats.CopyFrom(request->selfpstats());
            }
            info.lastChildrenPstats.clear();
            if (request->has_childrenpstats())
            {
                info.lastChildrenPstats.push_back(request->childrenpstats());
            }

            if (wasOffline)
            {
                cout << "[server] 🔔 Agent 恢复上线: uid=" << uid
                     << " host=" << request->hostname()
                     << " ip=" << request->ipaddr() << endl;
            }
        }

        cout << "[server] 收到心跳: host=" << request->hostname()
             << " ip=" << request->ipaddr()
             << " uid=" << uid;
        if (request->has_selfpstats())
        {
            cout << " CPU=" << request->selfpstats().cpupercent() << "%"
                 << " RSS=" << request->selfpstats().rsskb() << "KB";
        }
        cout << endl;

        // 回复 Agent
        response->set_status(healthcheck::HealthCheckResponse::SERVING);
        response->set_pending(false);
        {
            lock_guard<mutex> lock(tasks_mutex);
            auto it = tasks_.find(request->ipaddr());
            if (it != tasks_.end() && !it->second.empty())
            {
                response->set_pending(true);
                response->mutable_taskdesc()->Swap(&it->second.front());
                it->second.pop();
                if (it->second.empty())
                {
                    tasks_.erase(it);
                }
            }
        }

        return Status::OK;
    }
};

// ============================================================
// 第2个服务：Hotmethod（任务执行与结果回报）— W4 增强版
// ============================================================
class HotmethodServiceImpl final : public hotmethod::Hotmethod::Service
{
    Status Collect(ServerContext *context,
                   const hotmethod::Target *request,
                   google::protobuf::Empty *response) override
    {
        cout << "[server] Collect 请求: ip=" << request->ip() << endl;
        return Status::OK;
    }

    Status NotifyResult(ServerContext *context,
                        const hotmethod::TaskResult *request,
                        google::protobuf::Empty *response) override
    {
        cout << "[server] 收到结果: taskID=" << request->taskid()
             << " error=\"" << request->errormessage() << "\""
             << " fileSize=" << request->file().size()
             << " cosKey=" << request->coskey()
             << " pstats_count=" << request->selfpstats_size()
             << endl;
        // W4: 打印 PidStats
        for (int i = 0; i < request->selfpstats_size(); i++)
        {
            const auto &ps = request->selfpstats(i);
            cout << "[server]   Agent PidStats: pid=" << ps.pid()
                 << " CPU=" << ps.cpupercent() << "%"
                 << " RSS=" << ps.rsskb() << "KB"
                 << " IO_r=" << ps.readkbpers() << "KB/s"
                 << " IO_w=" << ps.writekbpers() << "KB/s" << endl;
        }
        return Status::OK;
    }
};

// ============================================================
// 第3个服务：Control（API 后台的指挥接口）
// ============================================================
class ControlServiceImpl final : public control::Control::Service
{
    // CreateTask：API 后台下发一个新任务
    Status CreateTask(ServerContext *context,
                      const control::CreateTaskRequest *request,
                      control::CreateTaskResponse *response) override
    {
        cout << "[server] CreateTask: targetIP=" << request->targetip() << endl;

        // 生成任务 ID（用时间戳+随机数）
        string taskID = "task-" + to_string(chrono::system_clock::now().time_since_epoch().count()) + "-" + to_string(rand() % 10000);

        // 如果请求里带了 TaskDesc，就用它；否则构造一个默认的 perf 任务
        hotmethod::TaskDesc taskDesc;
        if (request->has_taskdesc())
        {
            taskDesc.CopyFrom(request->taskdesc());
            taskDesc.set_taskid(taskID);
        }
        else
        {
            // W2 默认：perf 采集 CPU，99Hz，10秒
            taskDesc.set_taskid(taskID);
            taskDesc.set_tasktype(0);     // 通用 CPU
            taskDesc.set_profilertype(0); // perf
            taskDesc.set_timeoutsec(30);
            auto *argv = taskDesc.mutable_sampleargv();
            argv->set_hz(99);
            argv->set_duration(10);
            argv->set_pid(-1); // -1 表示采集整个系统
            argv->set_callgraph("fp");
            argv->set_event("cpu-cycles");
        }

        // 把任务放进对应 IP 的队列
        {
            lock_guard<mutex> lock(tasks_mutex);
            tasks_[request->targetip()].push(taskDesc);
            cout << "[server] 任务入队: taskID=" << taskID
                 << " targetIP=" << request->targetip()
                 << " 队列长度=" << tasks_[request->targetip()].size() << endl;
        }

        response->set_taskid(taskID);
        response->set_code(0); // 0 = 成功
        response->set_msg("ok");

        return Status::OK;
    }

    // FetchData：获取某任务的结果数据
    Status FetchData(ServerContext *context,
                     const control::FetchDataRequest *request,
                     control::FetchDataResponse *response) override
    {
        response->set_code(0);
        return Status::OK;
    }

    // StatAgent：查询某 Agent 当前资源占用
    Status StatAgent(ServerContext *context,
                     const control::StatAgentRequest *request,
                     control::StatAgentResponse *response) override
    {
        response->set_code(0);
        return Status::OK;
    }
};

// ============================================================
// 第4个服务：InitAgentInfo（Agent 初始化）— W4 增强版
// ============================================================
class InitAgentInfoServiceImpl final : public initpb::InitAgentInfo::Service
{
    Status RegisterAgent(ServerContext *context,
                         const initpb::RegisterAgentRequest *request,
                         initpb::RegisterAgentResponse *response) override
    {
        cout << "[server] Agent 注册: host=" << request->hostname()
             << " ip=" << request->ipaddr()
             << " uid=" << request->uid()
             << " version=" << request->agentversion() << endl;
        response->set_code(0);

        // W4: 初始化 Agent 信息
        {
            lock_guard<mutex> lock(agents_mutex);
            AgentInfo &info = agents_[request->uid()];
            info.hostname = request->hostname();
            info.ipAddr = request->ipaddr();
            info.uid = request->uid();
            info.version = request->agentversion();
            info.online = true;
            info.lastHeartbeat = chrono::steady_clock::now();
        }
        return Status::OK;
    }

    // W4: FetchConfig — 返回 MinIO/COS 配置给 Agent
    Status FetchConfig(ServerContext *context,
                       const initpb::FetchConfigRequest *request,
                       initpb::FetchConfigResponse *response) override
    {
        cout << "[server] FetchConfig: uid=" << request->uid() << endl;
        response->set_code(0);

        // 返回默认 MinIO 配置
        auto *cosCfg = response->mutable_cosconfig();
        cosCfg->set_endpoint("minio:9000");
        cosCfg->set_accesskeyid("drop");
        cosCfg->set_secretaccesskey("dropdrop");
        cosCfg->set_bucket("drop");
        cosCfg->set_usessl(false);
        cosCfg->set_region("us-east-1");

        cout << "[server] 下发 COS 配置: endpoint=" << cosCfg->endpoint()
             << " bucket=" << cosCfg->bucket() << endl;
        return Status::OK;
    }
};

// ============================================================
// main：启动所有服务 — W4 增强版
// ============================================================
int main(int argc, char **argv)
{
    // 监听的地址和端口（0.0.0.0 表示接受任何 IP 的连接）
    string server_address("0.0.0.0:50051");

    // 创建 4 个服务的实例
    HealthCheckServiceImpl healthcheck_service;
    HotmethodServiceImpl hotmethod_service;
    ControlServiceImpl control_service;
    InitAgentInfoServiceImpl init_service;

    // 启用 gRPC 反射（让 grpcurl 能自动发现服务和方法）
    grpc::reflection::InitProtoReflectionServerBuilderPlugin();

    // 用 Builder 模式构建服务器
    ServerBuilder builder;
    builder.AddListeningPort(server_address, grpc::InsecureServerCredentials());
    builder.RegisterService(&healthcheck_service);
    builder.RegisterService(&hotmethod_service);
    builder.RegisterService(&control_service);
    builder.RegisterService(&init_service);

    // 启动服务器（BuildAndStart 返回一个智能指针）
    unique_ptr<Server> server(builder.BuildAndStart());
    cout << "[server] drop_server 启动，监听: " << server_address << endl;

    // 信号处理：Ctrl+C 或 kill 时优雅退出
    signal(SIGINT, [](int)
           { server_running = false; });
    signal(SIGTERM, [](int)
           { server_running = false; });

    // W4: 启动 Agent 离线检测线程（每 10 秒扫描一次）
    thread offline_checker([]()
                           {
        while (server_running) {
            this_thread::sleep_for(chrono::seconds(10));
            auto now = chrono::steady_clock::now();
            lock_guard<mutex> lock(agents_mutex);
            for (auto &pair : agents_) {
                AgentInfo &info = pair.second;
                auto elapsed = chrono::duration_cast<chrono::seconds>(now - info.lastHeartbeat).count();
                if (info.online && elapsed > AGENT_TIMEOUT_SEC) {
                    info.online = false;
                    cout << "[server] ⚠️  Agent 离线: uid=" << info.uid
                         << " host=" << info.hostname
                         << " ip=" << info.ipAddr
                         << " 最后心跳: " << elapsed << "秒前" << endl;
                }
            }
        } });

    // W2 自测模式：设置环境变量 W2_TEST=1 启动，会自动创建一个 demo 任务
    if (getenv("W2_TEST"))
    {
        thread test_thread([&]()
                           {
            this_thread::sleep_for(std::chrono::seconds(3));
            cout << "[server] ==== W2 自测：自动创建 demo 任务 ====" << endl;

            hotmethod::TaskDesc taskDesc;
            string taskID = "w2-demo-" + to_string(std::chrono::system_clock::now().time_since_epoch().count());
            taskDesc.set_taskid(taskID);
            taskDesc.set_tasktype(0);
            taskDesc.set_profilertype(0);
            taskDesc.set_timeoutsec(30);
            auto *argv = taskDesc.mutable_sampleargv();
            argv->set_hz(99);
            argv->set_duration(10);
            argv->set_pid(-1);
            argv->set_callgraph("fp");
            argv->set_event("cpu-cycles");

            {
                lock_guard<mutex> lock(tasks_mutex);
                tasks_["127.0.0.1"].push(taskDesc);
            }
            cout << "[server] demo 任务已入队: taskID=" << taskID
                 << " targetIP=127.0.0.1" << endl; });
        test_thread.detach();
    }

    // 等待退出信号（替代 server->Wait() 的永久阻塞）
    while (server_running)
    {
        this_thread::sleep_for(std::chrono::milliseconds(500));
    }

    cout << "[server] 收到退出信号，正在关闭..." << endl;
    offline_checker.join();
    server->Shutdown();
    cout << "[server] 已关闭" << endl;
    return 0;
}
