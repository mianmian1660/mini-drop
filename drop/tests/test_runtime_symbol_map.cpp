// ============================================================
// tests/test_runtime_symbol_map.cpp — RuntimeSymbolMap 纯函数单测
// ============================================================
// 覆盖（修复计划 Step 4 要求的单测）：
//   - host /tmp 与 /proc/<pid>/root/tmp 路径选择
//   - 空文件和 stale map 拒绝
//   - 原子复制（含同文件跳过）
//   - Java refresh cooldown
//   - runtime status 的 complete/partial/missing 聚合
//   - runtime_report_to_json 结构
// ============================================================

#include "common/RuntimeSymbolMap.h"

#include <gtest/gtest.h>

#include <cstdio>
#include <cstdlib>
#include <fstream>
#include <string>
#include <sys/stat.h>
#include <sys/time.h>
#include <unistd.h>

using namespace drop;

namespace
{

std::string temp_path(const std::string &name)
{
    return "/tmp/drop_test_runtime_" + std::to_string(::getpid()) + "_" + name;
}

void write_file(const std::string &path, const std::string &content)
{
    std::ofstream out(path, std::ios::binary);
    out << content;
}

void set_mtime(const std::string &path, int64_t mtimeSec)
{
    struct timeval tv[2];
    tv[0].tv_sec = mtimeSec;
    tv[0].tv_usec = 0;
    tv[1].tv_sec = mtimeSec;
    tv[1].tv_usec = 0;
    ::utimes(path.c_str(), tv);
}

} // namespace

// ---- 路径选择 ----
TEST(RuntimeSymbolMap, PathSelection)
{
    EXPECT_EQ(runtime_perf_map_host_path(123), "/tmp/perf-123.map");
    EXPECT_EQ(runtime_perf_map_pid_root_path(123, 123), "/proc/123/root/tmp/perf-123.map");
    EXPECT_EQ(runtime_perf_map_pid_root_path(123, 1), "/proc/123/root/tmp/perf-1.map");
    EXPECT_EQ(runtime_perf_map_host_path(0), "/tmp/perf-0.map");
}

TEST(RuntimeSymbolMap, NamespacePidFallsBackToHostPid)
{
    // 在 host PID namespace 或无法读取 /proc 的环境中，必须安全回退为 host PID。
    EXPECT_EQ(runtime_pid_namespace_pid(::getpid()), ::getpid());
}

// ---- 进程启动时间解析（用当前进程自身验证） ----
TEST(RuntimeSymbolMap, ProcessStartMsParses)
{
    int64_t startMs = 0;
    ASSERT_TRUE(runtime_process_start_ms(::getpid(), &startMs));
    EXPECT_GT(startMs, 0);
    int64_t nowMs = runtime_now_ms();
    // 启动时间不应晚于当前时间；且不早于 30 天（合理历史范围）。
    // 注意：测试二进制刚启动几毫秒就跑此断言，不能用 >1s 的宽松下界。
    EXPECT_LE(startMs, nowMs);
    EXPECT_LT(nowMs - startMs, 30LL * 24 * 3600 * 1000);
}

// ---- 空文件 / 不存在 / stale map 拒绝 ----
TEST(RuntimeSymbolMap, MapReadyRejectsEmptyMissingStale)
{
    std::string missing = temp_path("missing.map");
    ::remove(missing.c_str());
    EXPECT_FALSE(runtime_map_ready(missing, 0));

    std::string empty = temp_path("empty.map");
    write_file(empty, "");
    EXPECT_FALSE(runtime_map_ready(empty, 0));

    std::string good = temp_path("good.map");
    write_file(good, "0x1000 0x20 goodFunc\n");
    int64_t nowSec = ::time(nullptr);
    set_mtime(good, nowSec);
    EXPECT_TRUE(runtime_map_ready(good, 0));

    // stale：进程启动于 5 分钟前，map mtime 却在 10 分钟前（PID 复用旧文件）
    set_mtime(good, nowSec - 600);
    int64_t procStart = (nowSec - 300) * 1000LL; // 5 分钟前启动
    EXPECT_FALSE(runtime_map_ready(good, procStart));

    // fresh：进程启动于 5 分钟前，map mtime 是当前 → 可消费
    set_mtime(good, nowSec);
    EXPECT_TRUE(runtime_map_ready(good, procStart));

    ::remove(empty.c_str());
    ::remove(good.c_str());
}

