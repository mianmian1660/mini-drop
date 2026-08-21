#include "common/CoreEbpfCollector.h"

#include <algorithm>
#include <cerrno>
#include <cstdlib>
#include <cstring>
#include <chrono>
#include <dirent.h>
#include <fstream>
#include <sys/stat.h>
#include <thread>

#ifdef DROP_NATIVE_CP_HAVE_LIBBPF
#include <bpf/bpf.h>
#include <bpf/libbpf.h>
#endif

namespace drop
{

bool CoreContinuousSamplerAvailable()
{
#ifdef DROP_NATIVE_CP_HAVE_LIBBPF
    const char *configured = std::getenv("DROP_NATIVE_CP_CORE_OBJECT");
    const char *path = configured && *configured ? configured : "/app/native_cp.bpf.o";
    struct stat st = {};
    return ::stat(path, &st) == 0 && st.st_size > 0;
#else
    return false;
#endif
}

struct CoreEbpfCollector::Impl
{
#ifdef DROP_NATIVE_CP_HAVE_LIBBPF
    bpf_object *object = nullptr;
    std::vector<bpf_link *> links;
    int targetsFd = -1;
    int targetTidsFd = -1;
    int histogramFds[2] = {-1, -1};
    int activeHistogramFd = -1;
    int lostFd = -1;
#endif
    bool running = false;
    bool blockAvailable = false;
    std::string degradationReason;
    std::vector<uint32_t> targetIds;
    std::vector<uint32_t> targetTids;
};

namespace
{

bool numeric_name(const char *name)
{
    if (!name || !*name)
        return false;
    for (const char *p = name; *p; ++p)
        if (*p < '0' || *p > '9')
            return false;
    return true;
}

std::vector<uint32_t> process_tids(const std::vector<ContinuousTargetProcess> &targets)
{
    std::vector<uint32_t> tids;
    for (const auto &target : targets)
    {
        if (target.pid <= 0)
            continue;
        const std::string path = "/proc/" + std::to_string(target.pid) + "/task";
        DIR *directory = ::opendir(path.c_str());
        if (!directory)
            continue;
        while (dirent *entry = ::readdir(directory))
            if (numeric_name(entry->d_name))
                tids.push_back(static_cast<uint32_t>(std::strtoul(entry->d_name, nullptr, 10)));
        ::closedir(directory);
    }
    std::sort(tids.begin(), tids.end());
    tids.erase(std::unique(tids.begin(), tids.end()), tids.end());
    return tids;
}

bool tracepoint_available(const std::string &category, const std::string &event)
{
    struct stat st = {};
    const std::string relative = "/events/" + category + "/" + event + "/id";
    return ::stat(("/sys/kernel/tracing" + relative).c_str(), &st) == 0 ||
           ::stat(("/sys/kernel/debug/tracing" + relative).c_str(), &st) == 0;
}

#ifdef DROP_NATIVE_CP_HAVE_LIBBPF
void append_histogram_map(int fd, std::vector<CoreHistogramSample> *result)
{
    if (fd < 0 || !result)
        return;
    struct Key
    {
        uint32_t signal;
        uint32_t tgid;
        uint32_t slot;
    } key = {}, next = {};
    uint64_t value = 0;
    while (bpf_map_get_next_key(fd, nullptr, &next) == 0)
    {
        key = next;
        if (bpf_map_lookup_elem(fd, &key, &value) == 0)
            result->push_back({key.signal, key.tgid, key.slot, value});
        bpf_map_delete_elem(fd, &key);
        next = {};
    }
}

uint64_t read_and_reset_lost(int fd)
{
    if (fd < 0)
        return 0;
    uint32_t zero = 0;
    uint64_t value = 0;
    if (bpf_map_lookup_elem(fd, &zero, &value) == 0)
    {
        uint64_t reset = 0;
        bpf_map_update_elem(fd, &zero, &reset, BPF_ANY);
    }
    return value;
}
#endif

} // namespace

CoreEbpfCollector::CoreEbpfCollector()
    : impl_(new Impl)
{
}

CoreEbpfCollector::~CoreEbpfCollector()
{
    Stop();
}

bool CoreEbpfCollector::Start(const std::vector<ContinuousTargetProcess> &targets, std::string *error)
{
#ifndef DROP_NATIVE_CP_HAVE_LIBBPF
    if (error)
        *error = "libbpf support is not compiled into this Agent";
    return false;
#else
    if (impl_->running)
        return UpdateTargets(targets, error);
    const char *configured = std::getenv("DROP_NATIVE_CP_CORE_OBJECT");
    std::string objectPath = configured && *configured ? configured : "/app/native_cp.bpf.o";
    bpf_object_open_opts opts = {};
    opts.sz = sizeof(opts);
    impl_->object = bpf_object__open_file(objectPath.c_str(), &opts);
    long openError = impl_->object ? libbpf_get_error(impl_->object) : -ENOMEM;
    if (openError != 0)
    {
        if (error)
            *error = "failed to open CO-RE object " + objectPath + ": " + std::strerror(static_cast<int>(-openError));
        impl_->object = nullptr;
        return false;
    }
    const bool blockAvailable = tracepoint_available("block", "block_rq_issue") &&
                                tracepoint_available("block", "block_rq_complete");
    const bool wakeupNewAvailable = tracepoint_available("sched", "sched_wakeup_new");
    bpf_program *program = nullptr;
    bpf_object__for_each_program(program, impl_->object)
    {
        const std::string name = bpf_program__name(program);
        if ((!blockAvailable && (name == "cp_block_issue" || name == "cp_block_complete")) ||
            (!wakeupNewAvailable && name == "cp_sched_wakeup_new"))
            bpf_program__set_autoload(program, false);
    }
    if (!tracepoint_available("raw_syscalls", "sys_enter") ||
        !tracepoint_available("raw_syscalls", "sys_exit") ||
        !tracepoint_available("sched", "sched_wakeup") ||
        !tracepoint_available("sched", "sched_switch"))
    {
        if (error)
            *error = "required raw_syscalls/sched tracepoints are unavailable";
        Stop();
        return false;
    }
    int loadResult = bpf_object__load(impl_->object);
    if (loadResult != 0)
    {
        if (error)
            *error = std::string("failed to load CO-RE object: ") + std::strerror(loadResult < 0 ? -loadResult : errno);
        Stop();
        return false;
    }
    program = nullptr;
    bpf_object__for_each_program(program, impl_->object)
    {
        if (bpf_program__fd(program) < 0)
            continue;
        bpf_link *link = bpf_program__attach(program);
        if (!link || libbpf_get_error(link))
        {
            if (error)
                *error = "failed to attach CO-RE tracepoint program";
            Stop();
            return false;
        }
        impl_->links.push_back(link);
    }
    impl_->targetsFd = bpf_object__find_map_fd_by_name(impl_->object, "target_tgids");
    impl_->targetTidsFd = bpf_object__find_map_fd_by_name(impl_->object, "target_tids");
    impl_->histogramFds[0] = bpf_object__find_map_fd_by_name(impl_->object, "histogram_a");
    impl_->histogramFds[1] = bpf_object__find_map_fd_by_name(impl_->object, "histogram_b");
    impl_->activeHistogramFd = bpf_object__find_map_fd_by_name(impl_->object, "active_histogram");
    impl_->lostFd = bpf_object__find_map_fd_by_name(impl_->object, "lost_events");
    if (impl_->targetsFd < 0 || impl_->targetTidsFd < 0 ||
        impl_->histogramFds[0] < 0 || impl_->histogramFds[1] < 0 ||
        impl_->activeHistogramFd < 0 || impl_->lostFd < 0)
    {
        if (error)
            *error = "CO-RE object is missing required maps";
        Stop();
        return false;
    }
    impl_->running = true;
    impl_->blockAvailable = blockAvailable;
    impl_->degradationReason = blockAvailable ? "" :
        "kernel block request tracepoints are unavailable; block IO latency is degraded while syscall IO and sched latency remain active";
    uint32_t zero = 0;
    uint32_t active = 0;
    if (bpf_map_update_elem(impl_->activeHistogramFd, &zero, &active, BPF_ANY) != 0)
    {
        if (error)
            *error = "failed to initialize CO-RE histogram buffer";
        Stop();
        return false;
    }
    if (!UpdateTargets(targets, error))
    {
        Stop();
        return false;
    }
    return true;
#endif
}

bool CoreEbpfCollector::UpdateTargets(const std::vector<ContinuousTargetProcess> &targets, std::string *error)
{
#ifndef DROP_NATIVE_CP_HAVE_LIBBPF
    (void)targets;
    if (error)
        *error = "libbpf support is not compiled into this Agent";
    return false;
#else
    if (!impl_->running || impl_->targetsFd < 0 || impl_->targetTidsFd < 0)
    {
        if (error)
            *error = "CO-RE collector is not running";
        return false;
    }
    std::vector<uint32_t> next;
    for (const auto &target : targets)
        if (target.pid > 0)
            next.push_back(static_cast<uint32_t>(target.pid));
    if (next.empty())
        next.push_back(0); // host scope wildcard; the BPF program checks this key
    std::sort(next.begin(), next.end());
    next.erase(std::unique(next.begin(), next.end()), next.end());
    for (uint32_t old : impl_->targetIds)
        if (std::find(next.begin(), next.end(), old) == next.end())
            bpf_map_delete_elem(impl_->targetsFd, &old);
    uint8_t enabled = 1;
    for (uint32_t id : next)
        if (bpf_map_update_elem(impl_->targetsFd, &id, &enabled, BPF_ANY) != 0)
        {
            if (error)
                *error = std::string("failed to update CO-RE target map: ") + std::strerror(errno);
            return false;
        }
    impl_->targetIds = std::move(next);

    std::vector<uint32_t> nextTids = process_tids(targets);
    for (uint32_t old : impl_->targetTids)
        if (std::find(nextTids.begin(), nextTids.end(), old) == nextTids.end())
            bpf_map_delete_elem(impl_->targetTidsFd, &old);
    for (const auto &target : targets)
    {
        if (target.pid <= 0)
            continue;
        const uint32_t tgid = static_cast<uint32_t>(target.pid);
        const std::string path = "/proc/" + std::to_string(target.pid) + "/task";
        DIR *directory = ::opendir(path.c_str());
        if (!directory)
            continue;
        while (dirent *entry = ::readdir(directory))
        {
            if (!numeric_name(entry->d_name))
                continue;
            const uint32_t tid = static_cast<uint32_t>(std::strtoul(entry->d_name, nullptr, 10));
            if (bpf_map_update_elem(impl_->targetTidsFd, &tid, &tgid, BPF_ANY) != 0)
            {
                ::closedir(directory);
                if (error)
                    *error = std::string("failed to update CO-RE target TID map: ") + std::strerror(errno);
                return false;
            }
        }
        ::closedir(directory);
    }
    impl_->targetTids = std::move(nextTids);
    return true;
#endif
}

std::vector<CoreHistogramSample> CoreEbpfCollector::Drain(uint64_t *lost)
{
    std::vector<CoreHistogramSample> result;
    if (lost)
        *lost = 0;
#ifdef DROP_NATIVE_CP_HAVE_LIBBPF
    if (!impl_->running)
        return result;
    uint32_t zero = 0;
    uint32_t active = 0;
    if (bpf_map_lookup_elem(impl_->activeHistogramFd, &zero, &active) != 0)
        return result;
    active &= 1;
    const uint32_t replacement = active ^ 1;
    if (bpf_map_update_elem(impl_->activeHistogramFd, &zero, &replacement, BPF_ANY) != 0)
        return result;
    // Let in-flight programs that observed the old selector finish before the
    // inactive map is enumerated and cleared.
    std::this_thread::sleep_for(std::chrono::milliseconds(2));
    append_histogram_map(impl_->histogramFds[active], &result);
    const uint64_t lostValue = read_and_reset_lost(impl_->lostFd);
    if (lost)
        *lost = lostValue;
#endif
    return result;
}

std::vector<CoreHistogramSample> CoreEbpfCollector::StopAndDrain(uint64_t *lost)
{
    std::vector<CoreHistogramSample> result;
    if (lost)
        *lost = 0;
#ifdef DROP_NATIVE_CP_HAVE_LIBBPF
    if (!impl_->object)
        return result;
    for (auto *link : impl_->links)
        bpf_link__destroy(link);
    impl_->links.clear();
    append_histogram_map(impl_->histogramFds[0], &result);
    append_histogram_map(impl_->histogramFds[1], &result);
    if (lost)
        *lost = read_and_reset_lost(impl_->lostFd);
    bpf_object__close(impl_->object);
    impl_->object = nullptr;
    impl_->targetsFd = -1;
    impl_->targetTidsFd = -1;
    impl_->histogramFds[0] = impl_->histogramFds[1] = -1;
    impl_->activeHistogramFd = -1;
    impl_->lostFd = -1;
#endif
    impl_->targetIds.clear();
    impl_->targetTids.clear();
    impl_->running = false;
    impl_->blockAvailable = false;
    impl_->degradationReason.clear();
    return result;
}

void CoreEbpfCollector::Stop()
{
    uint64_t ignored = 0;
    (void)StopAndDrain(&ignored);
}

bool CoreEbpfCollector::Running() const
{
    return impl_ && impl_->running;
}

bool CoreEbpfCollector::BlockAvailable() const
{
    return impl_ && impl_->running && impl_->blockAvailable;
}

std::string CoreEbpfCollector::DegradationReason() const
{
    return impl_ ? impl_->degradationReason : std::string{};
}

#ifdef DROP_NATIVE_CP_TESTING
bool CoreEbpfCollector::SetLostForTesting(uint64_t value)
{
#ifdef DROP_NATIVE_CP_HAVE_LIBBPF
    if (!impl_->running || impl_->lostFd < 0)
        return false;
    uint32_t zero = 0;
    return bpf_map_update_elem(impl_->lostFd, &zero, &value, BPF_ANY) == 0;
#else
    (void)value;
    return false;
#endif
}
#endif

} // namespace drop
