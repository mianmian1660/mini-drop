const COLLECTOR_LABELS = {
    perf_cpu: 'perf CPU 火焰图',
    async_profiler_java: 'Java async-profiler',
    go_pprof: 'Go pprof CPU',
    go_pprof_heap: 'Go pprof Heap',
    ebpf_cpu: 'eBPF CPU 火焰图',
    ebpf_io: 'eBPF IO 延迟',
    ebpf_sched: 'eBPF 调度延迟',
    java_heap: 'Java Heap Dump',
    python_py_spy: 'Python py-spy',
    python_memray: 'Python memray',
    gperftools_cpu: 'gperftools CPU',
    bolt_profile: 'BOLT 优化 Profile',
    script_diagnostic: '受限脚本诊断',
};

const CAPABILITY_LABELS = {
    perf_cpu: 'perf CPU',
    perf: 'perf',
    async_profiler_java: 'Java async-profiler',
    'async-profiler': 'async-profiler',
    java: 'Java',
    go_pprof: 'Go pprof',
    pprof: 'pprof',
    ebpf_cpu: 'eBPF CPU',
    ebpf_io: 'eBPF IO',
    ebpf_sched: 'eBPF 调度',
    ebpf: 'eBPF',
    bpftrace: 'bpftrace',
};

// 采集能力的中文名称与用途说明（capability 展示用）。
// 未覆盖的能力回退到 capabilityLabel 的短名，不展示原始内部字段名。
const CAPABILITY_DESCRIPTIONS = {
    perf_cpu: { name: 'CPU 采样', usage: 'perf 火焰图，定位 CPU 热点' },
    perf: { name: 'CPU 采样', usage: 'perf 火焰图，定位 CPU 热点' },
    'async-profiler': { name: 'Java 采样', usage: 'Java async-profiler 火焰图' },
    async_profiler_java: { name: 'Java 采样', usage: 'Java async-profiler 火焰图' },
    java: { name: 'Java 采样', usage: 'Java async-profiler 火焰图' },
    go_pprof: { name: 'Go 采样', usage: 'Go pprof CPU 火焰图' },
    go_pprof_heap: { name: 'Go 堆分析', usage: 'Go pprof Heap 堆快照' },
    pprof: { name: 'Go 采样', usage: 'Go pprof CPU 火焰图' },
    ebpf_cpu: { name: 'eBPF CPU 采样', usage: 'eBPF CPU 火焰图' },
    ebpf_io: { name: 'eBPF IO', usage: 'eBPF IO 延迟分析' },
    ebpf_sched: { name: 'eBPF 调度', usage: 'eBPF 调度延迟分析' },
    ebpf: { name: 'eBPF 采样', usage: 'eBPF 火焰图' },
    bpftrace: { name: 'bpftrace 脚本', usage: '受限脚本诊断' },
    native_cp_perf_event: { name: '内核符号', usage: 'perf_event 可读，支持内核符号解析' },
    native_cp_perf: { name: '内核符号', usage: 'perf 命令可用，支持内核符号解析' },
    native_cp_btf: { name: '内核符号', usage: 'BTF 可用，支持内核符号解析' },
    native_cp_ebpf_fs: { name: 'eBPF 采样', usage: 'eBPF 文件系统可用' },
    native_cp_tracefs: { name: 'eBPF 采样', usage: 'tracefs 可用' },
    native_cp_tracepoint_block: { name: 'eBPF IO', usage: '块设备 tracepoint 可用' },
    native_cp_tracepoint_sched: { name: 'eBPF 调度', usage: '调度 tracepoint 可用' },
    native_cp_bpftrace: { name: 'bpftrace 脚本', usage: 'bpftrace 可用，支持受限脚本诊断' },
    native_cp_memlock_unlimited: { name: 'eBPF 内存', usage: 'memlock 无限制，支持 eBPF 程序加载' },
    native_cp_sampler_perf_event: { name: '持续采集', usage: 'perf_event 采样器就绪' },
    native_cp_sampler_core_ready: { name: '持续采集', usage: '核心采样器就绪' },
    native_cp_sampler_bpftrace_ready: { name: '持续采集', usage: 'bpftrace 采样器就绪' },
    lang_go_goresym: { name: 'Go 符号', usage: 'Go 符号解析可用' },
    lang_java_asprof: { name: 'Java 符号', usage: 'Java 符号解析可用' },
    lang_python_pyspy: { name: 'Python 采样', usage: 'py-spy 可用' },
    lang_runtime_perf_map: { name: '运行时符号', usage: 'perf map 运行时符号可用' },
    perf_dwarf_call_graph: { name: 'DWARF 调用栈', usage: 'perf DWARF 调用栈可用' },
};

