#include "common/CoreEbpfCollector.h"

#include <gtest/gtest.h>

#include <atomic>
#include <chrono>
#include <cerrno>
#include <cstring>
#include <cstdlib>
#include <fcntl.h>
#include <string>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <thread>
#include <unistd.h>
#include <vector>

namespace drop
{
namespace
{

bool CoreIntegrationEnabled()
{
    const char *enabled = std::getenv("DROP_RUN_CORE_EBPF_INTEGRATION");
    return enabled && std::string(enabled) == "1";
}

uint64_t signal_count(const std::vector<CoreHistogramSample> &samples, uint32_t signal)
{
    uint64_t total = 0;
    for (const auto &sample : samples)
        if (sample.signal == signal)
            total += sample.count;
    return total;
}

ContinuousTargetProcess self_target()
{
    ContinuousTargetProcess target;
    target.pid = static_cast<int>(::getpid());
    target.processStartMs = 1;
    target.comm = "drop_tests";
    target.exe = "/proc/self/exe";
    return target;
}

void exercise_pipe_io(int iterations)
{
    int fds[2] = {-1, -1};
    ASSERT_EQ(::pipe(fds), 0);
    const char byte = 'x';
    char readByte = 0;
    for (int index = 0; index < iterations; ++index)
    {
        ASSERT_EQ(::write(fds[1], &byte, 1), 1);
        ASSERT_EQ(::read(fds[0], &readByte, 1), 1);
    }
    ::close(fds[0]);
    ::close(fds[1]);
}

TEST(CoreEbpfIntegration, FiltersTargetsAndOnlyTracksReadWriteSyscalls)
{
    if (!CoreIntegrationEnabled())
        GTEST_SKIP() << "set DROP_RUN_CORE_EBPF_INTEGRATION=1 in a privileged host-PID container";
    CoreEbpfCollector collector;
    std::string error;
    ASSERT_TRUE(collector.Start({self_target()}, &error)) << error;
    EXPECT_EQ(collector.DegradationReason().empty(), collector.BlockAvailable());
    uint64_t lost = 0;
    (void)collector.Drain(&lost);

    for (int index = 0; index < 5000; ++index)
        (void)::syscall(SYS_getpid);
    auto nonIo = collector.Drain(&lost);
    EXPECT_EQ(signal_count(nonIo, 2), 0u);

    const pid_t noise = ::fork();
    ASSERT_GE(noise, 0);
    if (noise == 0)
    {
        int fds[2] = {-1, -1};
        if (::pipe(fds) != 0)
            _exit(2);
        const char byte = 'n';
        char readByte = 0;
        for (int index = 0; index < 2000; ++index)
            if (::write(fds[1], &byte, 1) != 1 || ::read(fds[0], &readByte, 1) != 1)
                _exit(3);
        _exit(0);
    }
    int status = 0;
    ASSERT_EQ(::waitpid(noise, &status, 0), noise);
    ASSERT_TRUE(WIFEXITED(status));
    ASSERT_EQ(WEXITSTATUS(status), 0);
    auto filtered = collector.Drain(&lost);
    for (const auto &sample : filtered)
        EXPECT_EQ(sample.tgid, static_cast<uint32_t>(::getpid()));

    exercise_pipe_io(500);
    auto first = collector.Drain(&lost);
    EXPECT_GT(signal_count(first, 2), 0u);
    exercise_pipe_io(500);
    auto second = collector.Drain(&lost);
    EXPECT_GT(signal_count(second, 2), 0u);

    ASSERT_TRUE(collector.SetLostForTesting(7));
    (void)collector.Drain(&lost);
    EXPECT_EQ(lost, 7u);
    (void)collector.Drain(&lost);
    EXPECT_EQ(lost, 0u);
    collector.Stop();
}

TEST(CoreEbpfIntegration, AttributesWakeupLatencyToAwakenedTargetThread)
{
    if (!CoreIntegrationEnabled())
        GTEST_SKIP() << "set DROP_RUN_CORE_EBPF_INTEGRATION=1 in a privileged host-PID container";
    CoreEbpfCollector collector;
    std::string error;
    ASSERT_TRUE(collector.Start({self_target()}, &error)) << error;
    uint64_t lost = 0;
    (void)collector.Drain(&lost);

    int fds[2] = {-1, -1};
    ASSERT_EQ(::pipe(fds), 0);
    std::atomic<bool> waiting{false};
    std::thread sleeper([&] {
        waiting = true;
        char byte = 0;
        (void)::read(fds[0], &byte, 1);
    });
    while (!waiting.load())
        std::this_thread::yield();
    ASSERT_TRUE(collector.UpdateTargets({self_target()}, &error)) << error;
    const char byte = 'w';
    ASSERT_EQ(::write(fds[1], &byte, 1), 1);
    sleeper.join();
    ::close(fds[0]);
    ::close(fds[1]);

    auto samples = collector.Drain(&lost);
    EXPECT_GT(signal_count(samples, 3), 0u);
    for (const auto &sample : samples)
        EXPECT_EQ(sample.tgid, static_cast<uint32_t>(::getpid()));
    collector.Stop();
}

TEST(CoreEbpfIntegration, CorrelatesBlockIssueAndCompletion)
{
    if (!CoreIntegrationEnabled())
        GTEST_SKIP() << "set DROP_RUN_CORE_EBPF_INTEGRATION=1 in a privileged host-PID container";
    CoreEbpfCollector collector;
    std::string error;
    ASSERT_TRUE(collector.Start({self_target()}, &error)) << error;
    if (!collector.BlockAvailable())
    {
        const std::string reason = collector.DegradationReason();
        collector.Stop();
        GTEST_SKIP() << reason;
    }
    uint64_t lost = 0;
    (void)collector.Drain(&lost);

    const std::string path = "/host-tmp/mini-drop-core-ebpf-" + std::to_string(::getpid()) + ".bin";
    int fd = ::open(path.c_str(), O_CREAT | O_TRUNC | O_WRONLY | O_SYNC, 0600);
    ASSERT_GE(fd, 0) << path << ": " << std::strerror(errno);
    void *buffer = nullptr;
    ASSERT_EQ(::posix_memalign(&buffer, 4096, 4096), 0);
    std::memset(buffer, 0x5a, 4096);
    for (int index = 0; index < 2048; ++index)
        ASSERT_EQ(::write(fd, buffer, 4096), 4096);
    ASSERT_EQ(::fdatasync(fd), 0);
    ::close(fd);
    std::free(buffer);
    std::this_thread::sleep_for(std::chrono::milliseconds(100));

    auto samples = collector.Drain(&lost);
    EXPECT_GT(signal_count(samples, 1), 0u);
    for (const auto &sample : samples)
        EXPECT_EQ(sample.tgid, static_cast<uint32_t>(::getpid()));
    collector.Stop();
    EXPECT_EQ(::unlink(path.c_str()), 0);
}

} // namespace
} // namespace drop
