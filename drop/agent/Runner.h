// ============================================================
// agent/Runner.h — Runner 生命周期接口（对齐新复刻指南 5.8 节）
// ============================================================
// 7 个虚方法 Validate/Prepare/Start/Poll/Stop/Collect/Cleanup，
// TaskContext 只暴露任务目录、任务描述、ProcessExecutor、Clock、
// ObjectStore、Logger，不让 Runner 任意访问 Agent 全局状态。
// ============================================================

#pragma once

#include "common/Clock.h"
#include "common/LoggerIface.h"
#include "common/ObjectStore.h"
#include "common/ProcessExecutor.h"
#include "common/proto/hotmethod.pb.h"

#include <string>

namespace drop_agent
{

    // resultCode 和旧 run_*() 系列函数的返回码约定保持一致（-1 fork失败，
    // -2 等待出错/无eBPF样本，-3 超时，-4 目标不存在，-5 信号终止/bpftrace不可用，
    // -6 产物缺失），调用方（agent/WorkerThread.cpp）靠它构建 RunnerOutcome
    // 喂给 build_task_result()，不需要重新猜测每种失败对应的数值。
    struct ValidationResult
    {
        bool ok = false;
        int resultCode = 0;
        std::string errorCode;
        std::string errorMessage;
    };

    struct PrepareResult
    {
        bool ok = false;
        int resultCode = 0;
        std::string errorCode;
        std::string errorMessage;
    };

    struct StartResult
    {
        bool ok = false;
        int resultCode = 0;
        std::string errorCode;
        std::string errorMessage;
    };

    enum class PollStatus
    {
        kRunning,
        kSucceeded,
        kFailed,
    };

    struct PollResult
    {
        PollStatus status = PollStatus::kRunning;
        int resultCode = 0; // 只在 status != kRunning 时有意义，语义对齐旧 run_*() 的返回码约定
    };

    enum class StopReason
    {
        kCancel,
        kTimeout,
        kAgentShutdown,
    };

    struct StopResult
    {
        bool ok = true; // 幂等：已终态时直接 ok=true，不报错
    };

    struct CollectResult
    {
        int resultCode = 0;
        std::string outputPath;
        std::string profilerName;
        std::string remoteKeyHint; // 相对文件名，如 "perf.data"；最终 key 由调用方拼 "<tid>/" 前缀
        std::string contentType;
        bool partial = false;
    };

    struct CleanupResult
    {
        bool ok = true;
    };

    /// 指南 5.8："TaskContext 只暴露任务目录、受限配置、ProcessExecutor、
    /// Clock、ObjectStore 和 Logger"。task 按值持有（不是引用）——它要跨
    /// TaskQueue(生产者：Heartbeat 线程) -> Worker 线程的队列边界搬运，
    /// 必须是独立所有权对象，不能引用一个可能已经析构的栈帧。
    struct TaskContext
    {
        std::string taskDir; // 任务输出文件的基础路径（不含后缀），Phase 5 之前只是一个文件前缀，不是真正的隔离目录
        hotmethod::TaskDesc task;
        drop::ProcessExecutor *executor = nullptr;
        drop::Clock *clock = nullptr;
        drop::ObjectStore *objectStore = nullptr;
        drop::Logger *logger = nullptr;
    };

    class Runner
    {
    public:
        virtual ~Runner() = default;
        virtual ValidationResult Validate(const TaskContext &ctx) = 0;
        virtual PrepareResult Prepare(TaskContext &ctx) = 0;
        virtual StartResult Start(TaskContext &ctx) = 0;
        virtual PollResult Poll(TaskContext &ctx) = 0;
        virtual StopResult Stop(TaskContext &ctx, StopReason reason) = 0;
        virtual CollectResult Collect(TaskContext &ctx) = 0;
        virtual CleanupResult Cleanup(TaskContext &ctx) = 0;
    };

} // namespace drop_agent
