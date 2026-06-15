// ============================================================
// drop_server (Server 主程序) 小白版注释
// ============================================================
// 这个程序是"调度中心"，运行在一台中心服务器上
// 它同时启动 4 个 gRPC 服务，各司其职：
//   1. HealthCheck 服务 — 收心跳、派任务
//   2. Hotmethod 服务  — 收采集结果
//   3. Control 服务     — 收 API 后台的指令
//   4. Init 服务        — 处理 Agent 注册
// ============================================================

#include <iostream>      // cout（控制台输出）
#include <string>        // string
#include <thread>        // thread
#include <chrono>        // 时间相关
#include <memory>        // unique_ptr（智能指针，自动释放内存）
#include <mutex>         // mutex（保护共享队列）
#include <queue>         // queue（任务队列）
#include <unordered_map> // unordered_map（按 IP 保存任务队列）

#include <grpcpp/grpcpp.h>                    // gRPC 服务端库
#include "common/proto/healthcheck.grpc.pb.h" // 心跳协议
#include "common/proto/hotmethod.grpc.pb.h"   // 任务协议
#include "common/proto/control.grpc.pb.h"     // 控制协议
#include "common/proto/init.grpc.pb.h"        // 初始化协议

// 引入 gRPC 服务端需要的类
using grpc::Server;        // gRPC 服务器
using grpc::ServerBuilder; // 服务器构建器
using grpc::ServerContext; // 请求上下文
using grpc::Status;        // 返回状态
using namespace std;

static mutex tasks_mutex;
static unordered_map<string, queue<hotmethod::TaskDesc>> tasks_;

// ============================================================
// 第1个服务：HealthCheck（心跳）
// ============================================================
// 这是一个"类"，继承了 proto 生成的基类
// 重写 Do() 方法来实现心跳逻辑
class HealthCheckServiceImpl final : public healthcheck::HealthCheck::Service
{
    // Do：当 Agent 发来心跳时，这个函数被调用
    Status Do(ServerContext *context,
              const healthcheck::HealthCheckRequest *request,
              healthcheck::HealthCheckResponse *response) override
    {
        cout << "[server] 收到心跳: host=" << request->hostname()
             << " ip=" << request->ipaddr() << endl;

        // 回复 Agent：我还活着，目前没有任务
        response->set_status(healthcheck::HealthCheckResponse::SERVING);
        response->set_pending(false); // false = 没有任务
        {
            lock_guard<mutex> lock(tasks_mutex);
            auto it = tasks_.find(request->ipaddr());
            if (it != tasks_.end() && !it->second.empty())
            {
                response->set_pending(true);
                response->mutable_taskdesc()->Swap(&it->second.front());
                it->second.pop();
                if (it->second.empty())
                {
                    tasks_.erase(it);
                }
            }
        }

        return Status::OK; // 返回"成功"
    }
};

// ============================================================
// 第2个服务：Hotmethod（任务执行与结果回报）
// ============================================================
class HotmethodServiceImpl final : public hotmethod::Hotmethod::Service
{
    // Collect：Server 主动推任务给 Agent（备用通道）
    Status Collect(ServerContext *context,
                   const hotmethod::Target *request,
                   google::protobuf::Empty *response) override
    {
        cout << "[server] Collect 请求: ip=" << request->ip() << endl;
        return Status::OK;
    }

    // NotifyResult：Agent 采集完，把结果送回来
    Status NotifyResult(ServerContext *context,
                        const hotmethod::TaskResult *request,
                        google::protobuf::Empty *response) override
    {
        cout << "[server] 收到结果: taskID=" << request->taskid()
             << " error=" << request->errormessage() << endl;
        // TODO: 把任务状态更新为"完成"或"失败"，通知 API 后台
        return Status::OK;
    }
};

// ============================================================
// 第3个服务：Control（API 后台的指挥接口）
// ============================================================
class ControlServiceImpl final : public control::Control::Service
{
    // CreateTask：API 后台下发一个新任务
    Status CreateTask(ServerContext *context,
                      const control::CreateTaskRequest *request,
                      control::CreateTaskResponse *response) override
    {
        cout << "[server] CreateTask: targetIP=" << request->targetip() << endl;

        // 目前返回一个假的任务 ID
        response->set_taskid("demo-task-001");
        response->set_code(0); // 0 = 成功
        response->set_msg("ok");
        // TODO: 把任务放进 tasks_[targetIP] 队列

        return Status::OK;
    }

    // FetchData：获取某任务的结果数据
    Status FetchData(ServerContext *context,
                     const control::FetchDataRequest *request,
                     control::FetchDataResponse *response) override
    {
        response->set_code(0);
        return Status::OK;
    }

    // StatAgent：查询某 Agent 当前资源占用
    Status StatAgent(ServerContext *context,
                     const control::StatAgentRequest *request,
                     control::StatAgentResponse *response) override
    {
        response->set_code(0);
        return Status::OK;
    }
};

// ============================================================
// 第4个服务：InitAgentInfo（Agent 初始化）
// ============================================================
class InitAgentInfoServiceImpl final : public initpb::InitAgentInfo::Service
{
    // RegisterAgent：Agent 启动时来报到
    Status RegisterAgent(ServerContext *context,
                         const initpb::RegisterAgentRequest *request,
                         initpb::RegisterAgentResponse *response) override
    {
        cout << "[server] Agent 注册: host=" << request->hostname()
             << " ip=" << request->ipaddr() << endl;
        response->set_code(0);
        return Status::OK;
    }

    // FetchConfig：Agent 拉取配置
    Status FetchConfig(ServerContext *context,
                       const initpb::FetchConfigRequest *request,
                       initpb::FetchConfigResponse *response) override
    {
        response->set_code(0);
        return Status::OK;
    }
};

// ============================================================
// main：启动所有服务
// ============================================================
int main(int argc, char **argv)
{
    // 监听的地址和端口（0.0.0.0 表示接受任何 IP 的连接）
    string server_address("0.0.0.0:50051");

    // 创建 4 个服务的实例
    HealthCheckServiceImpl healthcheck_service;
    HotmethodServiceImpl hotmethod_service;
    ControlServiceImpl control_service;
    InitAgentInfoServiceImpl init_service;

    // 用 Builder 模式构建服务器
    ServerBuilder builder;
    builder.AddListeningPort(server_address, grpc::InsecureServerCredentials());
    builder.RegisterService(&healthcheck_service);
    builder.RegisterService(&hotmethod_service);
    builder.RegisterService(&control_service);
    builder.RegisterService(&init_service);

    // 启动服务器（BuildAndStart 返回一个智能指针）
    unique_ptr<Server> server(builder.BuildAndStart());
    cout << "[server] drop_server 启动，监听: " << server_address << endl;

    // Wait() 会阻塞主线程，直到服务器被关闭
    server->Wait();
    return 0;
}
