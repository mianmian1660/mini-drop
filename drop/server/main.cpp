// ============================================================
// drop_server (Server 主程序) — 精简入口
// ============================================================
// 启动 4 个 gRPC 服务 + Agent 离线检测线程
// 各服务实现分散在 server/*Service.cpp 中
// ============================================================

#include "server/HealthCheckService.h"
#include "server/HotmethodService.h"
#include "server/ControlService.h"
#include "server/InitAgentInfoService.h"
#include "server/AgentInfo.h"
#include "server/TaskQueue.h"

#include <iostream>
#include <thread>
#include <chrono>
#include <memory>
#include <mutex>
#include <cstdlib>
#include <csignal>
#include <atomic>

#include <grpcpp/grpcpp.h>
#include <grpcpp/ext/proto_server_reflection_plugin.h>

using namespace std;
using namespace drop_server;

static atomic<bool> server_running{true};

// A3: 任务队列快照路径，可用 DROP_TASK_QUEUE_SNAPSHOT 覆盖。
// 注意：默认写在容器内 /tmp，只在"进程重启、容器不重建"时生效；
// 要跨 `docker compose down/up` 持久化，需要给这个路径挂一个 volume（docker-compose.yml 未改动，留给后续统一调整）。
static string task_queue_snapshot_path()
{
    const char *v = getenv("DROP_TASK_QUEUE_SNAPSHOT");
    if (v && *v)
        return string(v);
    return "/tmp/drop_server_task_queue.bin";
}

int main(int argc, char **argv)
{
    string server_address("0.0.0.0:50051");

    // A3: 启动时从磁盘快照恢复未派发的任务队列（新复刻指南 5.13 节验收项）
    string snapshotPath = task_queue_snapshot_path();
    restore_tasks_from_disk(snapshotPath);

    HealthCheckServiceImpl healthcheck_service;
    HotmethodServiceImpl hotmethod_service;
    ControlServiceImpl control_service;
    InitAgentInfoServiceImpl init_service;

    grpc::reflection::InitProtoReflectionServerBuilderPlugin();

    grpc::ServerBuilder builder;
    builder.AddListeningPort(server_address, grpc::InsecureServerCredentials());
    builder.RegisterService(&healthcheck_service);
    builder.RegisterService(&hotmethod_service);
    builder.RegisterService(&control_service);
    builder.RegisterService(&init_service);

    unique_ptr<grpc::Server> server(builder.BuildAndStart());
    cout << "[server] drop_server 启动，监听: " << server_address << endl;

    signal(SIGINT, [](int)
           { server_running = false; });
    signal(SIGTERM, [](int)
           { server_running = false; });

    // Agent 离线检测线程（每 10 秒扫描）
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

    // A3: 定期把任务队列快照落盘（每 5 秒），供 Server 重启后恢复
    thread snapshot_thread([snapshotPath]()
                           {
        while (server_running) {
            this_thread::sleep_for(chrono::seconds(5));
            snapshot_tasks_to_disk(snapshotPath);
        } });

    // W2 自测模式
    if (getenv("W2_TEST"))
    {
        thread test_thread([]()
                           {
            this_thread::sleep_for(chrono::seconds(3));
            cout << "[server] ==== W2 自测：自动创建 demo 任务 ====" << endl;

            hotmethod::TaskDesc taskDesc;
            string taskID = "w2-demo-" + to_string(chrono::system_clock::now().time_since_epoch().count());
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

    while (server_running)
        this_thread::sleep_for(chrono::milliseconds(500));

    cout << "[server] 收到退出信号，正在关闭..." << endl;
    offline_checker.join();
    snapshot_thread.join();
    snapshot_tasks_to_disk(snapshotPath); // 退出前做最后一次快照，避免丢失最近 5 秒内的变化
    server->Shutdown();
    cout << "[server] 已关闭" << endl;
    return 0;
}