// ---- 原子复制 + 同文件跳过 ----
TEST(RuntimeSymbolMap, AtomicCopy)
{
    std::string src = temp_path("copy_src.map");
    std::string dst = temp_path("copy_dst.map");
    std::string dstTmp = dst + ".tmp" + std::to_string(::getpid());
    write_file(src, "0x2000 0x10 fn\n");
    set_mtime(src, 1700000000);
    ::remove(dst.c_str());
    ::remove(dstTmp.c_str());

    EXPECT_TRUE(runtime_copy_map_atomic(src, dst));
    {
        std::ifstream in(dst);
        std::string content((std::istreambuf_iterator<char>(in)), std::istreambuf_iterator<char>());
        EXPECT_EQ(content, "0x2000 0x10 fn\n");
    }
    struct stat srcStat {}, dstStat {};
    ASSERT_EQ(::stat(src.c_str(), &srcStat), 0);
    ASSERT_EQ(::stat(dst.c_str(), &dstStat), 0);
    EXPECT_EQ(dstStat.st_mtime, srcStat.st_mtime);
    // 同文件跳过
    EXPECT_TRUE(runtime_copy_map_atomic(src, dst));
    // 同 inode（src 的硬链接）跳过
    EXPECT_TRUE(runtime_same_file(src, src));
    // 不存在的源
    std::string missingSrc = temp_path("copy_missing_src.map");
    ::remove(missingSrc.c_str());
    EXPECT_FALSE(runtime_copy_map_atomic(missingSrc, dst));

    ::remove(src.c_str());
    ::remove(dst.c_str());
    ::remove(dstTmp.c_str());
}

// ---- Java refresh cooldown ----
TEST(RuntimeSymbolMap, JavaRefreshCooldown)
{
    int pid = 777001;
    int64_t t0 = 1000000;
    EXPECT_TRUE(runtime_java_refresh_allowed(pid, t0));
    runtime_java_refresh_record(pid, t0);
    // 60s 内不允许
    EXPECT_FALSE(runtime_java_refresh_allowed(pid, t0 + 10 * 1000));
    EXPECT_FALSE(runtime_java_refresh_allowed(pid, t0 + 59 * 1000));
    // 60s 后允许
    EXPECT_TRUE(runtime_java_refresh_allowed(pid, t0 + 61 * 1000));
    // 其他 PID 不受影响
    EXPECT_TRUE(runtime_java_refresh_allowed(pid + 1, t0));
}

// ---- symbol_status 聚合 ----
TEST(RuntimeSymbolMap, AggregateStatus)
{
    RuntimeSymbolReport none;
    none.java.detected = false;
    none.node.detected = false;
    none.python.detected = false;
    EXPECT_EQ(runtime_aggregate_status(none), "complete");

    RuntimeSymbolReport allReady;
    allReady.java.detected = true;
    allReady.java.ready = true;
    allReady.node.detected = true;
    allReady.node.ready = true;
    allReady.python.detected = true;
    allReady.python.ready = true;
    EXPECT_EQ(runtime_aggregate_status(allReady), "complete");

    RuntimeSymbolReport someReady;
    someReady.java.detected = true;
    someReady.java.ready = true;
    someReady.node.detected = true;
    someReady.node.ready = false;
    someReady.node.missingPids.push_back(42);
    EXPECT_EQ(runtime_aggregate_status(someReady), "partial");

    RuntimeSymbolReport noneReady;
    noneReady.java.detected = true;
    noneReady.java.ready = false;
    noneReady.node.detected = true;
    noneReady.node.ready = false;
    EXPECT_EQ(runtime_aggregate_status(noneReady), "missing");

    RuntimeSymbolReport onePidMissing;
    onePidMissing.node.detected = true;
    onePidMissing.node.ready = true;
    onePidMissing.node.readyPids.push_back(101);
    onePidMissing.node.missingPids.push_back(102);
    EXPECT_EQ(runtime_aggregate_status(onePidMissing), "missing");
}

// ---- JSON 序列化结构 ----
TEST(RuntimeSymbolMap, ReportToJson)
{
    RuntimeSymbolReport report;
    report.status = "partial";
    report.java.detected = true;
    report.java.ready = true;
    report.node.detected = true;
    report.node.ready = false;
    report.node.missingPids.push_back(99);
    report.node.requiredFlag = "--perf-basic-prof";
    report.node.reason = "missing --perf-basic-prof flag";
    report.python.detected = false;

    std::string json = runtime_report_to_json(report);
    EXPECT_NE(json.find("\"symbol_status\":\"partial\""), std::string::npos);
    EXPECT_NE(json.find("\"runtime_maps\""), std::string::npos);
    EXPECT_NE(json.find("\"java\""), std::string::npos);
    EXPECT_NE(json.find("\"node\""), std::string::npos);
    EXPECT_NE(json.find("\"python\""), std::string::npos);
    EXPECT_NE(json.find("\"detected\":true"), std::string::npos);
    EXPECT_NE(json.find("\"missing\":[99]"), std::string::npos);
    EXPECT_NE(json.find("\"required_flag\":\"--perf-basic-prof\""), std::string::npos);
}
