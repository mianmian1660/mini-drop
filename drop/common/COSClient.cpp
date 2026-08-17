// ============================================================
// common/COSClient.cpp — MinIO/COS 对象存储上传 实现
// ============================================================
// 使用 MinIO Client (mc) 而非 curl，支持 AWS Signature V4 认证
// mc 需要在 Docker 镜像中预装：wget -q https://dl.min.io/client/mc/...
// ============================================================

#include "common/COSClient.h"
#include "common/Utils.h" // exec_capture
#include "common/Log.h"   // drop::log_event

#include <chrono>
#include <iostream>
#include <string>
#include <thread>

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

        // 2. 上传文件 + 3. 验证上传（检查文件是否存在）
        // 网络/对象存储的瞬时抖动很常见，最多重试 3 次，指数退避 500ms/1s，
        // 避免一次性抖动就把整个采集任务判定为失败。mc alias set 是纯本地
        // 配置写入，不涉及网络，失败了重试也没用，不在重试范围内。
        string remotePath = aliasName + string("/") + bucket + "/" + remoteKey;
        const int kMaxAttempts = 3;

        for (int attempt = 1; attempt <= kMaxAttempts; attempt++)
        {
            cout << "[cos] 上传(第" << attempt << "/" << kMaxAttempts << "次): "
                 << localPath << " -> " << remotePath << endl;

            string cpResult;
            int cpRet = exec_capture({"mc", "cp", "--quiet", localPath, remotePath}, &cpResult);
            if (!cpResult.empty())
                cerr << "[cos] 上传警告: " << cpResult << endl;

            if (cpRet == 0)
            {
                string statResult;
                int statRet = exec_capture({"mc", "stat", remotePath}, &statResult);
                if (statRet == 0)
                {
                    cout << "[cos] 上传成功! key=" << remoteKey
                         << " attempt=" << attempt << endl;
                    drop::log_event("drop_agent", "artifact_upload_retry_result",
                                     {{"key", remoteKey}, {"ok", "true"}, {"attempts", to_string(attempt)}});
                    return true;
                }
                cerr << "[cos] 上传后校验失败(mc stat), key=" << remoteKey << endl;
            }
            else
            {
                cerr << "[cos] 上传失败, exitCode=" << cpRet << " key=" << remoteKey << endl;
            }

            if (attempt < kMaxAttempts)
            {
                auto backoff = chrono::milliseconds(500 * (1 << (attempt - 1))); // 500ms, 1000ms
                cout << "[cos] " << backoff.count() << "ms 后重试上传..." << endl;
                this_thread::sleep_for(backoff);
            }
        }

        cerr << "[cos] 上传最终失败(已重试" << kMaxAttempts << "次), key=" << remoteKey << endl;
        drop::log_event("drop_agent", "artifact_upload_retry_result",
                         {{"key", remoteKey}, {"ok", "false"}, {"attempts", to_string(kMaxAttempts)}});
        return false;
    }

} // namespace drop
