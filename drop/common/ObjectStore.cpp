// ============================================================
// common/ObjectStore.cpp — 实现
// ============================================================

#include "common/ObjectStore.h"
#include "common/COSClient.h" // drop::upload_to_minio

namespace drop
{

    bool MinioObjectStore::Put(const common::CosConfig &cosConfig,
                                const std::string &localPath,
                                const std::string &remoteKey)
    {
        return upload_to_minio(cosConfig, localPath, remoteKey);
    }

} // namespace drop
