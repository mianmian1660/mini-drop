package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	TaskKindPerfCPU           = "perf_cpu"
	TaskKindAsyncProfilerJava = "async_profiler_java"
	TaskKindGoPprof           = "go_pprof"
	TaskKindGoPprofHeap       = "go_pprof_heap"
	TaskKindEBPFCPU           = "ebpf_cpu"
	TaskKindEBPFIO            = "ebpf_io"
	TaskKindEBPFSched         = "ebpf_sched"
	TaskKindJavaHeap          = "java_heap"
	TaskKindPythonPySpy       = "python_py_spy"
	TaskKindPythonMemray      = "python_memray"
	TaskKindGperftools        = "gperftools_cpu"
	TaskKindBOLT              = "bolt_profile"
	TaskKindScriptDiagnostic  = "script_diagnostic"
)

type TaskKindField struct {
	Name        string      `json:"name"`
	Label       string      `json:"label"`
	Type        string      `json:"type"`
	Required    bool        `json:"required"`
	Default     interface{} `json:"default,omitempty"`
	Min         *int        `json:"min,omitempty"`
	Max         *int        `json:"max,omitempty"`
	Options     []string    `json:"options,omitempty"`
	Placeholder string      `json:"placeholder,omitempty"`
}

type TaskKindDefinition struct {
	ID               string                 `json:"id"`
	Name             string                 `json:"name"`
	DisplayName      string                 `json:"display_name"`
	Runner           string                 `json:"runner"`
	AnalysisPipeline string                 `json:"analysis_pipeline"`
	Capabilities     []string               `json:"capabilities"`
	Schema           []TaskKindField        `json:"schema"`
	Default          map[string]interface{} `json:"default"`
	MaxDuration      uint64                 `json:"max_duration"`
	MaxConcurrency   int                    `json:"max_concurrency"`
	SupportedOS      []string               `json:"supported_os"`
	TaskType         uint32                 `json:"task_type"`
	ProfilerType     uint32                 `json:"profiler_type"`
	Event            string                 `json:"event,omitempty"`
	Enabled          bool                   `json:"enabled"`
	DisabledReason   string                 `json:"disabled_reason,omitempty"`
}

func ptrInt(v int) *int { return &v }

