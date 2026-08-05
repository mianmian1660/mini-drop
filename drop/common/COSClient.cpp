// ============================================================
// common/COSClient.cpp — MinIO/COS 对象存储上传 实现
// ============================================================
// 使用 MinIO Client (mc) 而非 curl，支持 AWS Signature V4 认证
// mc 需要在 Docker 镜像中预装：wget -q https://dl.min.io/client/mc/...
// ============================================================

#include "common/COSClient.h"
#include "common/Utils.h" // exec_capture

#include <iostream>
#include <string>

using namespace std;

namespace drop
{

    bool upload_to_minio(const common::CosConfig &cosConfig,
                         const string &localPath,
                         const string &remoteKey)
    {
        string endpoint = cosConfig.endpoint();
        if (endpoint.empty())
            endpoint = "localhost:9000";
        string bucket = cosConfig.bucket();
        if (bucket.empty())
            bucket = "drop-data";

        string aliasName = "myminio";

        cout << "[cos] 配置 mc alias: " << aliasName << " -> http://" << endpoint << endl;
        string aliasResult;
        int aliasRet = exec_capture({"mc", "alias", "set", aliasName,
                                     "http://" + endpoint,
                                     cosConfig.accesskeyid(),
                                     cosConfig.secretaccesskey()},
                                    &aliasResult);
        if (!aliasResult.empty())
            cout << "[cos] mc alias: " << aliasResult << endl;
        if (aliasRet != 0)
        {
            cerr << "[cos] 配置 mc alias 失败, exitCode=" << aliasRet << endl;
            return false;
        }

        // 2. 上传文件
        string remotePath = aliasName + string("/") + bucket + "/" + remoteKey;
        cout << "[cos] 上传: " << localPath << " -> " << remotePath << endl;

        string cpResult;
        int cpRet = exec_capture({"mc", "cp", "--quiet", localPath, remotePath}, &cpResult);
        if (!cpResult.empty())
        {
            cerr << "[cos] 上传警告: " << cpResult << endl;
        }
        if (cpRet != 0)
        {
            cerr << "[cos] 上传失败, exitCode=" << cpRet << " key=" << remoteKey << endl;
            return false;
        }

        // 3. 验证上传（检查文件是否存在）
        string statResult;
        int statRet = exec_capture({"mc", "stat", remotePath}, &statResult);
        bool success = (statRet == 0);

        if (success)
        {
            cout << "[cos] 上传成功! key=" << remoteKey << endl;
        }
        else
        {
            cerr << "[cos] 上传失败! key=" << remoteKey << endl;
        }

        return success;
    }

} // namespace drop
