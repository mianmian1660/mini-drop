// ============================================================
// drop_agent (Agent 主程序) 小白版注释
// ============================================================
// 这个程序运行在你要监控的 Linux 机器上
// 它的工作循环：
//   1. 启动 → 向 Server 报到
//   2. 每 5 秒发一次心跳（"我还活着"）
//   3. 如果心跳回复里有任务 → 执行采集（目前还没实现）
//   4. 采集完 → 上传结果 → 回报 Server
// ============================================================

#include <iostream> // cout, cerr（控制台输出）
#include <string>   // string 类型（字符串）
#include <thread>   // this_thread::sleep_for（让程序暂停）
#include <chrono>   // seconds（时间单位）

#include <grpcpp/grpcpp.h>                    // gRPC 客户端库
#include "common/proto/healthcheck.grpc.pb.h" // 心跳协议的 C++ 生成代码
#include "common/proto/init.grpc.pb.h"        // 初始化协议的 C++ 生成代码

// 只引入 gRPC 相关的几个类（避免名字冲突）
using grpc::Channel;         // 通信通道（相当于"电话线"）
using grpc::ClientContext;   // 请求上下文（设置超时等）
using grpc::Status;          // 操作结果（成功/失败）
using namespace std;         // 省去 std:: 前缀
using namespace std::chrono; // 省去 chrono:: 前缀

int main(int argc, char **argv)
{
    // ---------- 1. 确定连接地址 ----------
    string server_addr = "localhost:50051"; // 默认连本地
    if (argc > 1)
    {
        server_addr = argv[1]; // 命令行可以指定地址
    }

    cout << "[agent] drop_agent 启动，连接 server: " << server_addr << endl;

    // 创建到 Server 的"电话线"（不加密的连接）
    auto channel = grpc::CreateChannel(server_addr, grpc::InsecureChannelCredentials());

    // ---------- 2. 注册自己 ----------
    {
        // 创建一个"存根"（stub）= 远程服务的本地代理
        // 调用 stub 的方法就像调用本地函数，实际通过网络发给 Server
        auto stub = initpb::InitAgentInfo::NewStub(channel);

        // 填写注册请求
        initpb::RegisterAgentRequest req;
        req.set_hostname("demo-host"); // 主机名
        req.set_ipaddr("127.0.0.1");   // IP 地址
        req.set_uid("agent-001");      // 唯一 ID
        req.set_agentversion("1.0.0"); // 版本

        // 发送请求，等待响应
        initpb::RegisterAgentResponse resp;
        ClientContext ctx; // 本次请求的上下文
        Status status = stub->RegisterAgent(&ctx, req, &resp);

        // 检查结果
        if (status.ok())
        {
            cout << "[agent] 注册成功" << endl;
        }
        else
        {
            cerr << "[agent] 注册失败: " << status.error_message() << endl;
            return 1; // 注册失败就退出
        }
    }

    // ---------- 3. 心跳循环 ----------
    auto health_stub = healthcheck::HealthCheck::NewStub(channel);

    while (true)
    { // 无限循环，直到被杀死
        // 填写心跳请求
        healthcheck::HealthCheckRequest req;
        req.set_hostname("demo-host");
        req.set_ipaddr("127.0.0.1");
        req.set_uid("agent-001");
        req.set_agentversion("1.0.0");

        // 发送心跳
        healthcheck::HealthCheckResponse resp;
        ClientContext ctx;
        Status status = health_stub->Do(&ctx, req, &resp);

        if (status.ok())
        {
            cout << "[agent] 心跳 OK, pending=" << resp.pending() << endl;
            // TODO: 如果 resp.pending() 为 true，说明有任务要做
            // 从 resp.taskdesc() 获取任务详情并执行
        }
        else
        {
            cerr << "[agent] 心跳失败: " << status.error_message() << endl;
        }

        // 等 5 秒再发下一次心跳
        this_thread::sleep_for(seconds(5));
    }

    return 0;
}
