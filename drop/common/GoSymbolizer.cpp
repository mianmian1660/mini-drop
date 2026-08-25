#include "common/GoSymbolizer.h"
#include "common/Utils.h"

#include <algorithm>
#include <cerrno>
#include <chrono>
#include <cstring>
#include <cstdio>
#include <cstdlib>
#include <deque>
#include <dirent.h>
#include <fcntl.h>
#include <fstream>
#include <future>
#include <iomanip>
#include <iostream>
#include <elf.h>
#include <map>
#include <mutex>
#include <nlohmann/json.hpp>
#include <set>
#include <sstream>
#include <sys/stat.h>
#include <unistd.h>

namespace drop
{
namespace
{

constexpr const char *kCacheVersion = "goresym-v3.4";
constexpr int64_t kFailureRetryMs = 5 * 60 * 1000;

// 阶段四：GoReSym 独立开关（默认开启）。关闭时保留 perf 原始样本并标记
// partial/missing，不阻断 recorder；已有缓存仍可免费物化。
bool gore_sym_enabled()
{
    const char *value = std::getenv("DROP_CONTINUOUS_GORESYM");
    if (!value || !*value)
        return true;
    std::string text(value);
    std::transform(text.begin(), text.end(), text.begin(), [](unsigned char c) {
        return static_cast<char>(std::tolower(c));
    });
    return !(text == "0" || text == "false" || text == "no" || text == "off");
}

struct ExtractionJob
{
    std::string buildId;
    std::string dsoPath;
    std::string sourcePath;
    std::string cachePath;
    uint64_t sourceSize = 0;
};

struct ExtractionResult
{
    ExtractionJob job;
    bool ok = false;
    std::string reason;
};

enum class ExtractionState
{
    Pending,
    Failed,
};

struct StateEntry
{
    ExtractionState state = ExtractionState::Pending;
    int64_t retryAfterMs = 0;
    std::string reason;
};

std::mutex g_mutex;
std::map<std::string, StateEntry> g_states;
std::deque<ExtractionJob> g_queue;
std::future<ExtractionResult> g_active;
bool g_hasActive = false;

struct ReadyDso
{
    std::string buildId;
    bool positionIndependent = false;
    std::vector<GoRecoveredFunction> functions;
};

struct LoadBiasEntry
{
    uint64_t bias = 0;
    std::string buildId;
    std::string startTicks;
};

std::map<std::string, ReadyDso> g_readyDsos;
std::map<std::pair<int, std::string>, LoadBiasEntry> g_loadBiases;
std::set<std::string> g_knownGoBuildIds;
std::set<std::string> g_knownNonGoBuildIds;

int64_t now_ms()
{
    return std::chrono::duration_cast<std::chrono::milliseconds>(
               std::chrono::system_clock::now().time_since_epoch())
        .count();
}

bool regular_file(const std::string &path)
{
    struct stat st;
    return ::stat(path.c_str(), &st) == 0 && S_ISREG(st.st_mode) && st.st_size > 0;
}

bool is_process_leader(int pid)
{
    std::ifstream status("/proc/" + std::to_string(pid) + "/status");
    std::string line;
    while (std::getline(status, line))
    {
        if (line.rfind("Tgid:", 0) == 0)
            return std::atoi(line.substr(5).c_str()) == pid;
    }
    return false;
}

uint64_t regular_file_size(const std::string &path)
{
    struct stat st;
    if (::stat(path.c_str(), &st) != 0 || !S_ISREG(st.st_mode) || st.st_size <= 0)
        return 0;
    return static_cast<uint64_t>(st.st_size);
}

size_t align4(size_t value)
{
    return (value + 3U) & ~size_t{3U};
}

bool read_note_build_id(std::ifstream &in, uint64_t offset, uint64_t size, std::string *buildId)
{
    constexpr uint64_t kMaxNoteBytes = 16 * 1024 * 1024;
    if (size < sizeof(Elf64_Nhdr) || size > kMaxNoteBytes)
        return false;
    std::vector<unsigned char> data(static_cast<size_t>(size));
    in.clear();
    in.seekg(static_cast<std::streamoff>(offset), std::ios::beg);
    if (!in.read(reinterpret_cast<char *>(data.data()), static_cast<std::streamsize>(data.size())))
        return false;
    size_t cursor = 0;
    while (cursor + sizeof(Elf64_Nhdr) <= data.size())
    {
        Elf64_Nhdr header{};
        std::memcpy(&header, data.data() + cursor, sizeof(header));
        cursor += sizeof(header);
        size_t nameSize = header.n_namesz;
        size_t descSize = header.n_descsz;
        size_t paddedName = align4(nameSize);
        size_t paddedDesc = align4(descSize);
        if (paddedName > data.size() - cursor || paddedDesc > data.size() - cursor - paddedName)
            return false;
        const unsigned char *name = data.data() + cursor;
        const unsigned char *desc = data.data() + cursor + paddedName;
        if (header.n_type == NT_GNU_BUILD_ID && nameSize >= 3 &&
            std::memcmp(name, "GNU", 3) == 0 && descSize > 0)
        {
            std::ostringstream out;
            out << std::hex << std::setfill('0');
            for (size_t i = 0; i < descSize; ++i)
                out << std::setw(2) << static_cast<unsigned int>(desc[i]);
            *buildId = out.str();
            return true;
        }
        cursor += paddedName + paddedDesc;
    }
    return false;
}

bool secure_atomic_write(const std::string &path, const std::string &content)
{
    std::string pattern = path + ".tmp.XXXXXX";
    std::vector<char> tmp(pattern.begin(), pattern.end());
    tmp.push_back('\0');
    int fd = ::mkstemp(tmp.data());
    if (fd < 0)
        return false;
    bool ok = ::fchmod(fd, 0600) == 0;
    size_t written = 0;
    while (ok && written < content.size())
    {
        ssize_t n = ::write(fd, content.data() + written, content.size() - written);
        if (n < 0 && errno == EINTR)
            continue;
        if (n <= 0)
            ok = false;
        else
            written += static_cast<size_t>(n);
    }
    if (ok)
        ok = ::fsync(fd) == 0;
    if (::close(fd) != 0)
        ok = false;
    if (ok)
        ok = ::rename(tmp.data(), path.c_str()) == 0;
    if (!ok)
        ::unlink(tmp.data());
    return ok;
}

bool make_dirs(const std::string &path)
{
    for (size_t i = 1; i < path.size(); ++i)
        if (path[i] == '/')
            ::mkdir(path.substr(0, i).c_str(), 0755);
    ::mkdir(path.c_str(), 0755);
    struct stat st;
    return ::stat(path.c_str(), &st) == 0 && S_ISDIR(st.st_mode);
}

std::string cache_root()
{
    const char *configured = std::getenv("DROP_GO_SYMBOL_CACHE_DIR");
    std::string root = configured && *configured ? configured : "/var/lib/mini-drop/symbol-cache";
    return root + "/" + kCacheVersion;
}

std::string cache_path(const std::string &buildId)
{
    return cache_root() + "/" + buildId + ".map";
}

std::string json_escape(const std::string &s)
{
    std::string out;
    for (char c : s)
    {
        switch (c)
        {
        case '\\': out += "\\\\"; break;
        case '"': out += "\\\""; break;
        case '\n': out += "\\n"; break;
        case '\r': break;
        default: out += c;
        }
    }
    return out;
}

bool elf_is_pie(const std::string &path, bool *positionIndependent)
{
    std::ifstream in(path, std::ios::binary);
    unsigned char header[20] = {0};
    if (!in.read(reinterpret_cast<char *>(header), sizeof(header)))
        return false;
    if (header[0] != 0x7f || header[1] != 'E' || header[2] != 'L' || header[3] != 'F')
        return false;
    uint16_t type = header[5] == 2
                        ? static_cast<uint16_t>((header[16] << 8) | header[17])
                        : static_cast<uint16_t>(header[16] | (header[17] << 8));
    *positionIndependent = type == 3; // ET_DYN
    return type == 2 || type == 3;    // ET_EXEC / ET_DYN
}

std::map<std::string, std::vector<int>> dso_pid_index()
{
    std::map<std::string, std::vector<int>> index;
    DIR *proc = ::opendir("/proc");
    if (!proc)
        return index;
    struct dirent *entry;
    while ((entry = ::readdir(proc)) != nullptr)
    {
        char *end = nullptr;
        long parsed = std::strtol(entry->d_name, &end, 10);
        if (!end || *end != '\0' || parsed <= 0)
            continue;
        int pid = static_cast<int>(parsed);
        if (!is_process_leader(pid))
            continue;
        std::ifstream maps("/proc/" + std::to_string(pid) + "/maps");
        std::set<std::string> seen;
        std::string line;
        while (std::getline(maps, line))
        {
            size_t slash = line.find('/');
            if (slash == std::string::npos)
                continue;
            std::string path = line.substr(slash);
            const std::string deleted = " (deleted)";
            if (path.size() > deleted.size() && path.compare(path.size() - deleted.size(), deleted.size(), deleted) == 0)
                path.resize(path.size() - deleted.size());
            if (seen.insert(path).second)
                index[path].push_back(pid);
        }
    }
    ::closedir(proc);
    return index;
}

std::string resolve_source(const std::string &dsoPath, int pid)
{
    std::string procExe = "/proc/" + std::to_string(pid) + "/exe";
    char target[4096];
    ssize_t targetLen = ::readlink(procExe.c_str(), target, sizeof(target) - 1);
    if (targetLen > 0)
    {
        target[targetLen] = '\0';
        std::string mappedPath(target);
        const std::string deleted = " (deleted)";
        if (mappedPath.size() > deleted.size() &&
            mappedPath.compare(mappedPath.size() - deleted.size(), deleted.size(), deleted) == 0)
            mappedPath.resize(mappedPath.size() - deleted.size());
        if (mappedPath == dsoPath && regular_file(procExe))
            return procExe;
    }
    std::string rooted = "/proc/" + std::to_string(pid) + "/root" + dsoPath;
    if (regular_file(rooted))
        return rooted;
    if (regular_file(dsoPath))
        return dsoPath;
    return "";
}

bool write_relative_cache(const std::string &path, const std::vector<GoRecoveredFunction> &functions, std::string *reason)
{
    size_t slash = path.rfind('/');
    if (slash == std::string::npos || !make_dirs(path.substr(0, slash)))
    {
        *reason = "cannot create Go symbol cache directory";
        return false;
    }
    std::ostringstream out;
    for (const auto &fn : functions)
        out << std::hex << fn.start << ' ' << fn.size << ' ' << fn.name << '\n';
    if (!out.good() || !secure_atomic_write(path, out.str()))
    {
        *reason = "cannot commit Go symbol cache";
        return false;
    }
    return true;
}

void prune_symbol_cache(const std::string &preservePath)
{
    uint64_t maxBytes = 512ULL * 1024ULL * 1024ULL;
    size_t maxFiles = 256;
    if (const char *configured = std::getenv("DROP_GO_SYMBOL_CACHE_MAX_BYTES"))
    {
        uint64_t parsed = std::strtoull(configured, nullptr, 10);
        if (parsed > 0)
            maxBytes = parsed;
    }
    if (const char *configured = std::getenv("DROP_GO_SYMBOL_CACHE_MAX_FILES"))
    {
        unsigned long parsed = std::strtoul(configured, nullptr, 10);
        if (parsed > 0)
            maxFiles = static_cast<size_t>(parsed);
    }

    struct CacheFile
    {
        std::string path;
        uint64_t size;
        int64_t mtime;
    };
    std::vector<CacheFile> files;
    DIR *dir = ::opendir(cache_root().c_str());
    if (!dir)
        return;
    struct dirent *entry;
    while ((entry = ::readdir(dir)) != nullptr)
    {
        std::string name(entry->d_name);
        if (name.size() < 5 || name.compare(name.size() - 4, 4, ".map") != 0)
            continue;
        std::string path = cache_root() + "/" + name;
        struct stat st;
        if (::stat(path.c_str(), &st) == 0 && S_ISREG(st.st_mode))
            files.push_back({path, static_cast<uint64_t>(st.st_size), static_cast<int64_t>(st.st_mtime)});
    }
    ::closedir(dir);
    std::sort(files.begin(), files.end(), [](const CacheFile &a, const CacheFile &b) {
        return a.mtime > b.mtime;
    });

    uint64_t keptBytes = 0;
    size_t keptFiles = 0;
    for (const auto &file : files)
    {
        bool overLimit = keptFiles >= maxFiles || keptBytes >= maxBytes || file.size > maxBytes - keptBytes;
        if (file.path != preservePath && overLimit)
        {
            ::unlink(file.path.c_str());
            continue;
        }
        keptBytes += file.size;
        ++keptFiles;
    }
}

bool read_relative_cache(const std::string &path, std::vector<GoRecoveredFunction> *functions)
{
    functions->clear();
    std::ifstream in(path);
    std::string line;
    while (std::getline(in, line))
    {
        std::istringstream fields(line);
        uint64_t start = 0, size = 0;
        std::string name;
        if (!(fields >> std::hex >> start >> size))
            continue;
        std::getline(fields, name);
        name = drop::trim(name);
        if (start > 0 && size > 0 && !name.empty())
            functions->push_back({start, size, name});
    }
    return !functions->empty();
}

ExtractionResult extract_job(const ExtractionJob &job)
{
    ExtractionResult result;
    result.job = job;
    const char *configuredBin = std::getenv("DROP_GORESYM_BIN");
    std::string bin = configuredBin && *configuredBin ? configuredBin : "/usr/local/bin/GoReSym";
    int timeoutSec = 120;
    if (const char *configuredTimeout = std::getenv("DROP_GO_SYMBOL_TIMEOUT_SEC"))
        timeoutSec = std::max(10, std::atoi(configuredTimeout));
    const char *configuredMemory = std::getenv("DROP_GO_SYMBOL_MEMORY_LIMIT");
    std::string memoryLimit = configuredMemory && *configuredMemory ? configuredMemory : "1024MiB";
    const char *configuredGogc = std::getenv("DROP_GO_SYMBOL_GOGC");
    std::string gogc = configuredGogc && *configuredGogc ? configuredGogc : "20";
    std::string output;
    int rc = exec_capture({"timeout", std::to_string(timeoutSec), "nice", "-n", "10", "env",
                           "GOMEMLIMIT=" + memoryLimit, "GOGC=" + gogc,
                           bin, "-d", job.sourcePath},
                          &output, 64 * 1024 * 1024);
    if (rc != 0)
    {
        if (rc == 137 || output.find("out of memory") != std::string::npos ||
            output.find("cannot allocate memory") != std::string::npos)
            result.reason = "GoReSym exceeded memory limit";
        else
            result.reason = "GoReSym failed rc=" + std::to_string(rc);
        return result;
    }
    std::vector<GoRecoveredFunction> functions;
    if (!parse_goresym_json(output, &functions, &result.reason))
        return result;
    result.ok = write_relative_cache(job.cachePath, functions, &result.reason);
    if (result.ok)
        prune_symbol_cache(job.cachePath);
    return result;
}

void start_next_locked()
{
    if (g_hasActive || g_queue.empty())
        return;
    ExtractionJob job = g_queue.front();
    g_queue.pop_front();
    g_hasActive = true;
    std::cout << "[native-cp] Go symbol extraction started build_id=" << job.buildId
              << " dso=" << job.dsoPath << " bytes=" << job.sourceSize << std::endl;
    g_active = std::async(std::launch::async, [job]() { return extract_job(job); });
}

void reap_locked()
{
    if (!g_hasActive || g_active.wait_for(std::chrono::milliseconds(0)) != std::future_status::ready)
        return;
    ExtractionResult result = g_active.get();
    g_hasActive = false;
    if (result.ok)
    {
        g_states.erase(result.job.buildId);
        std::cout << "[native-cp] Go symbol extraction ready build_id=" << result.job.buildId << std::endl;
    }
    else
    {
        g_states[result.job.buildId] = {ExtractionState::Failed, now_ms() + kFailureRetryMs, result.reason};
        std::cout << "[native-cp] Go symbol extraction failed build_id=" << result.job.buildId
                  << " reason=" << result.reason << std::endl;
    }
    start_next_locked();
}

bool read_process_start_ticks(int pid, std::string *ticks)
{
    std::ifstream in("/proc/" + std::to_string(pid) + "/stat");
    std::string line;
    if (!std::getline(in, line))
        return false;
    size_t close = line.rfind(')');
    if (close == std::string::npos)
        return false;
    std::istringstream fields(line.substr(close + 2));
    std::string value;
    for (int field = 3; field <= 22; ++field)
    {
        if (!(fields >> value))
            return false;
        if (field == 22)
        {
            *ticks = value;
            return true;
        }
    }
    return false;
}

void cleanup_owned_go_perf_maps()
{
    DIR *dir = ::opendir("/tmp");
    if (!dir)
        return;
    struct dirent *entry;
    const std::string prefix = "perf-";
    const std::string suffix = ".map.mini-drop-go";
    while ((entry = ::readdir(dir)) != nullptr)
    {
        std::string name(entry->d_name);
        if (name.size() <= prefix.size() + suffix.size() || name.rfind(prefix, 0) != 0 ||
            name.compare(name.size() - suffix.size(), suffix.size(), suffix) != 0)
            continue;
        std::string pidText = name.substr(prefix.size(), name.size() - prefix.size() - suffix.size());
        if (pidText.empty() || pidText.find_first_not_of("0123456789") != std::string::npos)
            continue;
        int pid = std::atoi(pidText.c_str());
        if (pid > 0 && is_process_leader(pid))
            continue;
        ::unlink(("/tmp/" + name).c_str());
        ::unlink(("/tmp/perf-" + pidText + ".map").c_str());
    }
    ::closedir(dir);
}

bool atomic_write(const std::string &path, const std::string &content)
{
    return secure_atomic_write(path, content);
}

} // namespace

bool go_binary_has_build_info(const std::string &path)
{
    std::ifstream in(path, std::ios::binary);
    if (!in.is_open())
        return false;
    // Go's build-info header starts with "\xff Go buildinf:". Keep the bytes
    // encoded in this detector so the Agent does not match its own rodata.
    static const volatile unsigned char encodedMarker[] = {
        0xa5, 0x7a, 0x1d, 0x35, 0x7a, 0x38, 0x2f,
        0x33, 0x36, 0x3e, 0x33, 0x34, 0x3c, 0x60,
    };
    std::string marker;
    marker.reserve(sizeof(encodedMarker));
    for (size_t i = 0; i < sizeof(encodedMarker); ++i)
        marker.push_back(static_cast<char>(encodedMarker[i] ^ 0x5a));
    std::string carry;
    char buf[64 * 1024];
    size_t scanned = 0;
    constexpr size_t kMaxBuildInfoScanBytes = 1024 * 1024;
    while (scanned < kMaxBuildInfoScanBytes && (in.read(buf, sizeof(buf)) || in.gcount() > 0))
    {
        scanned += static_cast<size_t>(in.gcount());
        std::string chunk = carry + std::string(buf, static_cast<size_t>(in.gcount()));
        if (chunk.find(marker) != std::string::npos)
            return true;
        carry = chunk.size() >= marker.size() ? chunk.substr(chunk.size() - marker.size()) : chunk;
    }
    return false;
}

bool go_binary_has_pclntab(const std::string &path)
{
    // 阶段四：读取 ELF 段表，按段名查找 .gopclntab（Go 运行时函数元数据）。
    // stripped/重命名的 Go ELF 仍保留该段，不依赖 exe 名称或符号表。
    std::ifstream in(path, std::ios::binary);
    if (!in.is_open())
        return false;
    unsigned char ident[EI_NIDENT] = {0};
    if (!in.read(reinterpret_cast<char *>(ident), sizeof(ident)) ||
        std::memcmp(ident, ELFMAG, SELFMAG) != 0 || ident[EI_DATA] != ELFDATA2LSB ||
        ident[EI_CLASS] != ELFCLASS64)
        return false;
    auto readAt = [&in](uint64_t offset, void *buffer, size_t size) {
        in.clear();
        in.seekg(static_cast<std::streamoff>(offset), std::ios::beg);
        return static_cast<bool>(in.read(reinterpret_cast<char *>(buffer), size));
    };
    Elf64_Off shoff = 0;
    uint16_t shentsize = 0, shnum = 0, shstrndx = 0;
    if (!readAt(0x28, &shoff, sizeof(shoff)) || !readAt(0x3A, &shentsize, sizeof(shentsize)) ||
        !readAt(0x3C, &shnum, sizeof(shnum)) || !readAt(0x3E, &shstrndx, sizeof(shstrndx)))
        return false;
    if (shoff == 0 || shnum == 0 || shentsize < sizeof(Elf64_Shdr))
        return false;
    std::vector<Elf64_Shdr> sections(shnum);
    if (!readAt(shoff, sections.data(), sizeof(Elf64_Shdr) * shnum))
        return false;
    if (shstrndx >= shnum)
        return false;
    const Elf64_Shdr &strSection = sections[shstrndx];
    std::vector<char> strTable(strSection.sh_size + 1, '\0');
    if (strTable.size() <= 1 || !readAt(strSection.sh_offset, strTable.data(), strSection.sh_size))
        return false;
    for (const auto &section : sections)
    {
        if (section.sh_name >= strTable.size())
            continue;
        const char *name = strTable.data() + section.sh_name;
        if (std::strcmp(name, ".gopclntab") == 0 && section.sh_size > 0)
            return true;
    }
    return false;
}

bool elf_gnu_build_id(const std::string &path, std::string *buildId)
{
    buildId->clear();
    std::ifstream in(path, std::ios::binary);
    if (!in.is_open())
        return false;
    unsigned char ident[EI_NIDENT] = {0};
    if (!in.read(reinterpret_cast<char *>(ident), sizeof(ident)) ||
        std::memcmp(ident, ELFMAG, SELFMAG) != 0 || ident[EI_DATA] != ELFDATA2LSB)
        return false;
    in.clear();
    in.seekg(0, std::ios::beg);
    if (ident[EI_CLASS] == ELFCLASS64)
    {
        Elf64_Ehdr header{};
        if (!in.read(reinterpret_cast<char *>(&header), sizeof(header)) ||
            header.e_phentsize != sizeof(Elf64_Phdr))
            return false;
        for (Elf64_Half i = 0; i < header.e_phnum; ++i)
        {
            Elf64_Phdr ph{};
            in.clear();
            in.seekg(static_cast<std::streamoff>(header.e_phoff) +
                         static_cast<std::streamoff>(i) * header.e_phentsize,
                     std::ios::beg);
            if (!in.read(reinterpret_cast<char *>(&ph), sizeof(ph)))
                return false;
            if (ph.p_type == PT_NOTE && read_note_build_id(in, ph.p_offset, ph.p_filesz, buildId))
                return true;
        }
    }
    else if (ident[EI_CLASS] == ELFCLASS32)
    {
        Elf32_Ehdr header{};
        if (!in.read(reinterpret_cast<char *>(&header), sizeof(header)) ||
            header.e_phentsize != sizeof(Elf32_Phdr))
            return false;
        for (Elf32_Half i = 0; i < header.e_phnum; ++i)
        {
            Elf32_Phdr ph{};
            in.clear();
            in.seekg(static_cast<std::streamoff>(header.e_phoff) +
                         static_cast<std::streamoff>(i) * header.e_phentsize,
                     std::ios::beg);
            if (!in.read(reinterpret_cast<char *>(&ph), sizeof(ph)))
                return false;
            if (ph.p_type == PT_NOTE && read_note_build_id(in, ph.p_offset, ph.p_filesz, buildId))
                return true;
        }
    }
    return false;
}

bool go_source_matches_perf_build_id(const std::string &path, const std::string &perfBuildId)
{
    std::string elfBuildId;
    if (elf_gnu_build_id(path, &elfBuildId))
        return elfBuildId == perfBuildId;
    return go_binary_has_build_info(path);
}

bool go_file_build_id(const std::string &path, std::string *buildId)
{
    if (!go_binary_has_build_info(path))
        return false;
    if (elf_gnu_build_id(path, buildId))
        return true;
    std::ifstream in(path, std::ios::binary);
    if (!in.is_open())
        return false;
    uint64_t hash = 1469598103934665603ULL;
    char buffer[64 * 1024];
    while (in.read(buffer, sizeof(buffer)) || in.gcount() > 0)
    {
        for (std::streamsize i = 0; i < in.gcount(); ++i)
        {
            hash ^= static_cast<unsigned char>(buffer[i]);
            hash *= 1099511628211ULL;
        }
    }
    std::ostringstream identity;
    identity << "go-fnv-" << std::hex << std::setfill('0') << std::setw(16) << hash;
    *buildId = identity.str();
    return true;
}

std::vector<BuildIdEntry> discover_sampled_go_build_ids(const std::map<int, int> &sampledPids)
{
    std::vector<BuildIdEntry> discovered;
    std::set<std::string> seenPaths;
    for (const auto &sampled : sampledPids)
    {
        if (sampled.first <= 0 || sampled.second <= 0)
            continue;
        std::string procExe = "/proc/" + std::to_string(sampled.first) + "/exe";
        char target[4096];
        ssize_t targetLen = ::readlink(procExe.c_str(), target, sizeof(target) - 1);
        if (targetLen <= 0)
            continue;
        target[targetLen] = '\0';
        std::string dsoPath(target);
        const std::string deleted = " (deleted)";
        if (dsoPath.size() > deleted.size() &&
            dsoPath.compare(dsoPath.size() - deleted.size(), deleted.size(), deleted) == 0)
            dsoPath.resize(dsoPath.size() - deleted.size());
        if (dsoPath.empty() || !seenPaths.insert(dsoPath).second)
            continue;
        std::string buildId;
        if (go_file_build_id(procExe, &buildId))
            discovered.push_back({buildId, dsoPath});
    }
    return discovered;
}

bool parse_goresym_json(const std::string &text,
                        std::vector<GoRecoveredFunction> *functions,
                        std::string *reason)
{
    functions->clear();
    try
    {
        nlohmann::json root = nlohmann::json::parse(text);
        for (const char *key : {"UserFunctions", "StdFunctions"})
        {
            if (!root.contains(key) || !root[key].is_array())
                continue;
            for (const auto &item : root[key])
            {
                uint64_t start = item.value("Start", uint64_t{0});
                uint64_t end = item.value("End", uint64_t{0});
                std::string name = item.value("FullName", std::string{});
                if (start == 0 || end <= start || name.empty() || name.find('\n') != std::string::npos)
                    continue;
                functions->push_back({start, end - start, name});
            }
        }
    }
    catch (const std::exception &e)
    {
        *reason = std::string("invalid GoReSym JSON: ") + e.what();
        return false;
    }
    std::sort(functions->begin(), functions->end(), [](const auto &a, const auto &b) { return a.start < b.start; });
    if (functions->empty())
    {
        *reason = "GoReSym returned no functions";
        return false;
    }
    return true;
}

bool go_dso_load_bias(int pid, const std::string &dsoPath, bool positionIndependent, uint64_t *bias)
{
    if (!positionIndependent)
    {
        *bias = 0;
        return true;
    }
    std::ifstream maps("/proc/" + std::to_string(pid) + "/maps");
    std::string line;
    bool found = false;
    uint64_t best = 0;
    while (std::getline(maps, line))
    {
        size_t slash = line.find('/');
        if (slash == std::string::npos)
            continue;
        std::string path = line.substr(slash);
        const std::string deleted = " (deleted)";
        if (path.size() > deleted.size() && path.compare(path.size() - deleted.size(), deleted.size(), deleted) == 0)
            path.resize(path.size() - deleted.size());
        if (path != dsoPath)
            continue;
        std::istringstream fields(line.substr(0, slash));
        std::string range, perms, offsetText;
        if (!(fields >> range >> perms >> offsetText))
            continue;
        size_t dash = range.find('-');
        if (dash == std::string::npos)
            continue;
        uint64_t start = std::strtoull(range.substr(0, dash).c_str(), nullptr, 16);
        uint64_t offset = std::strtoull(offsetText.c_str(), nullptr, 16);
        if (start < offset)
            continue;
        uint64_t candidate = start - offset;
        if (!found || candidate < best)
        {
            best = candidate;
            found = true;
        }
    }
    if (found)
        *bias = best;
    return found;
}

bool materialize_go_perf_map(const std::string &relativeMapPath,
                             int pid,
                             const std::string &buildId,
                             const std::string &dsoPath,
                             bool positionIndependent,
                             std::string *reason)
{
    std::string startTicks;
    if (!read_process_start_ticks(pid, &startTicks))
    {
        *reason = "process exited before Go perf map materialization";
        return false;
    }
    uint64_t bias = 0;
    if (!go_dso_load_bias(pid, dsoPath, positionIndependent, &bias))
    {
        *reason = "cannot determine DSO load bias";
        return false;
    }
    std::string mapPath = "/tmp/perf-" + std::to_string(pid) + ".map";
    std::string sidecar = mapPath + ".mini-drop-go";
    std::string identity = buildId + " " + startTicks + "\n";
    if (regular_file(mapPath))
    {
        std::ifstream meta(sidecar);
        std::string existing;
        std::getline(meta, existing);
        if (!meta.good() && existing.empty())
        {
            *reason = "existing non-Go JIT perf map preserved";
            return false;
        }
        if (existing == drop::trim(identity))
            return true;
    }
    std::ifstream in(relativeMapPath);
    if (!in.is_open())
    {
        *reason = "Go relative symbol cache missing";
        return false;
    }
    std::ostringstream output;
    std::string line;
    size_t written = 0;
    while (std::getline(in, line))
    {
        std::istringstream fields(line);
        uint64_t start = 0, size = 0;
        std::string name;
        if (!(fields >> std::hex >> start >> size))
            continue;
        std::getline(fields, name);
        name = drop::trim(name);
        if (size == 0 || name.empty())
            continue;
        output << std::hex << (bias + start) << ' ' << size << ' ' << name << '\n';
        ++written;
    }
    if (written == 0 || !atomic_write(mapPath, output.str()))
    {
        *reason = "cannot write PID Go perf map";
        return false;
    }
    if (!atomic_write(sidecar, identity))
    {
        ::remove(mapPath.c_str());
        ::remove(sidecar.c_str());
        *reason = "cannot write PID Go perf map identity";
        return false;
    }
    return true;
}

GoSymbolReport prepare_go_symbols(const std::vector<BuildIdEntry> &entries)
{
    GoSymbolReport report;
    cleanup_owned_go_perf_maps();
    if (entries.empty())
        return report;
    auto index = dso_pid_index();
    std::lock_guard<std::mutex> lock(g_mutex);
    reap_locked();
    int64_t now = now_ms();
    for (const auto &entry : entries)
    {
        // A path can be replaced between windows. Do not let a previously
        // ready function table remain eligible until this exact build-id has
        // been validated and loaded below.
        g_readyDsos.erase(entry.dsoPath);
        auto pids = index.find(entry.dsoPath);
        if (pids == index.end() || pids->second.empty())
            continue;
        std::string source = resolve_source(entry.dsoPath, pids->second.front());
        if (source.empty() || g_knownNonGoBuildIds.count(entry.buildId))
            continue;
        if (!g_knownGoBuildIds.count(entry.buildId))
        {
            if (!go_binary_has_build_info(source))
            {
                g_knownNonGoBuildIds.insert(entry.buildId);
                continue;
            }
            g_knownGoBuildIds.insert(entry.buildId);
        }
        if (!go_source_matches_perf_build_id(source, entry.buildId))
        {
            report.failed.push_back({entry.buildId, entry.dsoPath, "source ELF build-id changed"});
            continue;
        }
        std::string cached = cache_path(entry.buildId);
        if (regular_file(cached))
        {
            bool pie = false;
            if (!elf_is_pie(source, &pie))
            {
                report.failed.push_back({entry.buildId, entry.dsoPath, "invalid Go ELF"});
                continue;
            }
            std::vector<GoRecoveredFunction> functions;
            if (!read_relative_cache(cached, &functions))
            {
                report.failed.push_back({entry.buildId, entry.dsoPath, "invalid Go symbol cache"});
                continue;
            }
            g_readyDsos[entry.dsoPath] = {entry.buildId, pie, std::move(functions)};
            bool anyReady = false;
            std::string lastReason;
            for (int pid : pids->second)
            {
                uint64_t bias = 0;
                std::string startTicks;
                if (go_dso_load_bias(pid, entry.dsoPath, pie, &bias) && read_process_start_ticks(pid, &startTicks))
                    g_loadBiases[{pid, entry.dsoPath}] = {bias, entry.buildId, startTicks};
                anyReady = materialize_go_perf_map(cached, pid, entry.buildId, entry.dsoPath, pie, &lastReason) || anyReady;
            }
            if (anyReady)
                report.ready.push_back({entry.buildId, entry.dsoPath, ""});
            else
                report.failed.push_back({entry.buildId, entry.dsoPath, lastReason});
            continue;
        }
        auto state = g_states.find(entry.buildId);
        if (state != g_states.end())
        {
            if (state->second.state == ExtractionState::Failed && now >= state->second.retryAfterMs)
                g_states.erase(state);
            else if (state->second.state == ExtractionState::Failed)
            {
                report.failed.push_back({entry.buildId, entry.dsoPath, state->second.reason});
                continue;
            }
            else
            {
                report.pending.push_back({entry.buildId, entry.dsoPath, "background extraction"});
                continue;
            }
        }
        if (!gore_sym_enabled())
        {
            // 阶段四：开关关闭 → 不排队新提取；保留 perf 原始样本并给出明确诊断。
            report.failed.push_back({entry.buildId, entry.dsoPath,
                                     "DROP_CONTINUOUS_GORESYM disabled"});
            report.disabled = "DROP_CONTINUOUS_GORESYM disabled";
            continue;
        }
        g_states[entry.buildId] = {ExtractionState::Pending, 0, ""};
        ExtractionJob job{entry.buildId, entry.dsoPath, source, cached, regular_file_size(source)};
        auto insertAt = std::upper_bound(g_queue.begin(), g_queue.end(), job.sourceSize,
                                         [](uint64_t size, const ExtractionJob &queued) {
                                             return size < queued.sourceSize;
                                         });
        g_queue.insert(insertAt, std::move(job));
        report.pending.push_back({entry.buildId, entry.dsoPath, "background extraction"});
    }
    start_next_locked();
    return report;
}

bool resolve_go_symbol(int pid,
                       const std::string &dsoPath,
                       uint64_t address,
                       std::string *name)
{
    std::lock_guard<std::mutex> lock(g_mutex);
    auto ready = g_readyDsos.find(dsoPath);
    if (ready == g_readyDsos.end())
        return false;
    auto biasIt = g_loadBiases.find({pid, dsoPath});
    std::string startTicks;
    if (!read_process_start_ticks(pid, &startTicks))
        return false;
    if (biasIt == g_loadBiases.end() || biasIt->second.buildId != ready->second.buildId ||
        biasIt->second.startTicks != startTicks)
    {
        // perf sample headers can identify a thread (TID), while the eager
        // /proc index above contains process leaders. Thread maps are still
        // available through /proc/<tid>/maps, so resolve and memoize lazily.
        uint64_t bias = 0;
        if (!go_dso_load_bias(pid, dsoPath, ready->second.positionIndependent, &bias))
            return false;
        g_loadBiases[{pid, dsoPath}] = {bias, ready->second.buildId, startTicks};
        biasIt = g_loadBiases.find({pid, dsoPath});
    }
    if (address < biasIt->second.bias)
        return false;
    uint64_t relative = address - biasIt->second.bias;
    const auto &functions = ready->second.functions;
    auto upper = std::upper_bound(functions.begin(), functions.end(), relative,
                                  [](uint64_t value, const GoRecoveredFunction &fn) { return value < fn.start; });
    if (upper == functions.begin())
        return false;
    const auto &fn = *std::prev(upper);
    if (relative < fn.start || relative - fn.start >= fn.size)
        return false;
    *name = fn.name;
    return true;
}

std::string go_symbol_report_json(const GoSymbolReport &report)
{
    auto items = [](const std::vector<GoSymbolItem> &values) {
        std::string out = "[";
        for (size_t i = 0; i < values.size(); ++i)
        {
            if (i) out += ",";
            out += "{\"build_id\":\"" + json_escape(values[i].buildId) +
                   "\",\"dso\":\"" + json_escape(values[i].dsoPath) + "\"";
            if (!values[i].reason.empty())
                out += ",\"reason\":\"" + json_escape(values[i].reason) + "\"";
            out += "}";
        }
        out += "]";
        return out;
    };
    return "{\"ready\":" + items(report.ready) +
           ",\"pending\":" + items(report.pending) +
           ",\"failed\":" + items(report.failed) +
           (report.disabled.empty()
                ? ""
                : ",\"disabled\":\"" + json_escape(report.disabled) + "\"") +
           "}";
}

} // namespace drop
