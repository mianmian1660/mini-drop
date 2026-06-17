// ============================================================
// common/COSClient.cpp — MinIO/COS 对象存储上传 实现
// ============================================================

#include "common/COSClient.h"
#include "common/Utils.h" // trim, shell_exec

#include <iostream>
#include <string>
#include <cstdlib> // atoi

using namespace std;

namespace drop
{

    bool upload_to_minio(const common::CosConfig &cosConfig,
                         const string &localPath,
                         const string &remoteKey)
    {
        string scheme = cosConfig.usessl() ? "https://" : "http://";
        string endpoint = cosConfig.endpoint();
        if (endpoint.empty())
            endpoint = "minio:9000";
        string bucket = cosConfig.bucket();
        if (bucket.empty())
            bucket = "drop";

        string url = scheme + endpoint + "/" + bucket + "/" + remoteKey;

        // curl 上传，连接超时 5s，总超时 10s
        string cmd = "curl -s --connect-timeout 5 --max-time 10 -o /dev/null -w '%{http_code}' -X PUT -T \"" +
                     localPath + "\" \"" + url + "\"";

        if (!cosConfig.accesskeyid().empty())
        {
            string user = cosConfig.accesskeyid();
            string pass = cosConfig.secretaccesskey();
            cmd += " -u \"" + user + ":" + pass + "\"";
        }

        cout << "[cos] 上传: " << url << " (key=" << remoteKey << ")" << endl;

        string result = shell_exec(cmd, 128);
        result = trim(result);

        int httpCode = atoi(result.c_str());
        if (httpCode >= 200 && httpCode < 300)
        {
            cout << "[cos] 上传成功! HTTP " << httpCode << " key=" << remoteKey << endl;
            return true;
        }
        else
        {
            cerr << "[cos] 上传失败! HTTP " << httpCode << " key=" << remoteKey << endl;
            return false;
        }
    }

} // namespace drop
