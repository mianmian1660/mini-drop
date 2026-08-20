#include "common/MemrayProfileIngest.h"
#include "common/Utils.h"

#include <algorithm>
#include <cerrno>
#include <cstdlib>
#include <chrono>
#include <dirent.h>
#include <filesystem>
#include <fstream>
#include <map>
#include <mutex>
#include <set>
#include <sstream>
#include <sys/stat.h>
#include <unistd.h>

namespace drop
{
namespace
{
namespace fs = std::filesystem;
std::mutex g_seenMutex;
std::map<std::string, int64_t> g_retryAfter;
std::set<std::string> g_pendingUpload;
std::map<std::string, std::string> g_profileFileIdentity;

int64_t now_ms_memray()
{
    return std::chrono::duration_cast<std::chrono::milliseconds>(
               std::chrono::system_clock::now().time_since_epoch()).count();
}

std::string read_link_memray(const std::string &path)
{
    char buf[4096];
    ssize_t n = ::readlink(path.c_str(), buf, sizeof(buf) - 1);
    if (n <= 0) return "";
    buf[n] = '\0';
    return buf;
}

std::string comm_memray(int pid)
{
    std::ifstream in("/proc/" + std::to_string(pid) + "/comm");
    std::string value;
    std::getline(in, value);
    return trim(value);
}

std::string inode_identity_memray(const std::string &path)
{
    struct stat st{};
    if (::stat(path.c_str(), &st) != 0)
        return "";
    return std::to_string(st.st_dev) + ":" + std::to_string(st.st_ino);
}

bool process_identity_memray(int pid, MemrayProcessIdentity *identity)
{
    if (!identity)
        return false;
    std::ifstream status("/proc/" + std::to_string(pid) + "/status");
    std::string line;
    int namespacePid = 0;
    while (std::getline(status, line))
    {
        if (line.rfind("NSpid:", 0) != 0)
            continue;
        std::istringstream values(line.substr(6));
        while (values >> namespacePid) {}
        break;
    }
    std::ifstream statFile("/proc/" + std::to_string(pid) + "/stat");
    std::string statLine;
    if (namespacePid <= 0 || !std::getline(statFile, statLine))
        return false;
    size_t close = statLine.rfind(')');
    if (close == std::string::npos)
        return false;
    std::istringstream fields(statLine.substr(close + 1));
    std::vector<std::string> values;
    std::string value;
    while (fields >> value)
        values.push_back(value);
    if (values.size() < 20)
        return false;
    char *end = nullptr;
    unsigned long long startTicks = std::strtoull(values[19].c_str(), &end, 10);
    if (!end || *end != '\0' || startTicks == 0)
        return false;
    std::string directory = "/proc/" + std::to_string(pid) + "/root/tmp/mini-drop-memray";
    std::string rootIdentity = inode_identity_memray(directory);
    if (rootIdentity.empty())
        return false;
    identity->hostPid = pid;
    identity->namespacePid = namespacePid;
    identity->startTicks = static_cast<uint64_t>(startTicks);
    identity->rootIdentity = std::move(rootIdentity);
    identity->comm = comm_memray(pid);
    identity->exe = read_link_memray("/proc/" + std::to_string(pid) + "/exe");
    return true;
}

MemrayProfileResult convert_profile(const std::string &path, int hostPid, const std::string &comm, const std::string &exe)
{
    MemrayProfileResult result;
    result.readyPath = path;
    result.pid = hostPid;
    result.comm = comm;
    result.exe = exe;
    MemrayProfileIdentity identity;
    if (parse_memray_profile_identity(path, &identity))
        result.profileID = identity.profileID;
    std::string output;
    int rc = exec_capture({"timeout", "20", "python3", "/app/memray_converter.py", path}, &output, 32 * 1024 * 1024);
    if (rc != 0)
    {
        result.reason = "Memray conversion failed rc=" + std::to_string(rc) + ": " + trim(output);
        return result;
    }
    std::istringstream lines(output);
    std::string line;
    std::string raw;
    if (std::getline(lines, line) && line.rfind("META\t", 0) == 0)
    {
        std::stringstream fields(line);
        std::string ignored;
        std::getline(fields, ignored, '\t');
        std::string convertedProfileID;
        std::getline(fields, convertedProfileID, '\t');
        if (!convertedProfileID.empty())
            result.profileID = std::move(convertedProfileID);
    }
    while (std::getline(lines, line)) raw += line + "\n";
    result.samples = parse_pyspy_raw(raw);
    result.ready = !result.profileID.empty() && !result.samples.empty();
    if (!result.ready) result.reason = "Memray profile contained no peak allocations";
    return result;
}
} // namespace

bool parse_memray_profile_identity(const std::string &path, MemrayProfileIdentity *identity)
{
    if (!identity)
        return false;
    std::string name = fs::path(path).filename().string();
    for (const auto &suffix : {std::string(".ready"), std::string(".done"), std::string(".part")})
        if (name.size() > suffix.size() && name.compare(name.size() - suffix.size(), suffix.size(), suffix) == 0)
        {
            name.resize(name.size() - suffix.size());
            break;
        }
    const std::string prefix = "memray-";
    if (name.rfind(prefix, 0) != 0)
        return false;
    size_t pidEnd = name.find('-', prefix.size());
    size_t ticksEnd = pidEnd == std::string::npos ? std::string::npos : name.find('-', pidEnd + 1);
    if (pidEnd == std::string::npos || ticksEnd == std::string::npos || ticksEnd + 1 >= name.size())
        return false;
    std::string pidText = name.substr(prefix.size(), pidEnd - prefix.size());
    std::string ticksText = name.substr(pidEnd + 1, ticksEnd - pidEnd - 1);
    char *pidTail = nullptr;
    char *ticksTail = nullptr;
    long pid = std::strtol(pidText.c_str(), &pidTail, 10);
    unsigned long long ticks = std::strtoull(ticksText.c_str(), &ticksTail, 10);
    if (!pidTail || *pidTail != '\0' || !ticksTail || *ticksTail != '\0' || pid <= 0 || ticks == 0)
        return false;
    identity->profileID = std::move(name);
    identity->namespacePid = static_cast<int>(pid);
    identity->startTicks = static_cast<uint64_t>(ticks);
    return true;
}

int resolve_memray_profile_process(const MemrayProfileIdentity &profile,
                                   const std::string &rootIdentity,
                                   const std::vector<MemrayProcessIdentity> &processes)
{
    for (const auto &process : processes)
        if (process.rootIdentity == rootIdentity && process.namespacePid == profile.namespacePid &&
            process.startTicks == profile.startTicks)
            return process.hostPid;
    return 0;
}

std::vector<MemrayProfileResult> collect_memray_profiles(size_t maxProfiles)
{
    std::vector<MemrayProfileResult> out;
    std::vector<MemrayProcessIdentity> processes;
    std::map<std::string, std::string> roots;
    DIR *proc = ::opendir("/proc");
    if (!proc) return out;
    struct dirent *entry;
    while ((entry = ::readdir(proc)) != nullptr)
    {
        char *end = nullptr;
        long parsed = std::strtol(entry->d_name, &end, 10);
        if (!end || *end != '\0' || parsed <= 0) continue;
        int pid = static_cast<int>(parsed);
        MemrayProcessIdentity identity;
        if (!process_identity_memray(pid, &identity)) continue;
        processes.push_back(identity);
        roots.emplace(identity.rootIdentity, "/proc/" + std::to_string(pid) + "/root/tmp/mini-drop-memray");
    }
    ::closedir(proc);

    for (const auto &root : roots)
    {
        fs::path directory(root.second);
        std::error_code ec;
        for (const auto &item : fs::directory_iterator(directory, fs::directory_options::skip_permission_denied, ec))
        {
            if (item.path().extension() != ".ready") continue;
            std::string fileIdentity = inode_identity_memray(item.path().string());
            if (fileIdentity.empty()) continue;
            {
                std::lock_guard<std::mutex> lock(g_seenMutex);
                if (g_pendingUpload.count(fileIdentity) > 0) continue;
                auto retry = g_retryAfter.find(fileIdentity);
                if (retry != g_retryAfter.end() && retry->second > now_ms_memray()) continue;
            }
            MemrayProfileIdentity profileIdentity;
            int hostPid = parse_memray_profile_identity(item.path().string(), &profileIdentity)
                              ? resolve_memray_profile_process(profileIdentity, root.first, processes)
                              : 0;
            auto process = std::find_if(processes.begin(), processes.end(), [&](const auto &candidate) {
                return candidate.hostPid == hostPid;
            });
            auto result = convert_profile(item.path().string(), hostPid,
                                          process == processes.end() ? "python" : process->comm,
                                          process == processes.end() ? "" : process->exe);
            {
                std::lock_guard<std::mutex> lock(g_seenMutex);
                if (result.ready)
                {
                    g_pendingUpload.insert(fileIdentity);
                    g_profileFileIdentity[result.profileID] = fileIdentity;
                    g_retryAfter.erase(fileIdentity);
                }
                else
                {
                    g_retryAfter[fileIdentity] = now_ms_memray() + 60 * 1000LL;
                }
            }
            out.push_back(std::move(result));
            if (out.size() >= maxProfiles) break;
        }
        if (out.size() >= maxProfiles) break;
    }
    {
        std::lock_guard<std::mutex> lock(g_seenMutex);
        int64_t now = now_ms_memray();
        for (auto it = g_retryAfter.begin(); it != g_retryAfter.end();)
            it = it->second <= now ? g_retryAfter.erase(it) : std::next(it);
    }
    return out;
}

bool acknowledge_memray_profile(const std::string &readyPath)
{
    if (readyPath.size() < 6 || readyPath.substr(readyPath.size() - 6) != ".ready") return false;
    std::string identity = inode_identity_memray(readyPath);
    MemrayProfileIdentity profile;
    parse_memray_profile_identity(readyPath, &profile);
    {
        std::lock_guard<std::mutex> lock(g_seenMutex);
        if (identity.empty())
        {
            auto known = g_profileFileIdentity.find(profile.profileID);
            if (known != g_profileFileIdentity.end())
                identity = known->second;
        }
        g_pendingUpload.erase(identity);
        g_retryAfter.erase(identity);
    }
    std::string done = readyPath.substr(0, readyPath.size() - 6) + ".done";
    if (::rename(readyPath.c_str(), done.c_str()) != 0)
        return false;
    std::lock_guard<std::mutex> lock(g_seenMutex);
    g_profileFileIdentity.erase(profile.profileID);
    return true;
}

void release_memray_profile(const std::string &readyPath)
{
    std::string identity = inode_identity_memray(readyPath);
    MemrayProfileIdentity profile;
    parse_memray_profile_identity(readyPath, &profile);
    std::lock_guard<std::mutex> lock(g_seenMutex);
    if (identity.empty())
    {
        auto known = g_profileFileIdentity.find(profile.profileID);
        if (known != g_profileFileIdentity.end())
            identity = known->second;
    }
    g_pendingUpload.erase(identity);
}

} // namespace drop
