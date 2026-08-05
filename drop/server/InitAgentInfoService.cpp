// ============================================================
// server/InitAgentInfoService.cpp — Agent 初始化服务 实现
// ============================================================

#include "server/InitAgentInfoService.h"
#include "server/AgentInfo.h"

#include <iostream>
#include <chrono>
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

    grpc::Status InitAgentInfoServiceImpl::RegisterAgent(
        grpc::ServerContext * /*context*/,
        const initpb::RegisterAgentRequest *request,
        initpb::RegisterAgentResponse *response)
    {
        cout << "[server] Agent 注册: host=" << request->hostname()
             << " ip=" << request->ipaddr()
             << " uid=" << request->uid()
             << " agent_id=" << request->agent_id()
             << " version=" << request->agentversion() << endl;
        response->set_code(0);

        {
            lock_guard<mutex> lock(agents_mutex);
            string stableID = request->agent_id().empty() ? request->uid() : request->agent_id();
            AgentInfo &info = agents_[stableID];
            info.hostname = request->hostname();
            info.ipAddr = request->ipaddr();
            info.uid = request->uid();
            info.agentID = stableID;
            info.version = request->agentversion();
            info.platform = request->platform();
            info.capabilities.assign(request->capabilities().begin(), request->capabilities().end());
            info.labels.assign(request->labels().begin(), request->labels().end());
            info.resourceBudget = request->resource_budget();
            info.online = true;
            info.lastHeartbeat = chrono::steady_clock::now();
            info.lastSeenUnixMs = system_now_unix_ms();
        }
        return grpc::Status::OK;
    }

    grpc::Status InitAgentInfoServiceImpl::FetchConfig(
        grpc::ServerContext * /*context*/,
        const initpb::FetchConfigRequest *request,
        initpb::FetchConfigResponse *response)
    {
        cout << "[server] FetchConfig: uid=" << request->uid() << endl;
        response->set_code(0);

        auto *cosCfg = response->mutable_cosconfig();
        // 使用 localhost 因为 agent 以 network_mode=host 运行
        cosCfg->set_endpoint("localhost:9000");
        cosCfg->set_accesskeyid("drop");
        cosCfg->set_secretaccesskey("dropdrop");
        cosCfg->set_bucket("drop-data");
        cosCfg->set_usessl(false);
        cosCfg->set_region("us-east-1");

        cout << "[server] 下发 COS 配置: endpoint=" << cosCfg->endpoint()
             << " bucket=" << cosCfg->bucket()
             << " insecure_transport=" << (cosCfg->usessl() ? "false" : "true")
             << " credentials=redacted" << endl;
        return grpc::Status::OK;
    }

} // namespace drop_server