func taskKindDefinitions() []TaskKindDefinition {
	commonDefaults := map[string]interface{}{
		"target_pid": 0,
		"duration":   10,
		"frequency":  99,
		"callgraph":  "fp",
		"subprocess": false,
	}
	commonFields := []TaskKindField{
		{Name: "target_pid", Label: "目标 PID", Type: "number", Default: 0, Min: ptrInt(0), Placeholder: "留空或 0 表示整机"},
		{Name: "duration", Label: "采样时长（秒）", Type: "number", Required: true, Default: 10, Min: ptrInt(1), Max: ptrInt(3600)},
		{Name: "frequency", Label: "采样频率（Hz）", Type: "number", Required: true, Default: 99, Min: ptrInt(1), Max: ptrInt(10000)},
	}
	return []TaskKindDefinition{
		{
			ID: TaskKindPerfCPU, Name: "perf_cpu", DisplayName: "perf CPU 火焰图",
			Runner: "perf", AnalysisPipeline: "perf_flamegraph",
			Capabilities: []string{"perf"}, SupportedOS: []string{"linux"},
			Default: mergeDefaults(commonDefaults, map[string]interface{}{"event": "cpu-clock"}),
			Schema: append(append([]TaskKindField{}, commonFields...),
				TaskKindField{Name: "callgraph", Label: "调用图模式", Type: "select", Default: "fp", Options: []string{"fp", "dwarf", "lbr"}},
				TaskKindField{Name: "subprocess", Label: "包含子进程", Type: "boolean", Default: false},
			),
			MaxDuration: 3600, MaxConcurrency: 1, TaskType: TaskTypeGeneric, ProfilerType: ProfilerPerf, Event: "cpu-clock",
			Enabled: true,
		},
		{
			ID: TaskKindAsyncProfilerJava, Name: "async_profiler_java", DisplayName: "Java async-profiler",
			Runner: "async-profiler", AnalysisPipeline: "perf_flamegraph",
			Capabilities: []string{"async-profiler", "java"}, SupportedOS: []string{"linux"},
			Default: mergeDefaults(commonDefaults, map[string]interface{}{"target_pid": 0, "event": "cpu"}),
			Schema: []TaskKindField{
				{Name: "target_pid", Label: "Java 目标 PID", Type: "number", Required: true, Default: 0, Min: ptrInt(1)},
				{Name: "duration", Label: "采样时长（秒）", Type: "number", Required: true, Default: 10, Min: ptrInt(1), Max: ptrInt(3600)},
				{Name: "frequency", Label: "采样频率（Hz）", Type: "number", Required: true, Default: 99, Min: ptrInt(1), Max: ptrInt(10000)},
				{Name: "event", Label: "采样事件", Type: "select", Default: "cpu", Options: []string{"cpu", "wall", "alloc"}},
			},
			MaxDuration: 3600, MaxConcurrency: 1, TaskType: TaskTypeJava, ProfilerType: ProfilerAsync, Event: "cpu",
			Enabled: true,
		},
		{
			ID: TaskKindGoPprof, Name: "go_pprof", DisplayName: "Go pprof CPU",
			Runner: "pprof", AnalysisPipeline: "pprof", // 与 analyzer 注册表一致（阶段 4 重分析格式匹配依赖此 pipeline）
			Capabilities: []string{"pprof"}, SupportedOS: []string{"linux"},
			Default: map[string]interface{}{"target_pid": 0, "duration": 10, "frequency": 1, "pprof_url": ""},
			Schema: []TaskKindField{
				{Name: "duration", Label: "采样时长（秒）", Type: "number", Required: true, Default: 10, Min: ptrInt(1), Max: ptrInt(3600)},
				{Name: "frequency", Label: "采样频率（Hz）", Type: "number", Required: true, Default: 1, Min: ptrInt(1), Max: ptrInt(10000)},
				{Name: "pprof_url", Label: "pprof Profile URL", Type: "url", Required: true, Default: "", Placeholder: "http://127.0.0.1:6060/debug/pprof/profile"},
			},
			MaxDuration: 3600, MaxConcurrency: 1, TaskType: TaskTypePprof, ProfilerType: ProfilerPprof,
			Enabled: true,
		},
		{
			ID: TaskKindGoPprofHeap, Name: "go_pprof_heap", DisplayName: "Go pprof Heap",
			Runner: "pprof", AnalysisPipeline: "pprof_heap",
			Capabilities: []string{"pprof"}, SupportedOS: []string{"linux"},
			Default: map[string]interface{}{"target_pid": 0, "duration": 1, "frequency": 1, "pprof_url": ""},
			Schema: []TaskKindField{
				{Name: "pprof_url", Label: "pprof Heap URL", Type: "url", Required: true, Default: "", Placeholder: "http://127.0.0.1:6060/debug/pprof/heap"},
			},
			MaxDuration: 60, MaxConcurrency: 1, TaskType: TaskTypePprof, ProfilerType: ProfilerPprof,
			Enabled: true,
		},
		{
			ID: TaskKindEBPFCPU, Name: "ebpf_cpu", DisplayName: "eBPF CPU 火焰图",
			Runner: "bpftrace", AnalysisPipeline: "bpf_histogram",
			Capabilities: []string{"ebpf", "bpftrace"}, SupportedOS: []string{"linux"},
			Default:     mergeDefaults(commonDefaults, map[string]interface{}{"event": "cpu", "frequency": 1}),
			Schema:      commonFields,
			MaxDuration: 3600, MaxConcurrency: 1, TaskType: TaskTypeBPF, ProfilerType: ProfilerBPF, Event: "cpu",
			Enabled: true,
		},
		{
			ID: TaskKindEBPFIO, Name: "ebpf_io", DisplayName: "eBPF IO 延迟直方图",
			Runner: "bpftrace", AnalysisPipeline: "bpf_histogram",
			Capabilities: []string{"ebpf", "bpftrace"}, SupportedOS: []string{"linux"},
			Default:     mergeDefaults(commonDefaults, map[string]interface{}{"event": "io", "frequency": 1}),
			Schema:      commonFields,
			MaxDuration: 3600, MaxConcurrency: 1, TaskType: TaskTypeBPF, ProfilerType: ProfilerBPF, Event: "io",
			Enabled: true,
		},
		{
			ID: TaskKindEBPFSched, Name: "ebpf_sched", DisplayName: "eBPF 调度延迟直方图",
			Runner: "bpftrace", AnalysisPipeline: "bpf_histogram",
			Capabilities: []string{"ebpf", "bpftrace"}, SupportedOS: []string{"linux"},
			Default:     mergeDefaults(commonDefaults, map[string]interface{}{"event": "sched", "frequency": 1}),
			Schema:      commonFields,
			MaxDuration: 3600, MaxConcurrency: 1, TaskType: TaskTypeBPF, ProfilerType: ProfilerBPF, Event: "sched",
			Enabled: true,
		},
		{
			ID: TaskKindJavaHeap, Name: "java_heap", DisplayName: "Java Heap Dump",
			Runner: "async-profiler", AnalysisPipeline: "java_heap",
			Capabilities: []string{"java", "java-heap"}, SupportedOS: []string{"linux"},
			Default: map[string]interface{}{"target_pid": 0, "duration": 1, "frequency": 1, "event": "heap"},
			Schema: []TaskKindField{
				{Name: "target_pid", Label: "Java 目标 PID", Type: "number", Required: true, Default: 0, Min: ptrInt(1)},
			},
			MaxDuration: 60, MaxConcurrency: 1, TaskType: TaskTypeJavaHeap, ProfilerType: ProfilerAsync, Event: "heap",
			Enabled: false, DisabledReason: "阶段 6 仅声明契约；真实 heap dump Runner 后续接入",
		},
		{
			ID: TaskKindPythonPySpy, Name: "python_py_spy", DisplayName: "Python py-spy",
			Runner: "py-spy", AnalysisPipeline: "perf_flamegraph",
			Capabilities: []string{"py-spy", "python"}, SupportedOS: []string{"linux"},
			Default: mergeDefaults(commonDefaults, map[string]interface{}{"event": "cpu"}),
			Schema:  commonFields, MaxDuration: 3600, MaxConcurrency: 1, TaskType: TaskTypeGeneric, ProfilerType: ProfilerPerf,
			Enabled: false, DisabledReason: "阶段 6 仅声明契约；Agent Runner 后续接入",
		},
		{
			ID: TaskKindPythonMemray, Name: "python_memray", DisplayName: "Python memray",
			Runner: "memray", AnalysisPipeline: "memleak",
			Capabilities: []string{"memray", "python"}, SupportedOS: []string{"linux"},
			Default: mergeDefaults(commonDefaults, map[string]interface{}{"event": "alloc"}),
			Schema:  commonFields, MaxDuration: 3600, MaxConcurrency: 1, TaskType: TaskTypeMemcheck, ProfilerType: ProfilerPerf,
			Enabled: false, DisabledReason: "阶段 6 仅声明契约；Agent Runner 后续接入",
		},
		{
			ID: TaskKindGperftools, Name: "gperftools_cpu", DisplayName: "gperftools CPU",
			Runner: "gperftools", AnalysisPipeline: "perf_flamegraph",
			Capabilities: []string{"gperftools"}, SupportedOS: []string{"linux"},
			Default: mergeDefaults(commonDefaults, map[string]interface{}{"event": "cpu"}),
			Schema:  commonFields, MaxDuration: 3600, MaxConcurrency: 1, TaskType: TaskTypeGeneric, ProfilerType: ProfilerPerf,
			Enabled: false, DisabledReason: "阶段 6 仅声明契约；Agent Runner 后续接入",
		},
		{
			ID: TaskKindBOLT, Name: "bolt_profile", DisplayName: "BOLT 优化 Profile",
			Runner: "bolt", AnalysisPipeline: "perf_flamegraph",
			Capabilities: []string{"bolt"}, SupportedOS: []string{"linux"},
			Default: mergeDefaults(commonDefaults, map[string]interface{}{"event": "branch"}),
			Schema:  commonFields, MaxDuration: 3600, MaxConcurrency: 1, TaskType: TaskTypeGeneric, ProfilerType: ProfilerPerf,
			Enabled: false, DisabledReason: "阶段 6 仅声明契约；Agent Runner 后续接入",
		},
		{
			ID: TaskKindScriptDiagnostic, Name: "script_diagnostic", DisplayName: "受限脚本诊断",
			Runner: "restricted-script", AnalysisPipeline: "script_diagnostic",
			Capabilities: []string{"restricted-script"}, SupportedOS: []string{"linux"},
			Default: map[string]interface{}{"target_pid": 0, "duration": 10, "frequency": 1},
			Schema:  commonFields, MaxDuration: 300, MaxConcurrency: 1, TaskType: TaskTypeGeneric, ProfilerType: ProfilerPerf,
			Enabled: false, DisabledReason: "阶段 6 仅声明契约；安全沙箱与 Runner 后续接入",
		},
	}
}

