// ============================================================
// server/ControlService.cpp — 控制服务 实现
// ============================================================

#include "server/ControlService.h"
#include "server/TaskQueue.h"
#include "server/AgentInfo.h"

#include <iostream>
#include <string>
#include <chrono>
#include <mutex>

using namespace std;

namespace drop_server
{

    grpc::Status ControlServiceImpl::CreateTask(
        grpc::ServerContext * /*context*/,
        const control::CreateTaskRequest *request,
        control::CreateTaskResponse *response)
    {
        cout << "[server] CreateTask: targetIP=" << request->targetip() << endl;

        // 生成任务 ID
        string taskID = "task-" + to_string(chrono::system_clock::now().time_since_epoch().count()) +
                        "-" + to_string(rand() % 10000);

        hotmethod::TaskDesc taskDesc;
        if (request->has_taskdesc())
        {
            taskDesc.CopyFrom(request->taskdesc());
            taskDesc.set_taskid(taskID);
        }
        else
        {
            // 默认：perf 采集 CPU，99Hz，10秒
            taskDesc.set_taskid(taskID);
            taskDesc.set_tasktype(0);
            taskDesc.set_profilertype(0);
            taskDesc.set_timeoutsec(30);
            auto *argv = taskDesc.mutable_sampleargv();
            argv->set_hz(99);
            argv->set_duration(10);
            argv->set_pid(-1);
            argv->set_callgraph("fp");
            argv->set_event("cpu-cycles");
        }

        {
            lock_guard<mutex> lock(tasks_mutex);
            tasks_[request->targetip()].push(taskDesc);
            cout << "[server] 任务入队: taskID=" << taskID
                 << " targetIP=" << request->targetip()
                 << " 队列长度=" << tasks_[request->targetip()].size() << endl;
        }

        response->set_taskid(taskID);
        response->set_code(0);
        response->set_msg("ok");

        return grpc::Status::OK;
    }

    grpc::Status ControlServiceImpl::FetchData(
        grpc::ServerContext * /*context*/,
        const control::FetchDataRequest * /*request*/,
        control::FetchDataResponse *response)
    {
        response->set_code(0);
        return grpc::Status::OK;
    }

    grpc::Status ControlServiceImpl::StatAgent(
        grpc::ServerContext * /*context*/,
        const control::StatAgentRequest *request,
        control::StatAgentResponse *response)
    {
        string targetIP = request->targetip();

        // 在 agents_ 中查找该 IP 对应的 Agent
        {
            lock_guard<mutex> lock(agents_mutex);
            for (const auto &pair : agents_)
            {
                const AgentInfo &info = pair.second;
                if (info.ipAddr == targetIP)
                {
                    response->set_code(0);
                    response->set_msg("ok");
                    response->set_cpupercent(info.lastSelfPstats.cpupercent());
                    response->set_memorykb(info.lastSelfPstats.rsskb());

                    cout << "[server] StatAgent: ip=" << targetIP
                         << " host=" << info.hostname
                         << " online=" << info.online
                         << " cpu=" << info.lastSelfPstats.cpupercent() << "%" << endl;
                    return grpc::Status::OK;
                }
            }
        }

        // 未找到该 IP
        response->set_code(404);
        response->set_msg("agent not found: " + targetIP);
        return grpc::Status::OK;
    }

} // namespace drop_server
