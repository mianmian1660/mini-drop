// ============================================================
// drop_agent (Agent 主程序) — 精简入口
// ============================================================
// Phase 3/4 起，Agent 内部拆成三个线程，通过两条队列解耦（新复刻指南
// 5.7 节）：
//   agent/HeartbeatThread.h/cpp — 心跳、故障转移、把 Server 派发的任务
//                                  塞进 TaskQueue
//   agent/WorkerThread.h/cpp    — 消费 TaskQueue，跑 Runner 全生命周期
//                                  （Validate/Prepare/Start/Poll/Collect），
//                                  把结果丢进 UploadQueue
//   agent/UploadWorker.h/cpp    — 消费 UploadQueue，上传产物 + NotifyResult，
//                                  和采集节奏解耦，慢 IO 不拖慢下一个任务
//   agent/TaskQueue.h/cpp / agent/UploadQueue.h/cpp — 线程间任务缓冲（mutex+condvar）
//   agent/AttemptTracker.h/cpp      — running/completed 快照（线程安全）
//   agent/ServerChannelHolder.h/cpp — 当前 Server 连接快照（线程安全，
//                                      供 UploadWorker 上传/NotifyResult 用）
//
// main() 现在只做：加载配置 → 首次注册（失败即退出）→ 组装上面几个对象 →
// 启动三个线程 → 等待退出信号 → 按 Heartbeat→Worker→UploadWorker 顺序收尾。
//
// 采集器实现（drop_agent::Runner 生命周期接口）仍是：
//   agent/Runner.h / RunnerRegistry.h / RunnerUtils.h / runners/*Runner.cpp
// ============================================================

#include "agent/Config.h"
#include "agent/AgentUtils.h"
#include "agent/TaskQueue.h"
#include "agent/UploadQueue.h"
#include "agent/AttemptTracker.h"
#include "agent/ServerChannelHolder.h"
#include "agent/HeartbeatThread.h"
#include "agent/WorkerThread.h"
#include "agent/UploadWorker.h"
#include "common/CapabilityDetector.h" // drop::detect_capabilities

#include <iostream>
#include <string>
#include <thread>
#include <chrono>
#include <atomic>
#include <csignal>

#include <grpcpp/grpcpp.h>
#include "common/proto/hotmethod.grpc.pb.h"

using namespace std;
using namespace std::chrono;

static atomic<bool> agent_running{true};

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
    if (drop_agent::FileExists(configPath))
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

    auto capabilityReport = drop::detect_capabilities();
    cfg.capabilities = drop::merge_capabilities(cfg.capabilities, capabilityReport.capabilities);

    cout << "[agent] drop_agent 启动" << endl;
    cout << "[agent] hostname=" << cfg.hostname
         << " ip=" << cfg.ipAddr
         << " uid=" << cfg.uid
         << " version=" << cfg.agentVersion << endl;
    cout << "[agent] Native CP capabilities: perf_event="
         << (capabilityReport.perfEventReadable ? "yes" : "no")
         << " perf=" << (capabilityReport.perfCommand ? "yes" : "no")
         << " bpftrace=" << (capabilityReport.bpftraceCommand ? "yes" : "no")
         << " btf=" << (capabilityReport.btf ? "yes" : "no")
         << " ebpf_fs=" << (capabilityReport.ebpfFS ? "yes" : "no")
         << " tracefs=" << (capabilityReport.traceFS ? "yes" : "no")
         << " block_tracepoint=" << (capabilityReport.blockTracepoint ? "yes" : "no")
         << " sched_tracepoint=" << (capabilityReport.schedTracepoint ? "yes" : "no")
         << " memlock_unlimited=" << (capabilityReport.memlockUnlimited ? "yes" : "no")
         << " perf_event_paranoid=" << capabilityReport.perfEventParanoid << endl;

    // ---------- 2. 首次连接 Server（失败即退出，和旧行为一致）----------
    common::CosConfig cosConfig;
    bool registered = false;
    auto channel = drop_agent::ConnectToServer(cfg, cosConfig, registered);

    if (!channel || !registered)
    {
        cerr << "[agent] 所有 Server 均不可用，退出" << endl;
        return 1;
    }

    auto hotmethodStub = hotmethod::Hotmethod::NewStub(channel);

    // ---------- 3. 组装线程间共享状态 ----------
    drop_agent::TaskQueue taskQueue;
    drop_agent::UploadQueue uploadQueue;
    drop_agent::AttemptTracker attemptTracker;
    drop_agent::ServerChannelHolder channelHolder;
    // NewStub 返回 unique_ptr，Update 期望 shared_ptr；hotmethodStub 之后不再
    // 被 main.cpp 直接使用（Heartbeat/UploadWorker 都通过 channelHolder 取
    // stub），所以这里 std::move 转移所有权即可。
    channelHolder.Update(channel, std::move(hotmethodStub), cosConfig);

    drop_agent::WorkerThread worker(taskQueue, uploadQueue, agent_running);
    drop_agent::UploadWorker uploader(uploadQueue, attemptTracker, channelHolder);
    drop_agent::HeartbeatThread heartbeat(cfg, channel, cosConfig, taskQueue, attemptTracker, channelHolder, agent_running);

    // ---------- 4. 启动 Heartbeat / Worker / UploadWorker 线程 ----------
    uploader.Start();
    worker.Start();
    heartbeat.Start();

    while (agent_running)
        this_thread::sleep_for(milliseconds(200));

    // ---------- 5. 优雅关闭：Heartbeat 先停（不再有新任务入队）→
    //              Worker 再停（跑完/中断当前任务后退出循环，把结果丢进
    //              UploadQueue）→ UploadWorker 最后停（排空 UploadQueue，
    //              确保"采集刚结束还在排队等上传"的任务不被丢弃）----------
    cout << "[agent] 收到退出信号，正在关闭..." << endl;
    heartbeat.Stop();
    worker.Stop();
    uploader.Stop();
    return 0;
}
