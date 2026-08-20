#include "common/GoSymbolizer.h"

#include <gtest/gtest.h>

#include <atomic>
#include <cstdio>
#include <csignal>
#include <fstream>
#include <sstream>
#include <string>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <thread>
#include <unistd.h>

using namespace drop;

TEST(GoSymbolizer, DetectsEmbeddedGoBuildInfo)
{
    std::string path = "/tmp/mini-drop-go-buildinfo-" + std::to_string(::getpid());
    {
        std::ofstream out(path, std::ios::binary);
        out << "prefix\xff Go buildinf:payload";
    }
    EXPECT_TRUE(go_binary_has_build_info(path));
    ::remove(path.c_str());
}

TEST(GoSymbolizer, RejectsBuildInfoTextWithoutGoMagic)
{
    std::string path = "/tmp/mini-drop-not-go-buildinfo-" + std::to_string(::getpid());
    {
        std::ofstream out(path, std::ios::binary);
        out << "ordinary binary containing Go buildinf: text";
    }
    EXPECT_FALSE(go_binary_has_build_info(path));
    ::remove(path.c_str());
}

TEST(GoSymbolizer, ReadsAndValidatesELFBuildID)
{
    char exe[4096] = {0};
    ssize_t n = ::readlink("/proc/self/exe", exe, sizeof(exe) - 1);
    ASSERT_GT(n, 0);
    std::string buildId;
    ASSERT_TRUE(elf_gnu_build_id(std::string(exe, static_cast<size_t>(n)), &buildId));
    EXPECT_GE(buildId.size(), 16u);
    EXPECT_EQ(buildId.find_first_not_of("0123456789abcdef"), std::string::npos);
}

TEST(GoSymbolizer, ParsesStructuredFunctionRanges)
{
    const std::string json = R"({
      "UserFunctions":[{"Start":4727712,"End":4727840,"FullName":"main.work"}],
      "StdFunctions":[{"Start":4816640,"End":4816800,"FullName":"runtime.notewakeup"}]
    })";
    std::vector<GoRecoveredFunction> functions;
    std::string reason;
    ASSERT_TRUE(parse_goresym_json(json, &functions, &reason)) << reason;
    ASSERT_EQ(functions.size(), 2u);
    EXPECT_EQ(functions[0].start, 4727712u);
    EXPECT_EQ(functions[0].size, 128u);
    EXPECT_EQ(functions[1].name, "runtime.notewakeup");
}

TEST(GoSymbolizer, RejectsEmptyOrMalformedOutput)
{
    std::vector<GoRecoveredFunction> functions;
    std::string reason;
    EXPECT_FALSE(parse_goresym_json("not-json", &functions, &reason));
    EXPECT_FALSE(reason.empty());
    reason.clear();
    EXPECT_FALSE(parse_goresym_json("{\"UserFunctions\":[]}", &functions, &reason));
    EXPECT_EQ(reason, "GoReSym returned no functions");
}

TEST(GoSymbolizer, ComputesCurrentProcessPIELoadBias)
{
    char exe[4096] = {0};
    ssize_t n = ::readlink("/proc/self/exe", exe, sizeof(exe) - 1);
    ASSERT_GT(n, 0);
    std::string path(exe, static_cast<size_t>(n));
    uint64_t bias = 0;
    ASSERT_TRUE(go_dso_load_bias(::getpid(), path, true, &bias));
    EXPECT_GT(bias, 0u);
}

TEST(GoSymbolizer, ComputesLoadBiasFromThreadID)
{
    char exe[4096] = {0};
    ssize_t n = ::readlink("/proc/self/exe", exe, sizeof(exe) - 1);
    ASSERT_GT(n, 0);
    std::string path(exe, static_cast<size_t>(n));

    std::atomic<int> threadTid{0};
    std::thread worker([&threadTid]() {
        threadTid = static_cast<int>(::syscall(SYS_gettid));
        while (threadTid.load() > 0)
            ::usleep(1000);
    });
    while (threadTid.load() == 0)
        ::usleep(1000);

    uint64_t processBias = 0, threadBias = 0;
    EXPECT_TRUE(go_dso_load_bias(::getpid(), path, true, &processBias));
    EXPECT_TRUE(go_dso_load_bias(threadTid.load(), path, true, &threadBias));
    EXPECT_EQ(threadBias, processBias);
    threadTid = -1;
    worker.join();
}

TEST(GoSymbolizer, MaterializesPIEMapAndProtectsForeignJITMap)
{
    char exe[4096] = {0};
    ssize_t n = ::readlink("/proc/self/exe", exe, sizeof(exe) - 1);
    ASSERT_GT(n, 0);
    std::string exePath(exe, static_cast<size_t>(n));

    pid_t child = ::fork();
    ASSERT_GE(child, 0);
    if (child == 0)
    {
        ::pause();
        _exit(0);
    }

    const std::string relative = "/tmp/mini-drop-relative-map-" + std::to_string(::getpid());
    const std::string mapPath = "/tmp/perf-" + std::to_string(child) + ".map";
    const std::string sidecar = mapPath + ".mini-drop-go";
    ::remove(mapPath.c_str());
    ::remove(sidecar.c_str());
    {
        std::ofstream out(relative);
        out << "123 10 example.function\n";
    }

    uint64_t bias = 0;
    ASSERT_TRUE(go_dso_load_bias(child, exePath, true, &bias));
    std::string reason;
    EXPECT_TRUE(materialize_go_perf_map(relative, child, "build-a", exePath, true, &reason)) << reason;
    {
        std::ifstream in(mapPath);
        uint64_t address = 0, size = 0;
        std::string name;
        in >> std::hex >> address >> size >> name;
        EXPECT_EQ(address, bias + 0x123);
        EXPECT_EQ(size, 0x10u);
        EXPECT_EQ(name, "example.function");
    }

    // An owned map with stale identity is replaced for the current process.
    {
        std::ofstream out(mapPath, std::ios::trunc);
        out << "dead 1 stale\n";
        std::ofstream meta(sidecar, std::ios::trunc);
        meta << "old-build old-start-time\n";
    }
    reason.clear();
    EXPECT_TRUE(materialize_go_perf_map(relative, child, "build-b", exePath, true, &reason)) << reason;
    {
        std::ifstream in(mapPath);
        std::string line;
        std::getline(in, line);
        EXPECT_NE(line.find("example.function"), std::string::npos);
        std::ifstream meta(sidecar);
        std::getline(meta, line);
        EXPECT_EQ(line.rfind("build-b ", 0), 0u);
    }

    // A map without our sidecar belongs to another JIT and must not be touched.
    ::remove(sidecar.c_str());
    {
        std::ofstream out(mapPath, std::ios::trunc);
        out << "beef 1 foreign-jit\n";
    }
    reason.clear();
    EXPECT_FALSE(materialize_go_perf_map(relative, child, "build-c", exePath, true, &reason));
    EXPECT_EQ(reason, "existing non-Go JIT perf map preserved");
    {
        std::ifstream in(mapPath);
        std::string line;
        std::getline(in, line);
        EXPECT_EQ(line, "beef 1 foreign-jit");
    }

    ::kill(child, SIGTERM);
    ::waitpid(child, nullptr, 0);
    ::remove(relative.c_str());
    ::remove(mapPath.c_str());
    ::remove(sidecar.c_str());
}
