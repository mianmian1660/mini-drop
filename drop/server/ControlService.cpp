// ============================================================
// server/ControlService.cpp — 控制服务 实现
// ============================================================

#include "server/ControlService.h"
#include "server/TaskQueue.h"
#include "server/AgentInfo.h"
#include "common/Log.h"

#include <iostream>
#include <string>
#include <chrono>
#include <mutex>

using namespace std;

namespace drop_server
{

    grpc::Status ControlServiceImpl::CreateTask(
        grpc::ServerContext * /*context*/,
        const control::CreateTaskRequest *request,
        control::CreateTaskResponse *response)
    {
        cout << "[server] CreateTask: targetIP=" << request->targetip() << endl;

        hotmethod::TaskDesc taskDesc;
        string taskID;

        if (request->has_taskdesc())
        {
            // 使用 apiserver 下发的 TID，保证端到端 TID 一致
            taskDesc.CopyFrom(request->taskdesc());
            taskID = taskDesc.taskid();
            cout << "[server]   使用 apiserver TID: " << taskID << endl;
        }
        else
        {
            // 无 TaskDesc：生成默认任务（自测/调试用）
            taskID = "task-" + to_string(chrono::system_clock::now().time_since_epoch().count()) +
                     "-" + to_string(rand() % 10000);
            taskDesc.set_taskid(taskID);
            taskDesc.set_tasktype(0);
            taskDesc.set_profilertype(0);
            taskDesc.set_timeoutsec(30);
            auto *argv = taskDesc.mutable_sampleargv();
            argv->set_hz(99);
            argv->set_duration(10);
            argv->set_pid(-1);
            argv->set_callgraph("fp");
            argv->set_event("cpu-clock");
            cout << "[server]   生成默认 TID: " << taskID << endl;
        }

        EnqueueResult enqueueResult;
        size_t queueSize = 0;
        {
            lock_guard<mutex> lock(tasks_mutex);
            enqueueResult = enqueue_task_locked(request->targetip(), taskDesc);
            auto it = tasks_.find(request->targetip());
            if (it != tasks_.end())
                queueSize = it->second.size();
        }
        if (!enqueueResult.ok)
        {
            response->set_taskid(taskID);
            response->set_code(429);
            response->set_msg(enqueueResult.message);
            cout << "[server] 任务拒绝: taskID=" << taskID
                 << " targetIP=" << request->targetip()
                 << " reason=" << enqueueResult.message << endl;
            drop::log_event("drop_server", "task_enqueue_rejected",
                             {{"task_id", taskID}, {"target_ip", request->targetip()}, {"reason", enqueueResult.message}});
            return grpc::Status::OK;
        }

        cout << "[server] 任务入队: taskID=" << taskID
             << " attemptID=" << taskDesc.attempt_id()
             << " targetIP=" << request->targetip()
             << " duplicate=" << (enqueueResult.duplicate ? "true" : "false")
             << " 队列长度=" << queueSize << endl;
        drop::log_event("drop_server", "task_enqueued",
                         {{"task_id", taskID},
                          {"target_ip", request->targetip()},
                          {"duplicate", enqueueResult.duplicate ? "true" : "false"},
                          {"queue_size", to_string(queueSize)}});

        response->set_taskid(taskID);
        response->set_code(0);
        response->set_msg("ok");

        return grpc::Status::OK;
    }

    grpc::Status ControlServiceImpl::FetchData(
        grpc::ServerContext * /*context*/,
        const control::FetchDataRequest * /*request*/,
        control::FetchDataResponse *response)
    {
        // 预留但当前未启用：apiserver 侧没有任何真实代码路径调用这个 RPC
        // （只有 proto 生成的桩代码和测试用的假实现），任务结果的获取
        // 走的是 ResultNotifier 主动回调 + Artifact 下载，不经过这里。
        response->set_code(0);
        return grpc::Status::OK;
    }

