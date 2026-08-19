// ============================================================
// agent/ServerChannelHolder.h — 当前 Server 连接快照（线程安全）
// ============================================================
// HeartbeatThread 在故障转移时会切换到备用 Server（新的 channel/stub/
// CosConfig）；UploadWorker（Phase 4 起）上传产物、调用 NotifyResult 时
// 需要读到"当下有效"的那一份，而不是启动时的旧快照。二者不共享同一个
// stub 对象，只通过这个持锁的 holder 交换只读快照，避免裸指针跨线程
// 写读的竞态。
// ============================================================

#pragma once

#include <memory>
#include <mutex>
#include <grpcpp/grpcpp.h>
#include "common/proto/hotmethod.grpc.pb.h"
#include "common/proto/common.pb.h" // common::CosConfig

namespace drop_agent
{

    class ServerChannelHolder
    {
    public:
        void Update(std::shared_ptr<grpc::Channel> channel,
                    std::shared_ptr<hotmethod::Hotmethod::Stub> hotmethodStub,
                    const common::CosConfig &cosConfig);

        std::shared_ptr<hotmethod::Hotmethod::Stub> HotmethodStub() const;
        common::CosConfig GetCosConfig() const;

    private:
        mutable std::mutex mutex_;
        std::shared_ptr<grpc::Channel> channel_;
        std::shared_ptr<hotmethod::Hotmethod::Stub> hotmethodStub_;
        common::CosConfig cosConfig_;
    };

} // namespace drop_agent
