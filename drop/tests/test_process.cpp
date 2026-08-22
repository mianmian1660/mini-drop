// ============================================================
// tests/test_process.cpp — 整机（宿主机）资源采集单元测试
// 覆盖：CPU 增量计算、零增量/计数器异常、内存解析、磁盘容量
// 计算、读取失败降级。不依赖真实 /proc（通过路径注入临时文件）。
// ============================================================

#include <gtest/gtest.h>

#include <fstream>
#include <sstream>
#include <string>
#include <cstdio>
#include <unistd.h>

#include "common/Process.h"

namespace
{

    // 写一个临时文件，返回路径；用 getpid 保证并行测试不冲突
    std::string WriteTempFile(const std::string &content, const std::string &suffix)
    {
        std::string path = "/tmp/drop_test_" + std::to_string(getpid()) + "_" + suffix + ".txt";
        std::ofstream f(path);
        f << content;
        f.close();
        return path;
    }

    TEST(HostCpuJiffies, ParsesAggregateLine)
    {
        std::string path = WriteTempFile(
            "cpu  1000 10 100 400 20 5 5 0 0 0\n"
            "cpu0 500 5 50 200 10 2 2 0 0 0\n"
            "cpu1 500 5 50 200 10 3 3 0 0 0\n"
            "intr 12345\n",
            "stat");
        auto sample = drop::read_host_cpu_jiffies(path);
        std::remove(path.c_str());
        // 只解析 "cpu "（整机）行：total=1000+10+100+400+20+5+5+0+0+0=1540，idle=400+20=420
        ASSERT_TRUE(sample.valid);
        EXPECT_EQ(sample.total_jiffies, 1540u);
        EXPECT_EQ(sample.idle_jiffies, 420u);
    }

    TEST(HostCpuJiffies, MissingFileIsInvalid)
    {
        auto sample = drop::read_host_cpu_jiffies("/tmp/definitely_missing_proc_stat_xyz");
        EXPECT_FALSE(sample.valid);
        EXPECT_EQ(sample.total_jiffies, 0u);
    }

    TEST(HostCpuJiffies, NoCpuLineIsInvalid)
    {
        std::string path = WriteTempFile("intr 12345\nctxt 99\n", "stat_nocpu");
        auto sample = drop::read_host_cpu_jiffies(path);
        std::remove(path.c_str());
        EXPECT_FALSE(sample.valid);
    }

    TEST(HostCpuPercent, ComputesBusyRatio)
    {
        drop::HostCpuSample before;
        before.total_jiffies = 1000;
        before.idle_jiffies = 400;
        before.valid = true;
        drop::HostCpuSample after;
        after.total_jiffies = 2000;
        after.idle_jiffies = 700;
        after.valid = true;
        // busy 增量 = (2000-1000) - (700-400) = 1000-300 = 700；总量增量 1000 → 70%
        EXPECT_NEAR(drop::compute_host_cpu_percent(before, after), 70.0, 0.0001);
    }

    TEST(HostCpuPercent, ZeroDeltaReturnsZero)
    {
        drop::HostCpuSample before;
        before.total_jiffies = 1000;
        before.idle_jiffies = 400;
        before.valid = true;
        drop::HostCpuSample after = before;
        // 采样间隔内无变化：不能是 NaN/Inf，返回 0%
        EXPECT_EQ(drop::compute_host_cpu_percent(before, after), 0.0);
    }

    TEST(HostCpuPercent, CounterResetReturnsZero)
    {
        drop::HostCpuSample before;
        before.total_jiffies = 2000;
        before.idle_jiffies = 800;
        before.valid = true;
        drop::HostCpuSample after;
        after.total_jiffies = 1000; // 计数器回绕/重置
        after.idle_jiffies = 400;
        after.valid = true;
        EXPECT_EQ(drop::compute_host_cpu_percent(before, after), 0.0);
    }

    TEST(HostCpuPercent, IdleCounterAnomalyReturnsZero)
    {
        drop::HostCpuSample before;
        before.total_jiffies = 1000;
        before.idle_jiffies = 600;
        before.valid = true;
        drop::HostCpuSample after;
        after.total_jiffies = 2000;
        after.idle_jiffies = 400; // idle 反而变小：计数器异常，数据不可靠
        after.valid = true;
        // 保守处理：不产生虚假的 100% 忙碌尖峰，返回 0%
        EXPECT_EQ(drop::compute_host_cpu_percent(before, after), 0.0);
    }

    TEST(HostCpuPercent, InvalidSampleReturnsZero)
    {
        drop::HostCpuSample valid;
        valid.total_jiffies = 1000;
        valid.idle_jiffies = 400;
        valid.valid = true;
        drop::HostCpuSample invalid;
        invalid.valid = false;
        EXPECT_EQ(drop::compute_host_cpu_percent(valid, invalid), 0.0);
        EXPECT_EQ(drop::compute_host_cpu_percent(invalid, valid), 0.0);
    }

    TEST(HostMemInfo, ParsesMeminfoAndComputesUsage)
    {
        std::string path = WriteTempFile(
            "MemTotal:       3910000 kB\n"
            "MemFree:        1000000 kB\n"
            "MemAvailable:   2500000 kB\n"
            "Buffers:         200000 kB\n",
            "meminfo");
        auto mem = drop::read_host_meminfo(path);
        std::remove(path.c_str());
        ASSERT_TRUE(mem.valid);
        EXPECT_EQ(mem.total_bytes, 3910000ull * 1024);
        EXPECT_EQ(mem.available_bytes, 2500000ull * 1024);
    }

    TEST(HostMemInfo, MissingTotalIsInvalid)
    {
        std::string path = WriteTempFile("MemFree: 100 kB\nMemAvailable: 50 kB\n", "meminfo_bad");
        auto mem = drop::read_host_meminfo(path);
        std::remove(path.c_str());
        EXPECT_FALSE(mem.valid);
        EXPECT_EQ(mem.total_bytes, 0u);
    }

    TEST(HostMemInfo, MissingFileIsInvalid)
    {
        auto mem = drop::read_host_meminfo("/tmp/definitely_missing_meminfo_xyz");
        EXPECT_FALSE(mem.valid);
    }

    TEST(HostDisk, ReadsRealFilesystemCapacity)
    {
        // 对真实存在的目录 statvfs：总量 > 0、已用 <= 总量
        auto disk = drop::read_host_disk("/tmp");
        ASSERT_TRUE(disk.valid);
        EXPECT_GT(disk.total_bytes, 0u);
        EXPECT_LE(disk.used_bytes, disk.total_bytes);
        EXPECT_EQ(disk.mount, "/");
    }

    TEST(HostDisk, MissingPathFallsBackToInvalid)
    {
        auto disk = drop::read_host_disk("/definitely/not/a/real/mount/xyz");
        EXPECT_FALSE(disk.valid);
        EXPECT_EQ(disk.total_bytes, 0u);
        EXPECT_EQ(disk.used_bytes, 0u);
    }

} // namespace
