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

    grpc::Status HealthCheckServiceImpl::Do(
        grpc::ServerContext * /*context*/,
        const healthcheck::HealthCheckRequest *request,
        healthcheck::HealthCheckResponse *response)
    {
        string uid = request->uid();
        auto now = chrono::steady_clock::now();

        // 更新 Agent 信息
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

        // 回复 Agent，有任务就派发
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
                    tasks_.erase(it);
            }
        }

        return grpc::Status::OK;
    }

} // namespace drop_server
