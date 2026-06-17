// ============================================================
// server/HotmethodService.h — 任务结果回报服务 声明
// ============================================================

#pragma once

#include "common/proto/hotmethod.grpc.pb.h"

namespace drop_server
{

    class HotmethodServiceImpl final : public hotmethod::Hotmethod::Service
    {
        grpc::Status Collect(grpc::ServerContext *context,
                             const hotmethod::Target *request,
                             google::protobuf::Empty *response) override;

        grpc::Status NotifyResult(grpc::ServerContext *context,
                                  const hotmethod::TaskResult *request,
                                  google::protobuf::Empty *response) override;
    };

} // namespace drop_server