    grpc::Status ControlServiceImpl::StatAgent(
        grpc::ServerContext * /*context*/,
        const control::StatAgentRequest *request,
        control::StatAgentResponse *response)
    {
        string targetIP = request->targetip();

        // 在 agents_ 中查找该 IP 对应的 Agent (优先匹配 online 和最新心跳的记录)
        {
            lock_guard<mutex> lock(agents_mutex);
            const AgentInfo *bestMatch = nullptr;
            for (const auto &pair : agents_)
            {
                const AgentInfo &info = pair.second;
                if (info.ipAddr == targetIP)
                {
                    if (!bestMatch || (!bestMatch->online && info.online) ||
                        (bestMatch->online == info.online && info.lastSeenUnixMs > bestMatch->lastSeenUnixMs))
                    {
                        bestMatch = &info;
                    }
                }
            }

            if (bestMatch)
            {
                const AgentInfo &info = *bestMatch;
                response->set_code(0);
                response->set_msg("ok");
                response->set_cpupercent(info.lastSelfPstats.cpupercent());
                response->set_memorykb(info.lastSelfPstats.rsskb());
                response->set_readkbpers(info.lastSelfPstats.readkbpers());
                response->set_writekbpers(info.lastSelfPstats.writekbpers());
                response->set_agent_id(info.agentID);
                response->set_hostname(info.hostname);
                response->set_version(info.version);
                response->set_platform(info.platform);
                response->set_resource_budget(info.resourceBudget);
                response->set_online(info.online);
                response->set_last_seen_unix_ms(info.lastSeenUnixMs);
                for (const auto &capability : info.capabilities)
                    response->add_capabilities(capability);
                for (const auto &label : info.labels)
                    response->add_labels(label);

                cout << "[server] StatAgent: ip=" << targetIP
                     << " host=" << info.hostname
                     << " online=" << info.online
                     << " cpu=" << info.lastSelfPstats.cpupercent() << "%" << endl;
                return grpc::Status::OK;
            }
        }

        // 未找到该 IP
        response->set_code(404);
        response->set_msg("agent not found: " + targetIP);
        return grpc::Status::OK;
    }

    grpc::Status ControlServiceImpl::CancelTask(
        grpc::ServerContext * /*context*/,
        const control::CancelTaskRequest *request,
        control::CancelTaskResponse *response)
    {
        bool queuedCanceled = false;
        bool alreadyTerminal = false;
        bool dispatchedCancelRequested = false;
        {
            lock_guard<mutex> lock(tasks_mutex);
            queuedCanceled = cancel_task_locked(request->targetip(), request->taskid(), request->attempt_id());
            hotmethod::TaskDesc probe;
            probe.set_taskid(request->taskid());
            probe.set_attempt_id(request->attempt_id());
            alreadyTerminal = is_attempt_completed_locked(probe);

            // Phase 7：还没在队列里找到（说明已经派发给 Agent 了），且不是
            // 已经跑完的老任务——登记进 pending_cancels_，下次心跳
            // HealthCheckService::Do 会把它下发给 Agent，由 Agent 侧触发
            // Runner::Stop(kCancel)。没有 targetIP 就没法知道该通知哪个
            // Agent，跳过。
            if (!queuedCanceled && !alreadyTerminal && !request->targetip().empty())
            {
                request_cancel_locked(request->targetip(), request->taskid(), request->attempt_id());
                dispatchedCancelRequested = true;
            }
        }

        bool canceled = queuedCanceled || dispatchedCancelRequested;
        response->set_code(0);
        if (alreadyTerminal)
            response->set_msg("attempt already terminal");
        else if (queuedCanceled)
            response->set_msg("queued task canceled");
        else if (dispatchedCancelRequested)
            response->set_msg("cancel request relayed to agent, will take effect within grace period on next heartbeat");
        else
            response->set_msg("task not found");
        response->set_canceled(canceled);
        response->set_already_terminal(alreadyTerminal);

        cout << "[server] CancelTask: taskID=" << request->taskid()
             << " attemptID=" << request->attempt_id()
             << " targetIP=" << request->targetip()
             << " queuedCanceled=" << (queuedCanceled ? "true" : "false")
             << " dispatchedCancelRequested=" << (dispatchedCancelRequested ? "true" : "false")
             << " alreadyTerminal=" << (alreadyTerminal ? "true" : "false") << endl;
        return grpc::Status::OK;
    }

} // namespace drop_server