func mergeDefaults(base, extra map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func taskKindByID(id string) (TaskKindDefinition, bool) {
	for _, kind := range taskKindDefinitions() {
		if kind.ID == id {
			return kind, true
		}
	}
	return TaskKindDefinition{}, false
}

func inferTaskKind(req CreateTaskReq) string {
	if req.TaskKind != "" {
		return req.TaskKind
	}
	switch req.ProfilerType {
	case ProfilerAsync:
		return TaskKindAsyncProfilerJava
	case ProfilerPprof:
		if strings.Contains(strings.ToLower(req.PprofURL), "/heap") {
			return TaskKindGoPprofHeap
		}
		return TaskKindGoPprof
	case ProfilerBPF:
		switch req.Event {
		case "io":
			return TaskKindEBPFIO
		case "sched":
			return TaskKindEBPFSched
		default:
			return TaskKindEBPFCPU
		}
	default:
		return TaskKindPerfCPU
	}
}

func normalizeAndValidateCollector(req *CreateTaskReq) error {
	kindID := inferTaskKind(*req)
	kind, ok := taskKindByID(kindID)
	if !ok {
		return fmt.Errorf("不支持的 task_kind=%s", kindID)
	}
	if !kind.Enabled {
		return fmt.Errorf("task_kind=%s 当前仅声明契约，尚未启用: %s", kindID, kind.DisabledReason)
	}
	req.TaskKind = kind.ID
	req.TaskType = kind.TaskType
	req.ProfilerType = kind.ProfilerType
	if req.Duration == 0 {
		req.Duration = uint64(numberDefault(kind.Default["duration"], 10))
	}
	if req.Frequency == 0 {
		req.Frequency = uint32(numberDefault(kind.Default["frequency"], 99))
	}
	if req.Callgraph == "" {
		req.Callgraph = stringDefault(kind.Default["callgraph"], "fp")
	}
	if req.Event == "" {
		req.Event = kind.Event
	}
	if req.ProfilerType == ProfilerPprof {
		if req.PprofURL == "" && (strings.HasPrefix(req.Event, "http://") || strings.HasPrefix(req.Event, "https://")) {
			req.PprofURL = req.Event
		}
		req.Event = ""
	}
	if req.Duration == 0 || req.Duration > kind.MaxDuration {
		return fmt.Errorf("采样时长需为 1-%d 秒", kind.MaxDuration)
	}
	if req.Frequency == 0 || req.Frequency > 10000 {
		return fmt.Errorf("采样频率需为 1-10000 Hz")
	}
	if err := validateTaskKindSpecific(kind, req); err != nil {
		return err
	}
	return nil
}

func validateTaskKindSpecific(kind TaskKindDefinition, req *CreateTaskReq) error {
	switch kind.ID {
	case TaskKindAsyncProfilerJava:
		if req.TargetPID <= 0 {
			return fmt.Errorf("async-profiler 必须指定 Java 目标 PID")
		}
		if req.Event == "" {
			req.Event = "cpu"
		}
	case TaskKindGoPprof, TaskKindGoPprofHeap:
		parsed, err := url.ParseRequestURI(req.PprofURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("pprof_url 必须是可访问的 http/https 完整 URL")
		}
		if kind.ID == TaskKindGoPprofHeap && !strings.Contains(strings.ToLower(req.PprofURL), "/heap") {
			return fmt.Errorf("go_pprof_heap 任务的 pprof_url 必须指向 /debug/pprof/heap")
		}
	case TaskKindEBPFCPU, TaskKindEBPFIO, TaskKindEBPFSched:
		if req.Event != kind.Event {
			return fmt.Errorf("%s 仅支持 event=%s", kind.DisplayName, kind.Event)
		}
	case TaskKindPerfCPU:
		if req.Event == "" {
			req.Event = "cpu-clock"
		}
	}
	return nil
}

func numberDefault(v interface{}, fallback int) int {
	switch n := v.(type) {
	case int:
		return n
	case uint64:
		return int(n)
	case uint32:
		return int(n)
	case float64:
		return int(n)
	default:
		return fallback
	}
}

func stringDefault(v interface{}, fallback string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return fallback
}

func (s *APIServer) agentSupportsTaskKind(targetIP string, kind TaskKindDefinition) bool {
	if targetIP == "" {
		return true
	}
	var agent struct {
		Capabilities []byte `gorm:"column:capabilities"`
	}
	if err := s.DB.Table("agent_infos").Select("capabilities").Where("ip_addr = ?", targetIP).Limit(1).Scan(&agent).Error; err != nil {
		return true
	}
	if len(agent.Capabilities) == 0 || string(agent.Capabilities) == "null" {
		return true
	}
	var caps []string
	if err := json.Unmarshal(agent.Capabilities, &caps); err != nil || len(caps) == 0 {
		return true
	}
	capSet := map[string]bool{}
	for _, cap := range caps {
		capSet[strings.ToLower(strings.TrimSpace(cap))] = true
	}
	for _, required := range kind.Capabilities {
		if capSet[strings.ToLower(required)] {
			return true
		}
	}
	if capSet[kind.ID] || capSet[kind.Runner] {
		return true
	}
	if isEBPFTaskKind(kind.ID) && hasEBPFCapability(capSet) {
		return true
	}
	return false
}

func isEBPFTaskKind(kindID string) bool {
	return kindID == TaskKindEBPFCPU || kindID == TaskKindEBPFIO || kindID == TaskKindEBPFSched
}

func hasEBPFCapability(capSet map[string]bool) bool {
	if capSet["ebpf"] || capSet["bpftrace"] {
		return true
	}
	for cap := range capSet {
		if strings.HasPrefix(cap, "ebpf_") {
			return true
		}
	}
	return false
}

func (s *APIServer) ListTaskKinds(c *gin.Context) {
	kinds := taskKindDefinitions()
	includeDisabled, _ := strconv.ParseBool(c.DefaultQuery("include_disabled", "false"))
	if !includeDisabled {
		enabled := make([]TaskKindDefinition, 0, len(kinds))
		for _, kind := range kinds {
			if kind.Enabled {
				enabled = append(enabled, kind)
			}
		}
		kinds = enabled
	}
	if targetIP := strings.TrimSpace(c.Query("target_ip")); targetIP != "" {
		filtered := make([]TaskKindDefinition, 0, len(kinds))
		for _, kind := range kinds {
			if s.agentSupportsTaskKind(targetIP, kind) {
				filtered = append(filtered, kind)
			}
		}
		kinds = filtered
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i].ID < kinds[j].ID })
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"task_kinds": kinds, "total": len(kinds)}})
}
