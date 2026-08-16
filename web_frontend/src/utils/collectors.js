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
        return [text];
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
