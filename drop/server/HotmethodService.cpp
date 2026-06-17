// ============================================================
// server/HotmethodService.cpp — 任务结果回报服务 实现
// ============================================================

#include "server/HotmethodService.h"

#include <iostream>

using namespace std;

namespace drop_server
{

    grpc::Status HotmethodServiceImpl::Collect(
        grpc::ServerContext * /*context*/,
        const hotmethod::Target *request,
        google::protobuf::Empty * /*response*/)
    {
        cout << "[server] Collect 请求: ip=" << request->ip() << endl;
        return grpc::Status::OK;
    }

    grpc::Status HotmethodServiceImpl::NotifyResult(
        grpc::ServerContext * /*context*/,
        const hotmethod::TaskResult *request,
        google::protobuf::Empty * /*response*/)
    {
        cout << "[server] 收到结果: taskID=" << request->taskid()
             << " error=\"" << request->errormessage() << "\""
             << " fileSize=" << request->file().size()
             << " cosKey=" << request->coskey()
             << " pstats_count=" << request->selfpstats_size()
             << endl;

        for (int i = 0; i < request->selfpstats_size(); i++)
        {
            const auto &ps = request->selfpstats(i);
            cout << "[server]   Agent PidStats: pid=" << ps.pid()
                 << " CPU=" << ps.cpupercent() << "%"
                 << " RSS=" << ps.rsskb() << "KB"
                 << " IO_r=" << ps.readkbpers() << "KB/s"
                 << " IO_w=" << ps.writekbpers() << "KB/s" << endl;
        }
        return grpc::Status::OK;
    }

} // namespace drop_server
