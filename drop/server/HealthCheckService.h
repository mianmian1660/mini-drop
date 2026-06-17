// ============================================================
// server/HealthCheckService.h — 心跳服务 声明
// ============================================================

#pragma once

#include "common/proto/healthcheck.grpc.pb.h"

namespace drop_server
{

    class HealthCheckServiceImpl final : public healthcheck::HealthCheck::Service
    {
        grpc::Status Do(grpc::ServerContext *context,
                        const healthcheck::HealthCheckRequest *request,
                        healthcheck::HealthCheckResponse *response) override;
    };

} // namespace drop_server
