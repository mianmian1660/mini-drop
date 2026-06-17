// ============================================================
// common/COSClient.h — MinIO/COS 对象存储上传 声明
// ============================================================
// 使用 curl 命令行通过 S3 兼容 API 上传文件
// 支持连接超时、认证
// ============================================================

#pragma once

#include <string>
#include "common/proto/common.pb.h" // common::CosConfig

namespace drop
{

    /// 上传文件到 MinIO/COS（S3 兼容 API）
    /// @param cosConfig 存储配置（endpoint, accessKey, secretKey, bucket）
    /// @param localPath 本地文件路径
    /// @param remoteKey 远程对象 key
    /// @return 上传成功返回 true
    bool upload_to_minio(const common::CosConfig &cosConfig,
                         const std::string &localPath,
                         const std::string &remoteKey);

} // namespace drop
