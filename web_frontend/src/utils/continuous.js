export const CONTINUOUS_SIGNALS = ['cpu_profile', 'io_latency', 'io_syscall_latency', 'sched_latency'];
export const DEFAULT_CONTINUOUS_SIGNALS = ['cpu_profile'];
// 哨兵规则目前只支持 eBPF histogram 类信号的固定阈值判异，与后端
// detectionSignalTaskKind（apiserver/server/detection.go）保持一致。
export const SENTINEL_SIGNALS = ['sched_latency', 'io_latency', 'io_syscall_latency'];

export function decodeJSONField(value, fallback = []) {
    if (value == null || value === '') return fallback;
    if (Array.isArray(value) || (typeof value === 'object' && value !== null)) return value;
    try {
        const text = String(value);
        if (text.startsWith('[') || text.startsWith('{')) return JSON.parse(text);
        return JSON.parse(decodeURIComponent(escape(window.atob(text))));
    } catch (error) {
        return fallback;
    }
}

export function continuousStateLabel(value) {
    return ({
        pending: '待启动', running: '运行中', waiting: '等待进程', degraded: '降级运行',
        stopping: '停止中', stopped: '已停止', error: '异常', offline: 'Agent 离线',
    })[value] || value || '未知';
}

export function continuousStateColor(value) {
    return ({
        pending: ['#eef2ff', '#3538cd'], running: ['#ecfdf3', '#067647'], waiting: ['#fffaeb', '#b54708'],
        degraded: ['#fff7ed', '#c2410c'], stopping: ['#f2f4f7', '#475467'], stopped: ['#f2f4f7', '#475467'],
        error: ['#fff1f3', '#c01048'], offline: ['#fff1f3', '#b42318'],
    })[value] || ['#f2f4f7', '#475467'];
}

export function signalLabel(value) {
    return ({ cpu_profile: 'CPU', io_latency: '块 IO', io_syscall_latency: '系统调用 IO', sched_latency: '调度' })[value] || value;
}

export function formatRelativeTime(value) {
    if (!value) return '暂无数据';
    const time = new Date(value).getTime();
    if (!Number.isFinite(time)) return String(value);
    const seconds = Math.max(0, Math.floor((Date.now() - time) / 1000));
    if (seconds < 60) return `${seconds} 秒前`;
    if (seconds < 3600) return `${Math.floor(seconds / 60)} 分钟前`;
    if (seconds < 86400) return `${Math.floor(seconds / 3600)} 小时前`;
    return new Date(value).toLocaleString();
}

export function formatBytes(value) {
    const bytes = Number(value) || 0;
    if (bytes < 1024) return `${bytes} B`;
    if (bytes < 1024 ** 2) return `${(bytes / 1024).toFixed(1)} KB`;
    if (bytes < 1024 ** 3) return `${(bytes / 1024 ** 2).toFixed(1)} MB`;
    return `${(bytes / 1024 ** 3).toFixed(1)} GB`;
}

// ============================================================
// 阶段六：selector 模型辅助（pid_instance / exe_all_instances /
// cgroup / container_id）
// ============================================================

export const SELECTOR_MODES = ['pid_instance', 'exe_all_instances', 'cgroup', 'container_id'];

export function selectorModeLabel(mode) {
    return ({
        pid_instance: '单进程实例', exe_all_instances: '同路径全部实例',
        cgroup: 'cgroup', container_id: '容器',
    })[mode] || mode || '未知';
}

// selector 精确身份描述（用于列表/详情展示）。
export function selectorIdentity(session) {
    const mode = session?.selector_mode || 'exe_all_instances';
    const params = decodeJSONField(session?.selector_params, {});
    if (mode === 'pid_instance') {
        const pid = Number(params?.pid) || 0;
        const start = Number(params?.process_start_ms) || 0;
        const exe = params?.exe || session?.selector_exe || '';
        return { mode, exe, detail: `PID ${pid} · ${formatStartMs(start)}`, follow: '不跟随重启：进程退出后进入等待，PID 复用不会误采新进程' };
    }
    if (mode === 'cgroup') {
        return { mode, exe: '', detail: params?.cgroup || '', follow: '跟随 cgroup 内进程变化' };
    }
    if (mode === 'container_id') {
        return { mode, exe: '', detail: params?.container_id || '', follow: '跟随容器内进程变化' };
    }
    return { mode, exe: session?.selector_exe || '', detail: '全部实例', follow: '自动跟随同路径新实例' };
}

export function formatStartMs(value) {
    const ms = Number(value);
    if (!Number.isFinite(ms) || ms <= 0) return 'start 未知';
    const date = new Date(ms);
    return Number.isNaN(date.getTime()) ? `start ${value}` : date.toLocaleString();
}
