// ============================================================
// server/HotmethodService.cpp — 任务结果回报服务 实现
// ============================================================

#include "server/HotmethodService.h"
#include "server/ResultNotifier.h"
#include "server/TaskQueue.h"

#include <iostream>
#include <mutex>

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
        cout << "[server] stage=result_received task_tid=" << request->taskid()
             << " attempt_id=" << request->attempt_id()
             << " error=\"" << request->errormessage() << "\""
             << " error_code=" << request->error_code()
             << " fileSize=" << request->file().size()
             << " cosKey=" << request->coskey()
             << " artifactSize=" << request->artifact_size()
             << " sha256=" << request->artifact_sha256()
             << " manifestKey=" << request->manifest_key()
             << " partial=" << (request->partial() ? "true" : "false")
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

        {
            lock_guard<mutex> lock(tasks_mutex);
            mark_attempt_completed_locked(*request);
        }

        // A3: 主动通知 apiserver（"锦上添花"，失败不影响本 RPC 返回成功）
        bool notified = notify_apiserver_task_result(*request);
        cout << "[server] stage=result_callback task_tid=" << request->taskid()
             << " callback_ok=" << (notified ? "true" : "false")
             << " error_code=" << (notified ? "" : "CALLBACK_FAILED") << endl;

        return grpc::Status::OK;
    }

} // namespace drop_server
