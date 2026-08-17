#include "agent/ServerChannelHolder.h"

namespace drop_agent
{

    void ServerChannelHolder::Update(std::shared_ptr<grpc::Channel> channel,
                                      std::shared_ptr<hotmethod::Hotmethod::Stub> hotmethodStub,
                                      const common::CosConfig &cosConfig)
    {
        std::lock_guard<std::mutex> lock(mutex_);
        channel_ = std::move(channel);
        hotmethodStub_ = std::move(hotmethodStub);
        cosConfig_ = cosConfig;
    }

    std::shared_ptr<hotmethod::Hotmethod::Stub> ServerChannelHolder::HotmethodStub() const
    {
        std::lock_guard<std::mutex> lock(mutex_);
        return hotmethodStub_;
    }

    common::CosConfig ServerChannelHolder::GetCosConfig() const
    {
        std::lock_guard<std::mutex> lock(mutex_);
        return cosConfig_;
    }

} // namespace drop_agent