export function capabilityDescription(capability) {
    const key = String(capability || '');
    const desc = CAPABILITY_DESCRIPTIONS[key];
    if (!desc) return null;
    return desc;
}

function parseJSON(text) {
    try {
        return JSON.parse(text);
    } catch (_) {
        return null;
    }
}

function decodeBase64(text) {
    if (typeof text !== 'string' || !/^[A-Za-z0-9+/]+={0,2}$/.test(text) || text.length % 4 !== 0) {
        return '';
    }
    try {
        if (typeof window !== 'undefined' && typeof window.atob === 'function') {
            const binary = window.atob(text);
            try {
                const bytes = Uint8Array.from(binary, char => char.charCodeAt(0));
                return new TextDecoder('utf-8').decode(bytes);
            } catch (_) {
                return binary;
            }
        }
        return '';
    } catch (_) {
        try {
            return window.atob(text);
        } catch (__) {
            return '';
        }
    }
}

export function parseMaybeEncodedJSON(value, fallback = null) {
    if (value === undefined || value === null || value === '') return fallback;
    if (typeof value === 'object') return value;
    const text = String(value).trim();
    const direct = parseJSON(text);
    if (direct !== null) return direct;
    const decoded = decodeBase64(text);
    if (!decoded) return fallback;
    const parsed = parseJSON(decoded);
    return parsed === null ? fallback : parsed;
}

export function parseStringList(value) {
    const parsed = parseMaybeEncodedJSON(value, null);
    if (Array.isArray(parsed)) return parsed.map(item => String(item)).filter(Boolean);
    if (typeof value === 'string' && value.trim()) {
        const text = value.trim();
        if (/^[A-Za-z0-9+/]+={0,2}$/.test(text) && text.length >= 16) return ['能力解析失败'];
        // 兼容旧 Agent/接口返回的逗号或换行分隔能力字符串。
        // JSON 数组已在上方处理；这里仅拆分纯文本，避免能力字典收到
        // "perf_cpu,ebpf_io" 这种无法匹配的整体值。
        return text.split(/[\s,;，；]+/).map(item => item.trim()).filter(Boolean);
    }
    return [];
}

export function parseRequestParams(value) {
    const parsed = parseMaybeEncodedJSON(value, {});
    return parsed && typeof parsed === 'object' && !Array.isArray(parsed) ? parsed : {};
}

export function capabilityLabel(capability) {
    const key = String(capability || '');
    return CAPABILITY_LABELS[key] || key;
}

export function collectorLabelByKind(taskKind) {
    const key = String(taskKind || '');
    return COLLECTOR_LABELS[key] || key || '';
}

export function collectorLabelFromTask(task) {
    const kindLabel = collectorLabelByKind(task?.task_kind);
    if (kindLabel) return kindLabel;
    const params = parseRequestParams(task?.request_params);
    const event = String(params.event || '').toLowerCase();
    const taskType = Number(task?.type);
    const profilerType = Number(task?.profiler_type);

    if (taskType === 5) {
        if (event === 'sched') return 'eBPF 调度延迟';
        if (event === 'io' || event === 'blk') return 'eBPF IO 延迟';
        return 'eBPF CPU 火焰图';
    }
    if (profilerType === 1) return 'Java async-profiler';
    if (profilerType === 2) return 'Go pprof';
    if (profilerType === 3) return 'eBPF CPU 火焰图';
    return 'perf CPU 火焰图';
}
