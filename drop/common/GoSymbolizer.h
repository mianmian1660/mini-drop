#pragma once

#include "common/BuildId.h"

#include <cstdint>
#include <map>
#include <string>
#include <vector>

namespace drop
{

struct GoSymbolItem
{
    std::string buildId;
    std::string dsoPath;
    std::string reason;
};

struct GoSymbolReport
{
    std::vector<GoSymbolItem> ready;
    std::vector<GoSymbolItem> pending;
    std::vector<GoSymbolItem> failed;
};

struct GoRecoveredFunction
{
    uint64_t start = 0;
    uint64_t size = 0;
    std::string name;
};

/// Cheaply detects the Go build-info marker without depending on a symbol table.
bool go_binary_has_build_info(const std::string &path);

/// Reads the GNU build-id from an ELF PT_NOTE segment as lowercase hex.
bool elf_gnu_build_id(const std::string &path, std::string *buildId);

/// Validates a perf build-id against the source ELF. Go binaries without a
/// GNU build-id are accepted only when the Go build-info marker is present.
bool go_source_matches_perf_build_id(const std::string &path, const std::string &perfBuildId);

/// Returns the GNU build-id when present, otherwise a stable identity for a
/// confirmed Go binary. Non-Go files return false.
bool go_file_build_id(const std::string &path, std::string *buildId);

/// Discovers executable Go DSOs directly from sampled PIDs. This covers
/// stripped Go binaries omitted by `perf buildid-list`.
std::vector<BuildIdEntry> discover_sampled_go_build_ids(const std::map<int, int> &sampledPids);

/// Parses GoReSym JSON and returns relative/ELF virtual function ranges.
bool parse_goresym_json(const std::string &json,
                        std::vector<GoRecoveredFunction> *functions,
                        std::string *reason);

/// Returns the load bias for a DSO in /proc/<pid>/maps. ET_EXEC binaries use 0.
bool go_dso_load_bias(int pid, const std::string &dsoPath, bool positionIndependent, uint64_t *bias);

/// Atomically materializes a PID-specific perf map from a cached relative map.
bool materialize_go_perf_map(const std::string &relativeMapPath,
                             int pid,
                             const std::string &buildId,
                             const std::string &dsoPath,
                             bool positionIndependent,
                             std::string *reason);

/// Uses ready cache entries immediately and queues unseen Go build IDs for the
/// single background extractor. This function never waits for GoReSym.
GoSymbolReport prepare_go_symbols(const std::vector<BuildIdEntry> &entries);

/// Direct fallback for file-backed Go ELF mappings when perf does not consult
/// /tmp/perf-<pid>.map. Address is the runtime instruction pointer.
bool resolve_go_symbol(int pid,
                       const std::string &dsoPath,
                       uint64_t address,
                       std::string *name);

std::string go_symbol_report_json(const GoSymbolReport &report);

} // namespace drop
