// ============================================================
// common/LanguageStatus.h — 阶段 4/5 统一语言诊断契约（v2）
// ============================================================
// 把"有样本"与"语言采集 ready"区分开。每个语言按固定口径输出：
//   runtime_detection / collector_modes / collector_status /
//   symbol_status / semantic_frame_percent / semantic_sample_percent /
//   unresolved_frame_percent / target_module_unresolved_percent /
//   sample_count / reasons / processes
//
// 状态口径（阶段 4 冻结，服务端/前端不得再各自推导）：
//   collector_status:
//     ready    语义样本覆盖 >= 70% 且无确定性采集失败；托管语言
//              全局未解析率 <= 20%，Native 目标模块未解析率 < 5%
//     partial  已有有效语言帧，但部分进程缺能力或质量未达门槛
//     missing  识别到 runtime，但没有可用语言级采集方式
//     pending  GoReSym 等异步处理尚未完成
//     failed   采集器执行失败/超时/权限拒绝/产物无效
//     not_applicable 本窗未检测到该语言
//   symbol_status: complete(<=5% 未解析) | partial | missing(全部未解析) |
//                  unknown(无帧数据) | not_applicable
//   semantic_frame_percent 按 "样本权重 × 帧数" 计算；
//   semantic_sample_percent 按“至少含一个语言语义帧的样本权重”计算，
//   它才是 collector ready 的语义覆盖门槛。
//   sample_count 使用真实样本权重总和，不使用 JSON 行数。
//
// 序列化结果写入 symbol_refs：
//   { "diagnostics_version": 2, "language_status": { "<runtime>": {...} } }
// 旧字段 runtime_maps / native_go / python_fallback 原样保留一个兼容周期。
// ============================================================

#pragma once

#include "common/ContinuousSegmentProcessor.h"

#include <cstdint>
#include <map>
#include <string>
#include <vector>

namespace drop
{

struct LanguageProcessStatus
{
    int pid = 0;
    int64_t processStartMs = 0;
    std::string comm;
    std::string exe;
    std::string mode;    // perf-map | py-spy | py-spy-native | goresym | perf-fp | perf-dwarf
    std::string status;  // ready|partial|missing|pending|failed|not_applicable
    std::string reason;
};

struct LanguageStatusEntry
{
    std::string runtime;                 // go|java|node|python|native|kernel
    std::string runtimeDetection;        // detected|not_detected|unknown
    std::vector<std::string> collectorModes;
    std::string collectorStatus = "not_applicable";
    std::string symbolStatus = "not_applicable";
    double semanticFramePercent = 0.0;
    double semanticSamplePercent = 0.0;
    double unresolvedFramePercent = 0.0;
    double targetModuleUnresolvedPercent = 0.0;
    uint64_t frameWeight = 0;
    uint64_t semanticFrameWeight = 0;
    uint64_t unresolvedFrameWeight = 0;
    uint64_t semanticSampleWeight = 0;
    uint64_t targetModuleFrameWeight = 0;
    uint64_t targetModuleUnresolvedFrameWeight = 0;
    uint64_t sampleCount = 0;
    std::vector<std::string> reasons;
    std::vector<LanguageProcessStatus> processes;
};

struct LanguageStatusReport
{
    static constexpr int kDiagnosticsVersion = 2;
    int diagnosticsVersion = kDiagnosticsVersion;
    std::map<std::string, LanguageStatusEntry> languages;

    const LanguageStatusEntry *find(const std::string &runtime) const
    {
        auto it = languages.find(runtime);
        return it == languages.end() ? nullptr : &it->second;
    }
};

// ---- 帧分类纯函数（可单测） ----

/// 内核帧：结构化 mappingFile 为 "[...]"（如 [kernel.kallsyms]），或裸字符串
/// 以 "[kernel" 开头。排除内核帧后再计算语义覆盖率。
bool is_kernel_frame(const ContinuousStackFrame &frame);
bool is_kernel_frame_text(const std::string &frameText);

/// 阶段四：运行时基础设施帧（libjvm/libnode/libpython/libc 等解释器与
/// 系统库内部实现帧）。这些帧不属于业务代码，也不计入托管语言的语义
/// 覆盖分母——否则 Node/Java 栈里的 V8/JVM/libc 内部未解析帧会把覆盖率
/// 永远压到门槛以下。业务 DSO 与 JIT map（perf-<pid>.map）不受影响。
bool is_runtime_infrastructure_frame(const ContinuousStackFrame &frame);

/// Python 语义帧：CPython -X perf 的 "py::func:path.py+off"、py-spy 的
/// "func (file.py:line)"。libpython/libc 原生帧不会被误判。
bool is_python_semantic_frame(const ContinuousStackFrame &frame);
bool is_python_semantic_frame_text(const std::string &frameText);

/// JavaScript 语义帧：V8 --perf-basic-prof map 的
/// "LazyCompile:*fn /path/file.js:1:23"、"fn ~f.js"。libnode.so 的原生帧
/// 不算 JavaScript 语义帧。
bool is_js_semantic_frame_text(const std::string &frameText);

/// JVM 语义帧：perf-map/asprof 解析出的方法名（含 '.'/'/'/'$' 分隔符，
/// 排除 0x 未解析与 [kernel]）。
bool is_java_semantic_frame_text(const std::string &frameText);

/// Go 语义帧：Go 符号表/perf map 解析出的 "pkg.Func" 点分符号。
bool is_go_semantic_frame_text(const std::string &frameText);

/// 单样本帧权重统计：total/kernel/unresolved（样本权重 × 帧数）。
struct SampleFrameWeights
{
    uint64_t total = 0;
    uint64_t kernel = 0;
    uint64_t unresolved = 0;
};

SampleFrameWeights sample_frame_weights(const AggregatedSample &sample);

// ---- 报告构建 ----

/// 从最终（host 物理窗或 Session fan-out 后）窗口样本 + 物理诊断构建 v2
/// 语言状态。targets 非空时只保留命中身份（pid+start+exe）的进程实例。
/// unwindMode 为当前物理采集栈回溯模式（fp|dwarf），进入 native 行模式。
LanguageStatusReport build_language_status(const std::vector<AggregatedSample> &samples,
                                           const PhysicalDiagnostics &diagnostics,
                                           const std::vector<ContinuousTargetProcess> *targets,
                                           const std::string &unwindMode);

/// 序列化 language_status 子对象（含 diagnostics_version 外层键）。
std::string language_status_to_json(const LanguageStatusReport &report);

} // namespace drop
