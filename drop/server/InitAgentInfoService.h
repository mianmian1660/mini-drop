// ============================================================
// server/InitAgentInfoService.h — Agent 初始化服务 声明
// ============================================================

#pragma once

#include "common/proto/init.grpc.pb.h"

namespace drop_server
{

    class InitAgentInfoServiceImpl final : public initpb::InitAgentInfo::Service
    {
        grpc::Status RegisterAgent(grpc::ServerContext *context,
                                   const initpb::RegisterAgentRequest *request,
                                   initpb::RegisterAgentResponse *response) override;

        grpc::Status FetchConfig(grpc::ServerContext *context,
                                 const initpb::FetchConfigRequest *request,
                                 initpb::FetchConfigResponse *response) override;
    };

} // namespace drop_server
