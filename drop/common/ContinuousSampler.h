// ============================================================
// common/ContinuousSampler.h — Native CP sampler abstraction
// ============================================================

#pragma once

#include <cstdint>
#include <atomic>
#include <memory>
#include <string>
#include <thread>
#include <vector>

namespace drop
{

struct ContinuousSampleLabel
{
    std::string comm;
    int pid = 0;
    std::string exe;
};

struct ContinuousTargetProcess
{
    int pid = 0;
    int64_t processStartMs = 0;
    std::string comm;
    std::string exe;
};

// 阶段一（标量健康指标，借鉴 mysqld_exporter/postgres_exporter 的采集口径）
// 与阶段二（SQL digest/锁快照，借鉴 pg_stat_monitor/PMM 的聚合思路）共用同一个
// target 描述：一个 DBSnapshotSampler 实例按 engine 区分查询集合，短连接、
// 用完即断。密码不落这个结构体本身的持久化路径——passwordRef 只是本机文件
// 路径，由采集器在真正发起查询前读取，不经服务端中转、不落 Postgres。
struct DBTargetConfig
{
    std::string engine;        // "mysql" | "postgres"
    std::string instanceLabel; // 用户在 session labels 里起的名字，用于多实例区分
    std::string host;
    int port = 0;
    std::string user;
    std::string passwordRef;      // agent 本机文件路径，指向密码
    int pollIntervalSec = 10;     // 默认对齐 aggregationWindowSec
    int queryTimeoutMs = 500;     // 单条查询超时熔断
};

struct ContinuousSamplerConfig
{
    int sampleRateHz = 19;
    int aggregationWindowSec = 10;
    int uploadBatchSec = 60;
    int retentionHours = 24;
    uint64_t spoolMaxBytes = 5ULL * 1024 * 1024 * 1024;
    uint64_t spoolMinFreeBytes = 2ULL * 1024 * 1024 * 1024;
    int retryMaxSec = 300;
    std::string spoolDirectory = "/var/lib/mini-drop/continuous-spool";
    std::string sessionSID;
    std::string targetIP;
    std::string hostname;
    std::string apiBaseURL;
    std::string authUID;
    std::string scope = "host";
    std::string selectorExe;
    std::string signals = "cpu,io,io_syscall,sched";
    bool allowDegraded = false;
    std::vector<ContinuousTargetProcess> targetProcesses;
    std::vector<DBTargetConfig> dbTargets;
};

// Returns true only when this binary includes the libbpf loader and the
// configured CO-RE object can be opened. Runtime attach is still validated when
// a shared engine starts; callers use this for capability reporting only.
bool CoreContinuousSamplerAvailable();

// 阶段五：服务器存储压力（server_storage_pressure）全局开关。由
// ContinuousSessionManager 从心跳响应的 server_pressure.halted 解析后设置；
// 采样器在 spool_has_collection_capacity 处读取——压力期间停止产生新窗口，
// 已有 spool 继续按重试/ACK 流程排空。
void SetContinuousServerPressure(bool halted);
bool ContinuousServerPressureHalted();

// 阶段五：结构化栈帧。perf 解析保留 IP、symbol、DSO，并从 mmap/build-id
// 计算 file-relative normalized_offset；py-spy/memray/bpftrace 能获取的
// 字段如实填写；无法获取的字段为 NULL/0，不推测。
struct ContinuousStackFrame
{
    std::string function;       // 符号名（无符号时为空）
    std::string raw;            // 原始帧串（perf script 原样）
    std::string file;           // 源文件（可解析时）
    int32_t line = 0;           // 源文件行号（0 = 未知）
    uint64_t address = 0;       // 指令地址（IP；0 = 未知）
    std::string mappingFile;    // 所属 DSO（"" = 未知）
    std::string buildId;        // ELF build-id（16 进制；"" = 未知）
    uint64_t normalizedOffset = 0; // 相对 mapping 基址偏移（0 = 未知）
    bool resolved = false;      // 是否解析出符号
};

struct ContinuousSampleWindow
{
    int64_t windowStartUnixMs = 0;
    int64_t windowEndUnixMs = 0;
    ContinuousSampleLabel label;
    std::vector<std::string> stack;
    uint64_t count = 0;
};

// A stopped Session is acknowledged only after its finalized batches have
// been accepted by the API. These helpers are scoped to one Session directory.
bool ContinuousSessionHasPendingSpool(const ContinuousSamplerConfig &config);
bool DrainOneContinuousSessionBatch(const ContinuousSamplerConfig &config);

class Sampler
{
public:
    virtual ~Sampler() {}
    virtual std::string Name() const = 0;
    virtual bool Start(const ContinuousSamplerConfig &config, std::string *error) = 0;
    virtual void Stop() = 0;
    virtual bool Running() const = 0;
};

class PerfEventSampler : public Sampler
{
public:
    ~PerfEventSampler() override;
    std::string Name() const override;
    bool Start(const ContinuousSamplerConfig &config, std::string *error) override;
    void Stop() override;
    bool Running() const override;

private:
    void Loop();

    std::atomic<bool> running_{false};
    ContinuousSamplerConfig config_;
    std::thread worker_;
};

class DualTrackContinuousSampler : public Sampler
{
public:
    ~DualTrackContinuousSampler() override;
    std::string Name() const override;
    bool Start(const ContinuousSamplerConfig &config, std::string *error) override;
    void Stop() override;
    bool Running() const override;

private:
    void Loop();

    std::atomic<bool> running_{false};
    ContinuousSamplerConfig config_;
    std::thread worker_;
};

// Polls database system views/expvars (config.dbTargets) instead of
// perf_event/eBPF. Shares the same Sampler interface and the same
// spool/retry/ACK pipeline as PerfEventSampler/DualTrackContinuousSampler
// (both call run_continuous_spool_loop; this one does too), but is not a
// perf/eBPF collector — kept as a sibling class rather than folded into
// DualTrackContinuousSampler, whose `signals` set is perf/eBPF-specific.
class DBSnapshotSampler : public Sampler
{
public:
    ~DBSnapshotSampler() override;
    std::string Name() const override;
    bool Start(const ContinuousSamplerConfig &config, std::string *error) override;
    void Stop() override;
    bool Running() const override;

private:
    void Loop();

    std::atomic<bool> running_{false};
    ContinuousSamplerConfig config_;
    std::thread worker_;
};

// One physical degraded collector shared by all active user Sessions. Host and
// process scopes are mutually exclusive at the control plane, so process mode
// records the union of assigned TGIDs once and fans samples out by stable
// PID/start-time identity before writing each Session's spool.
class SharedDualTrackContinuousSampler
{
public:
    SharedDualTrackContinuousSampler();
    ~SharedDualTrackContinuousSampler();

    bool Start(const std::vector<ContinuousSamplerConfig> &sessions, std::string *error);
    void Stop();
    bool Running() const;
    // Start() is asynchronous. In strict mode reconciliation keeps the
    // previous recorder alive until the replacement has parsed its first
    // immutable perf switch-output file. Degraded collectors retain their
    // attach-level readiness and are always surfaced as degraded.
    bool Ready() const;
    bool Strict() const;
    bool Failed() const;
    std::string DegradationReason() const;

private:
    struct Impl;
    std::unique_ptr<Impl> impl_;
};

} // namespace drop
