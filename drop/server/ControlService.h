// ============================================================
// server/ControlService.h — 控制服务 声明
// ============================================================

#pragma once

#include "common/proto/control.grpc.pb.h"

namespace drop_server
{

    class ControlServiceImpl final : public control::Control::Service
    {
        grpc::Status CreateTask(grpc::ServerContext *context,
                                const control::CreateTaskRequest *request,
                                control::CreateTaskResponse *response) override;

        grpc::Status FetchData(grpc::ServerContext *context,
                               const control::FetchDataRequest *request,
                               control::FetchDataResponse *response) override;

        grpc::Status StatAgent(grpc::ServerContext *context,
                               const control::StatAgentRequest *request,
                               control::StatAgentResponse *response) override;
    };

} // namespace drop_server
