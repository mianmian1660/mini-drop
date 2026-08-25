// ============================================================
// server/HealthCheckService.cpp — 心跳服务 实现
// ============================================================

#include "server/HealthCheckService.h"
#include "server/AgentInfo.h"
#include "server/TaskQueue.h"

#include <iostream>
#include <mutex>

using namespace std;

namespace drop_server
{
    static int64_t system_now_unix_ms()
    {
        return chrono::duration_cast<chrono::milliseconds>(
                   chrono::system_clock::now().time_since_epoch())
            .count();
    }

    grpc::Status HealthCheckServiceImpl::Do(
        grpc::ServerContext * /*context*/,
        const healthcheck::HealthCheckRequest *request,
        healthcheck::HealthCheckResponse *response)
    {
        string uid = request->uid();
        string stableID = request->agent_id().empty() ? uid : request->agent_id();
        auto now = chrono::steady_clock::now();

        // 更新 Agent 信息
        {
            lock_guard<mutex> lock(agents_mutex);
            AgentInfo &info = agents_[stableID];
            bool wasOffline = !info.online;

            info.hostname = request->hostname();
            info.ipAddr = request->ipaddr();
            info.uid = uid;
            info.agentID = stableID;
            info.version = request->agentversion();
            info.platform = request->platform();
            info.capabilities.assign(request->capabilities().begin(), request->capabilities().end());
            info.labels.assign(request->labels().begin(), request->labels().end());
            info.resourceBudget = request->resource_budget();
            info.online = true;
            info.lastHeartbeat = now;
            info.lastSeenUnixMs = system_now_unix_ms();
            if (request->has_selfpstats())
            {
                info.lastSelfPstats.CopyFrom(request->selfpstats());
            }
            // 整机资源（可选字段）：旧 Agent 不携带时保持 hasHostStats=false
            if (request->has_host_stats())
            {
                info.lastHostStats.CopyFrom(request->host_stats());
                info.hasHostStats = true;
            }
            // 主机身份与系统信息（可选字段）：旧 Agent 不携带时保持 hasHostMetadata=false
            if (request->has_host_metadata())
            {
                info.lastHostMetadata.CopyFrom(request->host_metadata());
                info.hasHostMetadata = true;
            }
            info.lastChildrenPstats.clear();
            if (request->has_childrenpstats())
            {
                info.lastChildrenPstats.push_back(request->childrenpstats());
            }
            info.runningAttempts.assign(request->running_attempts().begin(), request->running_attempts().end());
            info.completedAttempts.assign(request->completed_attempts().begin(), request->completed_attempts().end());

            if (wasOffline)
            {
                cout << "[server] Agent 恢复上线: agent_id=" << stableID
                     << " uid=" << uid
                     << " host=" << request->hostname()
                     << " ip=" << request->ipaddr() << endl;
            }
        }

        cout << "[server] 收到心跳: host=" << request->hostname()
             << " ip=" << request->ipaddr()
             << " uid=" << uid
             << " agent_id=" << stableID
             << " running_attempts=" << request->running_attempts_size()
             << " completed_attempts=" << request->completed_attempts_size();
        if (request->has_selfpstats())
        {
            cout << " CPU=" << request->selfpstats().cpupercent() << "%"
                 << " RSS=" << request->selfpstats().rsskb() << "KB";
        }
        if (request->has_host_stats())
        {
            const auto &h = request->host_stats();
            cout << " 整机CPU=" << (h.cpu_available() ? h.cpu_percent() : -1.0) << "%"
                 << " 内存=" << (h.memory_available() ? h.memory_percent() : -1.0) << "%"
                 << " 磁盘=" << (h.disk_available() ? h.disk_percent() : -1.0) << "%";
        }
        cout << endl;

        // 回复 Agent，有任务就派发
        response->set_status(healthcheck::HealthCheckResponse::SERVING);
        response->set_pending(false);
        {
            lock_guard<mutex> lock(tasks_mutex);
            for (const auto &attempt : request->completed_attempts())
            {
                hotmethod::TaskResult completed;
                completed.set_taskid(attempt.taskid());
                completed.set_attempt_id(attempt.attempt_id());
                mark_attempt_completed_locked(completed);
            }
            hotmethod::TaskDesc task;
            if (pop_next_dispatchable_task_locked(request->ipaddr(), &task))
            {
                response->set_pending(true);
                response->mutable_taskdesc()->Swap(&task);
            }

            // Phase 7：把这个 Agent 汇报的 running_attempts 转成 key 集合，
            // 和 pending_cancels_ 比对，下发这次心跳要取消的 attempt 列表。
            unordered_set<string> runningKeys;
            for (const auto &attempt : request->running_attempts())
                runningKeys.insert(task_attempt_key(attempt.taskid(), attempt.attempt_id()));
            auto cancelList = collect_cancel_attempts_locked(request->ipaddr(), runningKeys);
            for (const auto &item : cancelList)
            {
                auto *entry = response->add_cancel_attempts();
                entry->set_taskid(item.first);
                entry->set_attempt_id(item.second);
                cout << "[server] 下发取消指令: taskID=" << item.first
                     << " attemptID=" << item.second
                     << " targetIP=" << request->ipaddr() << endl;
            }
        }

        return grpc::Status::OK;
    }

} // namespace drop_server
