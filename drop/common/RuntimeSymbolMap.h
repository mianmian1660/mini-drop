// ============================================================
// common/RuntimeSymbolMap.h — Java/Node/Python JIT perf map 自动接入
// ============================================================
// 解决持续 CPU profiling（整机 perf -a）中 Java/Node/Python 用户函数全
// [unknown] 的问题：把各 runtime 进程生成的 perf map 定位、校验、原子搬运
// 到 Agent 可见的 /tmp/perf-<pid>.map，让 perf script 能在同一份 perf.data
// 里解析出用户函数名。
//
// 调用时机：perf record 完成后、perf script 前（collect_window 内调用）。
//
// 职责边界（严格限定，见修复计划 Step 4）：
//   - 枚举 Java/Node/Python PID（与采样到的 PID 求交集，避免把无关基础设施
//     进程也标记为 missing）。
//   - 从 /proc/<host-pid>/root/tmp/perf-<namespace-pid>.map 定位 map（兼容容器 namespace）。
//   - 校验 map 非空且 mtime 不早于进程启动时间（防 PID 复用误用旧 map）。
//   - 原子复制到 Agent /tmp/perf-<pid>.map；源目标相同时跳过。
//   - Java 缺 map 时调用 `timeout 2 asprof jcmd <pid> Compiler.perfmap`，
//     每 PID 60 秒冷却、每轮最多 8 个 JVM、总预算 5 秒。
//   - Node/Python 不做运行时注入，缺 map 只标记 required_flag。
//   - 不删除目标进程生成的 map，不把 perf map 上传 build-id symbol store。
//
// 产出 RuntimeSymbolReport，序列化进批次 JSON 的 symbol_refs：
//   { "symbol_status": "complete|partial|missing",
//     "runtime_maps": {
//       "java":   {"detected": bool, "ready": bool, "missing": [...], "reason": ""},
//       "node":   {...},
//       "python": {...} } }
// ============================================================

#pragma once

#include <cstdint>
#include <string>
#include <vector>

namespace drop
{

struct RuntimeMapInfo
{
    bool detected = false;               // 发现该 runtime 的采样进程
    bool ready = false;                  // 所有检测到的进程 map 均已就绪（非空、非 stale、已就位）
    std::vector<int> readyPids;          // 已就绪的 PID（调试用）
    std::vector<int> missingPids;        // 缺 map 的 PID
    std::string reason;                  // 不 ready 时的原因（含 required_flag）
    std::string requiredFlag;            // node: --perf-basic-prof; python: -X perf
};

struct RuntimeSymbolReport
{
    std::string status = "missing";      // complete | partial | missing
    RuntimeMapInfo java;
    RuntimeMapInfo node;
    RuntimeMapInfo python;
    int skippedRefresh = 0;              // 因预算/冷却跳过的 JVM 刷新次数
};

// ---- 纯函数（可单测） ----

/// Agent（挂载宿主 /tmp）可见的 host namespace map 路径：/tmp/perf-<pid>.map
std::string runtime_perf_map_host_path(int pid);

/// 进程在最内层 PID namespace 中的 PID；读不到时回退为 host PID。
int runtime_pid_namespace_pid(int hostPid);

/// 跨 namespace 读取路径：/proc/<host-pid>/root/tmp/perf-<runtime-pid>.map。
/// runtime-pid 是 runtime 生成 perf map 时看到的 PID。
std::string runtime_perf_map_pid_root_path(int hostPid, int runtimePid);

/// 进程启动的 wall-clock 毫秒（/proc/<pid>/stat 第 22 字段 + /proc/uptime 换算）。
/// 读不到返回 false。
bool runtime_process_start_ms(int pid, int64_t *out);

/// map 是否可消费：文件存在、非空、mtime 不早于进程启动时间（防 PID 复用）。
/// processStartMs <= 0 时只校验存在且非空。
bool runtime_map_ready(const std::string &path, int64_t processStartMs);

/// 两个路径是否指向同一文件（st_dev + st_ino）。
bool runtime_same_file(const std::string &a, const std::string &b);

/// 原子复制（临时文件 + rename），源目标同文件直接返回 true。
bool runtime_copy_map_atomic(const std::string &src, const std::string &dst);

/// Java map 刷新冷却：每 PID 每 60 秒最多一次（仅查询，不记录）。
bool runtime_java_refresh_allowed(int pid, int64_t nowMs);

/// 记录一次 Java map 刷新尝试（成功/失败都记录，避免 60s 内重复尝试）。
void runtime_java_refresh_record(int pid, int64_t nowMs);

/// 根据报告中的三个 runtime 状态聚合 symbol_status（complete/partial/missing）。
std::string runtime_aggregate_status(const RuntimeSymbolReport &report);

/// 序列化为 symbol_refs JSON（供 build_batch_json 使用）。
std::string runtime_report_to_json(const RuntimeSymbolReport &report);

/// 主流程：枚举 runtime 进程（与 perf.data 采样 PID 交集）、定位/校验/复制
/// map、必要时刷新 Java map，产出报告。dataPath 为 perf.data 路径；perfBin
/// 为 perf 可执行文件（用于 -F comm,pid 提取采样 PID）。
RuntimeSymbolReport collect_runtime_maps(const std::string &perfBin, const std::string &dataPath);

/// 测试辅助：当前 wall-clock ms。
int64_t runtime_now_ms();

} // namespace drop
