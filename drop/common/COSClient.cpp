// ============================================================
// common/COSClient.cpp — MinIO/COS 对象存储上传 实现
// ============================================================
// 使用 MinIO Client (mc) 而非 curl，支持 AWS Signature V4 认证
// mc 需要在 Docker 镜像中预装：wget -q https://dl.min.io/client/mc/...
// ============================================================

#include "common/COSClient.h"
#include "common/Utils.h" // trim, shell_exec

#include <iostream>
#include <string>
#include <cstdlib>

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

        // 1. 配置 mc alias（一次即可，后续复用）
        string aliasCmd = "mc alias set " + aliasName +
                          " http://" + endpoint +
                          " " + cosConfig.accesskeyid() +
                          " " + cosConfig.secretaccesskey() +
                          " 2>&1";
        cout << "[cos] 配置 mc alias: " << aliasName << " -> http://" << endpoint << endl;
        string aliasResult = shell_exec(aliasCmd);
        if (!aliasResult.empty())
            cout << "[cos] mc alias: " << aliasResult << endl;

        // 2. 上传文件
        string remotePath = aliasName + string("/") + bucket + "/" + remoteKey;
        string cpCmd = "mc cp --quiet \"" + localPath + "\" \"" + remotePath + "\" 2>&1";
        cout << "[cos] 上传: " << localPath << " -> " << remotePath << endl;

        string cpResult = shell_exec(cpCmd);
        if (!cpResult.empty())
        {
            cerr << "[cos] 上传警告: " << cpResult << endl;
        }

        // 3. 验证上传（检查文件是否存在）
        string statCmd = "mc stat \"" + remotePath + "\" 2>&1 >/dev/null";
        int statRet = system(statCmd.c_str());
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
