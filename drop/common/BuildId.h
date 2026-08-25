// ============================================================
// common/BuildId.h — build-id 清单解析与本地符号缓存（一次性任务 + 持续采集共用）
// ============================================================
// 一次性任务(SymbolCollector.cpp)和持续采集(ContinuousSampler.cpp)都需要
// 解析 `perf buildid-list` 输出、判断本地是否已有该 build-id 的符号文件，
// 提取到这里避免两处各写一份。
//
// 持续采集是整机(-a)采集，没有一次性任务那样的单一目标 pid，读取跨容器
// 二进制时不能直接用已知 pid 兜底，需要先枚举当前存活进程建出"DSO 路径
// -> 可读该路径的 pid"索引，再按需查表——不针对单个 build-id 反向搜索全部
// 进程。设计依据见 docs/continuous-symbolization-design.md 任务1"技术方案
// 依据"一节（参考 Parca Agent 的 eBPF 映射事件配对到达、CPA 按 pid 逐个
// 解析自身映射的思路，退化为正向枚举建索引）。
// ============================================================

#pragma once

#include <string>
#include <unordered_map>
#include <vector>

namespace drop
{

    struct BuildIdEntry
    {
        std::string buildId;
        std::string dsoPath;
    };

    /// 解析 `perf buildid-list -i <perf.data>` 的输出，格式每行
    /// "<build-id> <dso-path>"，过滤伪 DSO（[kernel.kallsyms] 等），
    /// 同一 build-id 只保留第一次出现的路径。
    std::vector<BuildIdEntry> parse_buildid_list(const std::string &output);

    /// 本地 perf build-id 缓存目录里是否已有该 build-id 的符号文件
    /// （~/.debug/.build-id/<id[:2]>/<id[2:]>/elf）。
    bool build_id_cached_locally(const std::string &buildId);

    /// 本地 build-id 缓存文件的绝对路径（~/.debug/.build-id/<id[:2]>/
    /// <id[2:]>/elf）；buildId 太短时返回空串。供持续采集上报符号时读
    /// 已经预热进缓存的二进制，避免重复走 /proc 路径解析。
    std::string build_id_local_cache_path(const std::string &buildId);

    /// 把 srcPath 指向的文件内容拷贝进本地 build-id 缓存目录，供 perf
    /// script 自身的 build-id 缓存回退机制命中。srcPath 可以是字面路径，
    /// 也可以是 /proc/<pid>/root/... 这类跨命名空间路径。
    bool cache_build_id_locally(const std::string &buildId, const std::string &srcPath);

    /// 遍历一次当前存活进程的 /proc/<pid>/maps，建出"DSO 路径 -> 可读该
    /// 路径的 pid"索引。整机(-a)采集没有单一目标 pid，不针对单个
    /// build-id 反向搜索全部进程，而是先建一次索引再查表。
    std::unordered_map<std::string, int> build_dso_path_index();

    /// 用索引查到的 pid 尝试字面路径与 /proc/<pid>/root/<dsoPath>，
    /// 返回可读路径；两者都读不到返回空字符串。
    std::string resolve_via_pid(const std::string &dsoPath, int pid);

    // ============================================================
    // 阶段四：Native build-id 预热报告与容器 DSO 深度解析
    // ============================================================

    struct DsoResolveReportEntry
    {
        std::string dsoPath;
        std::string buildId;
        std::string status;       // ready | missing | failed
        std::string resolvedPath; // ready 时为本地缓存或解析出的可读路径
        std::string reason;       // missing/failed 的具体原因
        bool cached = false;      // 命中本地缓存，无需重新解析
    };

    /// 容器 DSO 依次尝试宿主路径 → /proc/<pid>/root/<path> → deleted mapping
    /// 通过 /proc/<pid>/map_files/<start>-<end> 恢复。返回可读路径；全部失败
    /// 返回空串。
    std::string resolve_dso_deep(int pid, const std::string &dsoPath);

} // namespace drop
