// ============================================================
// common/PidRegistry.h — 子进程 pid 登记表（线程安全，内存态）
// ============================================================
// Phase 5：CleanupWorker 需要知道"当前有哪些子进程正被某个 TimedProcessPoller
// 管着"，才能安全地区分"正常在跑，不用管"和"登记了很久却一直没被摘牌，
// 大概率是 Worker 线程卡死/崩溃丢下的孤儿进程，需要兜底强杀"。
//
// 单一所有权：TimedProcessPoller 在 Attach() 时登记、在真正收割进程（无论
// 是 Poll() 自然到终态、还是 ForceStop() 主动终止）时摘牌——这是它的常规
// 生命周期，不需要 CleanupWorker 插手。只有登记时间超过宽限期（明显超过
// 任何 Runner 自己的 timeoutSec+gracePeriodSec）仍未摘牌的条目，才是真正
// 的孤儿，CleanupWorker 才会去抢救——避免和正常收割路径对同一个 pid 竞态
// waitpid。
//
// 已知限制（明确排除在本次范围外）：这张表是内存态的，Agent 进程重启后
// 不做孤儿进程的持久化识别（需要 /proc/<pid>/cmdline 指纹落盘，是独立的
// 可单开的工作）。
// ============================================================

#pragma once

#include <chrono>
#include <mutex>
#include <unordered_map>
#include <utility>
#include <vector>
#include <sys/types.h> // pid_t

namespace drop
{

    class PidRegistry
    {
    public:
        void Register(pid_t pid);
        void Unregister(pid_t pid);

        /// 返回当前登记表的快照：(pid, 登记时刻)。CleanupWorker 用登记时刻
        /// 和 Clock::Now() 的差值判断是否超过宽限期。
        std::vector<std::pair<pid_t, std::chrono::steady_clock::time_point>> Snapshot() const;

    private:
        mutable std::mutex mutex_;
        std::unordered_map<pid_t, std::chrono::steady_clock::time_point> entries_;
    };

} // namespace drop
