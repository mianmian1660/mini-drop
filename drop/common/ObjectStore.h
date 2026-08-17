// ============================================================
// common/ObjectStore.h — 对象存储上传抽象
// ============================================================
// 指南 5.8 节要求 TaskContext 暴露 ObjectStore。当前 4 个内置 Runner
// 的上传统一走 Upload Worker（不经过这个接口），这里预留给未来需要
// 流式/分片上传的 Runner 直接使用；同时便于依赖注入做单元测试，
// 不需要真的连 MinIO。
// ============================================================

#pragma once

#include "common/proto/common.pb.h" // common::CosConfig

#include <string>

namespace drop
{

    class ObjectStore
    {
    public:
        virtual ~ObjectStore() = default;
        virtual bool Put(const common::CosConfig &cosConfig,
                          const std::string &localPath,
                          const std::string &remoteKey) = 0;
    };

    /// 生产实现：包装已有的 drop::upload_to_minio()（带重试，见 COSClient.cpp）。
    class MinioObjectStore : public ObjectStore
    {
    public:
        bool Put(const common::CosConfig &cosConfig,
                 const std::string &localPath,
                 const std::string &remoteKey) override;
    };

} // namespace drop
