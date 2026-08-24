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
    uint64_t spoolMinFreeBytes = 1ULL * 1024 * 1024 * 1024;
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
    // 阶段一：信号控制面。requestedSignals 是该 Session 请求的逻辑信号
    // （cpu_profile/io_latency/io_syscall_latency/sched_latency），由 assignment
    // 下发；signals 是物理采集信号集合（cpu/io/io_syscall/sched），共享采集器
    // 取其并集。collectorGeneration 标识物理采集器实例（切换即变），
    // targetFingerprint 标识目标进程集，batchSequence 单调递增。
    std::vector<std::string> requestedSignals;
    std::string collectorGeneration;
    std::string targetFingerprint;
    uint64_t batchSequence = 0;
    bool allowDegraded = false;
    std::vector<ContinuousTargetProcess> targetProcesses;
    std::vector<DBTargetConfig> dbTargets;
};

// Returns true only when this binary includes the libbpf loader and the
// configured CO-RE object can be opened. Runtime attach is still validated when
// a shared engine starts; callers use this for capability reporting only.
bool CoreContinuousSamplerAvailable();

// 阶段一：逻辑信号集合（cpu_profile/io_latency/io_syscall_latency/
// sched_latency）→ 物理采集信号集合字符串（cpu,io,io_syscall,sched）。
// 供 ContinuousSessionManager 构造采样器配置时把 assignment 的信号换算成
// 物理采集集；共享采集器再对所有活动 Session 取并集。
std::string PhysicalSignalsFromRequested(const std::vector<std::string> &requested);

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
    // 阶段一：无重叠采集器切换（cutover）。
    //   BeginHandoff()：进入 ready-but-not-owning——replacement 启动后先就绪
    //     但不输出正式窗口，直到 Own() 被调用。
    //   SetCutoverWatermark(ms, keepBefore)：
    //     旧 generation 收到 watermark 后，仅持久化 endMs <= watermark 的
    //     窗口（keepBefore=true，切点后数据让渡给新 generation）；
    //     新 generation 收到 watermark 后，仅持久化 startMs >= watermark 的
    //     窗口（keepBefore=false，切点前数据不输出）。
    //   Own(watermarkMs)：从 ready-but-not-owning 转为 owning；watermark=0
    //     表示首次启动无 cutover（全量持久化）。旧 generation 不调用 Own。
    void BeginHandoff();
    void SetCutoverWatermark(int64_t watermarkMs, bool keepBefore);
    void Own(int64_t watermarkMs);
    bool Owning() const;

private:
    struct Impl;
    std::unique_ptr<Impl> impl_;
};

} // namespace drop
