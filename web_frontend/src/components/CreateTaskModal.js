import React, { useState, useEffect, useMemo, useCallback } from 'react';
import { Link } from 'react-router-dom';
import { tasks, agents, schedules, taskKinds } from '../api';
import { capabilityLabel, parseStringList } from '../utils/collectors';
import { intervalHumanLabel } from '../utils/schedule';
import InfoTooltip from './InfoTooltip';

const S = {
    overlay: { position: 'fixed', inset: 0, zIndex: 1000, background: 'rgba(15, 23, 42, 0.45)', display: 'flex', alignItems: 'flex-start', justifyContent: 'center', padding: '48px 16px 24px', overflowY: 'auto' },
    card: { width: 'min(960px, 100%)', background: '#fff', borderRadius: 8, padding: 24, border: '1px solid #d0d7de', boxShadow: '0 24px 64px rgba(15, 23, 42, 0.28)' },
    header: { display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', gap: 16, marginBottom: 18, borderBottom: '1px solid #edf0f3', paddingBottom: 12 },
    title: { margin: 0, fontSize: 20, color: '#111827' },
    close: { background: '#f8fafc', color: '#475467', border: '1px solid #d0d7de', width: 34, height: 34, borderRadius: 6, cursor: 'pointer', fontSize: 18, lineHeight: 1 },
    section: { background: '#f8f9ff', borderRadius: 8, padding: 16, marginTop: 8, border: '1px solid #e0e4ff' },
    input: { width: '100%', padding: '8px 12px', border: '1px solid #ddd', borderRadius: 4, fontSize: 14, marginBottom: 12, boxSizing: 'border-box' },
    select: { width: '100%', padding: '8px 12px', border: '1px solid #ddd', borderRadius: 4, fontSize: 14, marginBottom: 12, boxSizing: 'border-box', background: '#fff' },
    label: { display: 'block', marginBottom: 4, fontWeight: 'bold', fontSize: 13, color: '#555' },
    btn: { background: '#4a6cf7', color: '#fff', border: 'none', padding: '10px 20px', borderRadius: 6, cursor: 'pointer', fontSize: 14 },
    linkBtn: { background: 'transparent', color: '#4a6cf7', border: 'none', padding: 0, cursor: 'pointer', fontSize: 12, fontWeight: 'bold' },
    kindGrid: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(180px, 1fr))', gap: 8, marginBottom: 8 },
    kindCard: (active, disabled = false) => ({
        textAlign: 'left', background: disabled ? '#f8fafc' : active ? '#eef3ff' : '#fff', border: active ? '2px solid #4a6cf7' : '1px solid #d0d7de',
        borderRadius: 8, padding: 10, cursor: disabled ? 'not-allowed' : 'pointer', minHeight: 112, boxSizing: 'border-box',
        opacity: disabled ? 0.72 : 1,
    }),
    kindTitle: { fontWeight: 'bold', color: '#111827', fontSize: 14, marginBottom: 6 },
    kindMeta: { display: 'flex', gap: 6, flexWrap: 'wrap', marginBottom: 6 },
    pill: { display: 'inline-flex', alignItems: 'center', fontSize: 11, lineHeight: '16px', padding: '1px 6px', borderRadius: 999, background: '#f3f4f6', color: '#475467' },
    kindDesc: { margin: 0, color: '#667085', fontSize: 12, lineHeight: 1.35 },
    err: { color: '#f44336', fontSize: 13, marginTop: 12 },
    ok: { color: '#4caf50', fontSize: 13, marginTop: 12 },
    hint: { fontSize: 11, color: '#888', marginTop: 2, marginBottom: 8 },
    warn: { fontSize: 12, color: '#b42318', background: '#fff6f5', border: '1px solid #fda29b', borderRadius: 6, padding: '8px 10px', margin: '0 0 12px' },
    chk: { display: 'flex', alignItems: 'center', gap: 8, marginBottom: 12 },
    presetBtn: (active) => ({
        padding: '4px 10px', fontSize: 12, borderRadius: 4, cursor: 'pointer',
        background: active ? '#4a6cf7' : '#e0e0e0', color: active ? '#fff' : '#333', border: 'none',
    }),
};

// 周期性深度采样的采样间隔预设（分钟 → 秒）。周期变化统一由
// interval_seconds 驱动，采样时长默认 = 间隔 - 10 秒，保证窗口不重叠。
const INTERVAL_PRESETS = [
    { label: '每 1 分钟', intervalSeconds: 60, frequency: 19 },
    { label: '每 5 分钟', intervalSeconds: 300, frequency: 19 },
    { label: '每 10 分钟', intervalSeconds: 600, frequency: 19 },
    { label: '每 30 分钟', intervalSeconds: 1800, frequency: 19 },
];

// defaultDurationForInterval 间隔变化时自动推荐的采样时长（间隔 - 10 秒）。
function defaultDurationForInterval(intervalSeconds) {
    const value = Number(intervalSeconds);
    if (!Number.isFinite(value) || value <= 0) return 10;
    return Math.max(1, value - 10);
}

function valuesFromKind(kind) {
    const out = {};
    (kind?.schema || []).forEach(field => {
        if (field.default !== undefined) out[field.name] = field.default;
    });
    return { ...out, ...(kind?.default || {}) };
}

function coerceField(field, value) {
    if (field.type === 'number') return parseInt(value, 10) || 0;
    if (field.type === 'boolean') return Boolean(value);
    return String(value ?? '');
}

function chooseDefaultKind(list, currentID) {
    if (!Array.isArray(list) || list.length === 0) return null;
    return list.find(k => k.id === currentID)
        || list.find(k => k.id === 'perf_cpu')
        || list.find(k => k.id === 'go_pprof')
        || list[0];
}

function kindDescription(kind) {
    switch (kind?.id) {
        case 'perf_cpu':
            return '本机 perf CPU 采样，适合生成 CPU 火焰图。';
        case 'async_profiler_java':
            return '采集 Java 进程 CPU/wall/alloc，需要填写目标 PID。';
        case 'go_pprof':
            return '拉取 Go pprof profile，需要填写 Profile URL。';
        case 'ebpf_cpu':
            return '使用 eBPF 采集 CPU 栈，适合内核态低开销观测。';
        case 'ebpf_io':
            return '采集块 IO 延迟与调用栈，适合排查磁盘抖动。';
        case 'ebpf_sched':
            return '采集调度延迟，适合排查线程等待和运行队列问题。';
        default:
            return kind?.description || '按后端 TaskKind 契约创建采样任务。';
    }
}

function kindNeedsURL(kind) {
    return (kind?.schema || []).some(field => field.name === 'pprof_url' || field.type === 'url');
}

function kindNeedsPID(kind) {
    return (kind?.schema || []).some(field => field.name === 'target_pid' && field.required);
}

// 字段标题覆盖：后端 schema 标题较泛化，按 TaskKind + 字段名显示更明确的标题。
const FIELD_LABEL_OVERRIDES = {
    'async_profiler_java:target_pid': 'Java 宿主机 PID',
    'java_heap:target_pid': 'Java 宿主机 PID',
    'go_pprof:pprof_url': 'Go CPU Profile URL',
    'go_pprof_heap:pprof_url': 'Go Heap Profile URL',
};

function fieldLabel(kind, field) {
    return FIELD_LABEL_OVERRIDES[`${kind?.id}:${field.name}`] || field.label;
}

// 单次采样关键字段的悬停提示，内容根据当前 TaskKind 显示。
const FIELD_TOOLTIPS = {
    'async_profiler_java:target_pid': (
        <>
            Java 单次采样使用 async-profiler 附加到指定 JVM，因此必须填写目标 Java 进程的宿主机 PID。
            请勿填写容器内 PID；JVM 重启后 PID 可能变化，需要重新确认。
        </>
    ),
    'java_heap:target_pid': (
        <>
            Java 单次采样使用 async-profiler 附加到指定 JVM，因此必须填写目标 Java 进程的宿主机 PID。
            请勿填写容器内 PID；JVM 重启后 PID 可能变化，需要重新确认。
        </>
    ),
    'go_pprof:pprof_url': (
        <>
            Go 单次采样通过 HTTP 拉取应用暴露的 pprof CPU Profile，因此需要填写 Agent 能访问的完整 URL，例如 <code>http://目标地址:6060/debug/pprof/profile</code>。
            系统无法仅通过 PID 判断 pprof 的监听地址和端口。
        </>
    ),
    'go_pprof_heap:pprof_url': (
        <>
            Go Heap 通过 HTTP 获取当前堆快照，因此需要填写 Agent 能访问的完整 URL，例如 <code>http://目标地址:6060/debug/pprof/heap</code>。
            目标 Go 应用必须已启用 <code>net/http/pprof</code>。
        </>
    ),
};

function fieldTooltip(kind, field) {
    return FIELD_TOOLTIPS[`${kind?.id}:${field.name}`] || null;
}

function capabilityMatches(kind, capabilities) {
    if (!kind) return true;
    const capSet = new Set(capabilities.map(cap => String(cap).toLowerCase()));
    const candidates = [kind.id, kind.runner, ...(kind.capabilities || [])].map(item => String(item || '').toLowerCase());
    return candidates.some(item => item && capSet.has(item));
}

function missingCapabilityText(kind, capabilities) {
    if (!kind || capabilityMatches(kind, capabilities)) return '';
    const labels = (kind.capabilities || [kind.runner || kind.id]).map(capabilityLabel).filter(Boolean);
    return `当前 Agent 未声明 ${labels.join(' / ')} capability`;
}

export default function CreateTaskModal({ onClose, onSuccess, initialTargetIP = '', lockTargetIP = false, scheduleSuccessLink = '/timeline' }) {
    const [f, setF] = useState({
        name: '', target_ip: initialTargetIP || '', task_kind: '', target_pid: 0, duration: 10, frequency: 99,
        callgraph: 'fp', event: 'cpu-clock', pprof_url: '',
        // 周期计划：间隔（秒）+ 开始时间（立即或指定）；不再使用 cron。
        continuous: false, interval_seconds: 300, start_mode: 'now', start_at: '', custom_interval_min: '',
    });
    // durationTouched：用户手工调整过采样时长后，切换间隔不再自动覆盖时长，
    // 立即显示重叠风险（时长 >= 间隔时禁止提交）。
    const [durationTouched, setDurationTouched] = useState(false);
    const [sub, setSub] = useState(false);
    const [err, setErr] = useState('');
    const [ok, setOk] = useState('');
    const [cid, setCid] = useState('');
    const [isSch, setIsSch] = useState(false);
    const [agentList, setAgentList] = useState([]);
    const [kindList, setKindList] = useState([]);
    const [catalogKinds, setCatalogKinds] = useState([]);
    const [aload, setAload] = useState(true);
    const [kload, setKload] = useState(true);
    const [taskKindError, setTaskKindError] = useState('');

    useEffect(() => {
        agents.list().then(r => {
            if (r.code === 0) {
                const list = r.data?.agents || [];
                setAgentList(list);
                const on = list.filter(a => a.online);
                if (initialTargetIP) {
                    setF(p => ({ ...p, target_ip: initialTargetIP }));
                } else if (on.length > 0) {
                    setF(p => p.target_ip ? p : ({ ...p, target_ip: on[0].ip_addr }));
                }
            }
        }).catch(() => { }).finally(() => setAload(false));

    }, [initialTargetIP]);

    useEffect(() => {
        taskKinds.list().then(r => {
            if (r.code === 0) setCatalogKinds(r.data?.task_kinds || []);
        }).catch(() => {});
    }, []);

    const loadTaskKinds = useCallback((targetIP) => {
        setKload(true);
        setTaskKindError('');
        taskKinds.list(targetIP ? { target_ip: targetIP } : {}).then(r => {
            if (r.code !== 0) throw new Error(r.message || '加载 TaskKind 失败');
            const list = r.data?.task_kinds || [];
            setKindList(list);
            setF(p => {
                if (list.length === 0) return { ...p, task_kind: '' };
                const kind = chooseDefaultKind(list, p.task_kind);
                return { ...p, task_kind: kind.id, ...valuesFromKind(kind) };
            });
        }).catch(e => {
            setKindList([]);
            setTaskKindError(e.message || '任务类型元数据加载失败');
        }).finally(() => setKload(false));
    }, []);

    useEffect(() => {
        loadTaskKinds(f.target_ip);
    }, [f.target_ip, loadTaskKinds]);

    const selectedKind = useMemo(
        () => kindList.find(k => k.id === f.task_kind) || null,
        [kindList, f.task_kind],
    );
    const selectedAgent = useMemo(
        () => agentList.find(a => a.ip_addr === f.target_ip) || null,
        [agentList, f.target_ip],
    );
    const selectedCapabilities = useMemo(
        () => parseStringList(selectedAgent?.capabilities),
        [selectedAgent],
    );
    const unavailableKinds = useMemo(() => {
        const available = new Set(kindList.map(kind => kind.id));
        return catalogKinds.filter(kind => kind.enabled !== false && !available.has(kind.id));
    }, [catalogKinds, kindList]);

    const up = (k, v) => {
        setErr('');
        if (k === 'target_ip' || k === 'task_kind') setTaskKindError('');
        if (k === 'duration') setDurationTouched(true);
        setF(p => {
            if (k === 'task_kind') {
                const kind = kindList.find(item => item.id === v);
                return { ...p, task_kind: v, ...valuesFromKind(kind) };
            }
            const n = { ...p, [k]: v };
            if (k === 'continuous' && v === true && !p.continuous) {
                const preset = INTERVAL_PRESETS[1]; // 默认每 5 分钟
                n.interval_seconds = preset.intervalSeconds;
                n.duration = defaultDurationForInterval(preset.intervalSeconds);
                n.frequency = preset.frequency;
                n.custom_interval_min = '';
            }
            return n;
        });
    };

    // 应用间隔预设：周期变化由 interval_seconds 驱动；未手工调整采样时长时
    // 自动重算默认时长，否则保留用户值并显示重叠风险。
    const applyIntervalPreset = (preset) => setF(p => ({
        ...p,
        interval_seconds: preset.intervalSeconds,
        custom_interval_min: '',
        duration: durationTouched ? p.duration : defaultDurationForInterval(preset.intervalSeconds),
        frequency: preset.frequency,
    }));

    const applyCustomInterval = (minutes) => {
        const min = Number(minutes);
        setF(p => {
            if (!Number.isFinite(min) || min <= 0) return { ...p, custom_interval_min: minutes };
            const intervalSeconds = Math.round(min * 60);
            return {
                ...p,
                custom_interval_min: minutes,
                interval_seconds: intervalSeconds,
                duration: durationTouched ? p.duration : defaultDurationForInterval(intervalSeconds),
            };
        });
    };

    // 下一次采集时间预估：立即开始 → 当前时间 + 间隔；指定开始时间且在未来
    // → 该时间；否则对齐到最近未来槽位（与后端 intervalNextRun 一致）。
    const nextRunEstimate = useMemo(() => {
        if (!f.continuous) return null;
        const intervalMs = (Number(f.interval_seconds) || 0) * 1000;
        if (intervalMs <= 0) return null;
        const now = new Date();
        let base = now;
        if (f.start_mode === 'schedule' && f.start_at) {
            const start = new Date(f.start_at);
            if (!Number.isNaN(start.getTime()) && start > now) base = start;
        }
        let next = new Date(base.getTime());
        while (next <= now) next = new Date(next.getTime() + intervalMs);
        return next;
    }, [f.continuous, f.interval_seconds, f.start_mode, f.start_at]);

    const overlapRisk = f.continuous && Number(f.duration) >= Number(f.interval_seconds);

    const renderField = (field) => {
        const value = f[field.name] ?? field.default ?? '';
        if (field.type === 'select') {
            return (
                <select style={S.select} value={value} onChange={e => up(field.name, e.target.value)}>
                    {(field.options || []).map(option => <option key={option} value={option}>{option}</option>)}
                </select>
            );
        }
        if (field.type === 'boolean') {
            return (
                <label style={S.chk}>
                    <input type="checkbox" checked={Boolean(value)} onChange={e => up(field.name, e.target.checked)} />
                    <span>{field.label}</span>
                </label>
            );
        }
        return (
            <>
                <input
                    style={S.input}
                    type={field.type === 'url' ? 'url' : field.type}
                    min={field.min}
                    max={field.max}
                    placeholder={field.placeholder || ''}
                    id={field.name}
                    name={field.name}
                    value={value}
                    onChange={e => up(field.name, coerceField(field, e.target.value))}
                />
                {field.placeholder && <p style={S.hint}>示例：{field.placeholder}</p>}
            </>
        );
    };

    const submit = async () => {
        if (!f.name.trim()) { setErr('请输入任务名称'); return; }
        if (!f.target_ip) { setErr('请选择目标 Agent'); return; }
        if (!selectedKind) { setErr('请选择任务类型'); return; }
        const dur = parseInt(f.duration, 10) || 10;
        if (dur < 1 || dur > selectedKind.max_duration) { setErr(`时长需为 1-${selectedKind.max_duration}s`); return; }
        if (f.continuous) {
            const interval = Number(f.interval_seconds) || 0;
            if (interval < 60) { setErr('采样间隔不能小于 1 分钟'); return; }
            if (dur >= interval) {
                setErr(`采样时长(${dur}s)需小于采样间隔(${interval}s)，否则相邻窗口会重叠`);
                return;
            }
            if (f.start_mode === 'schedule' && f.start_at) {
                const start = new Date(f.start_at);
                if (Number.isNaN(start.getTime())) { setErr('开始时间格式无效，请重新选择'); return; }
            }
        }
        if (selectedKind.id === 'async_profiler_java' && (parseInt(f.target_pid, 10) || 0) < 1) {
            setErr('Java async-profiler 需要填写大于 0 的 Java 目标 PID');
            return;
        }
        for (const field of selectedKind.schema || []) {
            const value = f[field.name];
            if (field.required && (value === '' || value === undefined || value === null || value === 0 && field.min > 0)) {
                setErr(`请填写${field.label}`);
                return;
            }
        }
        if (selectedKind.id === 'go_pprof') {
            const pprofURL = String(f.pprof_url || '').trim();
            if (!/^https?:\/\//i.test(pprofURL)) {
                setErr('Go pprof 需要填写 http/https 开头的完整 Profile URL');
                return;
            }
        }

        setSub(true); setErr(''); setOk('');
        const payload = {
            name: f.name.trim(),
            target_ip: f.target_ip,
            task_kind: f.task_kind,
            target_pid: parseInt(f.target_pid, 10) || 0,
            duration: dur,
            frequency: parseInt(f.frequency, 10) || 99,
            callgraph: f.callgraph,
            event: f.event,
            subprocess: Boolean(f.subprocess),
            pprof_url: String(f.pprof_url || '').trim(),
        };

        try {
            if (f.continuous) {
                let startAt;
                if (f.start_mode === 'schedule' && f.start_at) {
                    // datetime-local 无时区，按浏览器本地时区转 ISO（UTC）后提交
                    startAt = new Date(f.start_at).toISOString();
                }
                const r = await schedules.create({
                    ...payload,
                    interval_seconds: Number(f.interval_seconds),
                    start_at: startAt,
                });
                if (r.code === 0) {
                    setCid(r.data?.sid || ''); setIsSch(true); setOk('周期性深度采样已创建！');
                    setTimeout(() => onSuccess?.(), 3000);
                } else setErr(r.message || '创建失败');
            } else {
                const r = await tasks.create(payload);
                if (r.code === 0) {
                    setCid(r.data?.tid || ''); setIsSch(false); setOk('任务创建成功！');
                    setTimeout(() => onSuccess?.(), 2000);
                } else setErr(r.message || '创建失败');
            }
        } catch (e) { setErr('请求失败: ' + (e.message || '无法连接后端')); }
        finally { setSub(false); }
    };

    return (
        <div style={S.overlay} onClick={onClose}>
            <div style={S.card} onClick={e => e.stopPropagation()}>
                <div style={S.header}>
                    <div>
						<h3 style={S.title}>新建单次采样</h3>
                        <div style={S.hint}>任务类型与参数由后端 TaskKind 契约加载。</div>
                    </div>
                    <button style={S.close} onClick={onClose} disabled={sub} aria-label="关闭">×</button>
                </div>

                <div style={{ marginBottom: 16 }}>
                    <label style={S.label}>目标 Agent *</label>
                    {aload ? <p style={{ fontSize: 12, color: '#999' }}>加载中...</p>
                        : agentList.length === 0 ? <p style={{ fontSize: 12, color: '#f44' }}>没有在线 Agent</p>
                            : <select style={S.select} value={f.target_ip} onChange={e => up('target_ip', e.target.value)} disabled={lockTargetIP}>
                                <option value="">-- 选择 Agent --</option>
                                {agentList.map(a => <option key={a.ip_addr} value={a.ip_addr}>{a.hostname} ({a.ip_addr}) {a.online ? '在线' : '离线'}</option>)}
                            </select>}
                    {lockTargetIP && <p style={S.hint}>已锁定为当前主机。</p>}
                </div>

                <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16 }}>
                    <div><label style={S.label}>任务名称 *</label><input style={S.input} placeholder="CPU采样-nginx" value={f.name} onChange={e => up('name', e.target.value)} /></div>
                    <div style={{ gridColumn: '1 / -1' }}>
                        <label style={S.label}>任务类型 *</label>
                        {kload ? <p style={{ fontSize: 12, color: '#999' }}>加载中...</p> : taskKindError ? (
                            <p style={S.warn}>
                                {taskKindError}
                                <button type="button" style={{ ...S.linkBtn, marginLeft: 8 }} onClick={() => loadTaskKinds(f.target_ip)}>重试</button>
                            </p>
                        ) : (
                            <>
                                <div style={S.kindGrid}>
                                    {kindList.map(kind => (
                                        <button
                                            type="button"
                                            key={kind.id}
                                            style={S.kindCard(f.task_kind === kind.id)}
                                            onClick={() => up('task_kind', kind.id)}
                                        >
                                            <div style={S.kindTitle}>{kind.display_name}</div>
                                            <div style={S.kindMeta}>
                                                <span style={S.pill}>{kind.runner || 'runner'}</span>
                                                {kindNeedsPID(kind) && <span style={{ ...S.pill, background: '#ecfdf3', color: '#027a48' }}>需要 PID</span>}
                                                {kindNeedsURL(kind) && <span style={{ ...S.pill, background: '#fff7ed', color: '#c2410c' }}>需要 URL</span>}
                                            </div>
                                            <p style={S.kindDesc}>{kindDescription(kind)}</p>
                                        </button>
                                    ))}
                                    {unavailableKinds.map(kind => (
                                        <button
                                            type="button"
                                            key={kind.id}
                                            style={S.kindCard(false, true)}
                                            disabled
                                            title={missingCapabilityText(kind, selectedCapabilities)}
                                        >
                                            <div style={S.kindTitle}>{kind.display_name}</div>
                                            <div style={S.kindMeta}>
                                                <span style={S.pill}>{kind.runner || 'runner'}</span>
                                                <span style={{ ...S.pill, background: '#fef3c7', color: '#92400e' }}>当前不可用</span>
                                            </div>
                                            <p style={S.kindDesc}>{missingCapabilityText(kind, selectedCapabilities) || '当前 Agent 暂不支持该采集器。'}</p>
                                        </button>
                                    ))}
                                </div>
                            </>
                        )}
                        {!kload && !taskKindError && f.target_ip && kindList.length === 0 && (
                            <p style={S.warn}>目标 Agent {f.target_ip} 没有匹配的 TaskKind。请切换 Agent，或检查 drop_agent capability 上报。</p>
                        )}
                    </div>
                    {(selectedKind?.schema || [])
                        // 前端不再展示"包含子进程"字段（后端 schema 仍保留，提交参数不变）
                        .filter(field => field.name !== 'subprocess')
                        .map(field => (
                        <div key={field.name}>
                            {field.type !== 'boolean' && (
                                <label style={S.label}>
                                    {fieldLabel(selectedKind, field)}{field.required ? ' *' : ''}
                                    {fieldTooltip(selectedKind, field) && <InfoTooltip>{fieldTooltip(selectedKind, field)}</InfoTooltip>}
                                </label>
                            )}
                            {renderField(field)}
                        </div>
                    ))}
                </div>

                <p style={S.hint}>将生成: {selectedKind?.display_name || '未选择任务类型'}</p>

                <div style={{ ...S.section, background: f.continuous ? '#e8f0ff' : '#fafafa', border: f.continuous ? '1px solid #4a6cf7' : '1px solid #e0e0e0' }}>
                    <label style={S.chk}>
                        <input type="checkbox" checked={f.continuous} onChange={e => up('continuous', e.target.checked)} />
                        <span style={{ fontWeight: 'bold', fontSize: 14 }}>周期性深度采样 (Periodic Deep Sampling)</span>
                    </label>
                    {f.continuous && (
                        <div>
                            <label style={S.label}>采样间隔（每隔多久采集一次）<InfoTooltip>系统会按这个间隔自动创建一次深度采样窗口，采样时长必须短于间隔。</InfoTooltip></label>
                            <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', marginBottom: 8, alignItems: 'center' }}>
                                {INTERVAL_PRESETS.map(p => (
                                    <button key={p.intervalSeconds} style={S.presetBtn(f.interval_seconds === p.intervalSeconds && !f.custom_interval_min)}
                                        onClick={() => applyIntervalPreset(p)}>{p.label}</button>
                                ))}
                                <span style={{ display: 'inline-flex', alignItems: 'center', gap: 4 }}>
                                    <input
                                        type="number"
                                        min={1}
                                        style={{ width: 64, padding: '4px 8px', border: '1px solid #ddd', borderRadius: 4, fontSize: 12 }}
                                        placeholder="自定义"
                                        value={f.custom_interval_min}
                                        aria-label="自定义采样间隔（分钟）"
                                        onChange={e => applyCustomInterval(e.target.value)}
                                    />
                                    <span style={{ fontSize: 12, color: '#667085' }}>分钟</span>
                                </span>
                            </div>

                            <label style={S.label}>开始时间<InfoTooltip>选择立即开始，或指定第一次采集的时间；选择过去的时间会尽快开始。</InfoTooltip></label>
                            <div style={{ display: 'flex', gap: 16, flexWrap: 'wrap', alignItems: 'center', marginBottom: 12 }}>
                                <label style={{ display: 'inline-flex', gap: 6, alignItems: 'center', fontSize: 13 }}>
                                    <input type="radio" name="start-mode" checked={f.start_mode === 'now'} onChange={() => up('start_mode', 'now')} />立即开始
                                </label>
                                <label style={{ display: 'inline-flex', gap: 6, alignItems: 'center', fontSize: 13 }}>
                                    <input type="radio" name="start-mode" checked={f.start_mode === 'schedule'} onChange={() => up('start_mode', 'schedule')} />指定时间
                                </label>
                                {f.start_mode === 'schedule' && (
                                    <input
                                        type="datetime-local"
                                        style={{ padding: '6px 10px', border: '1px solid #ddd', borderRadius: 4, fontSize: 13 }}
                                        value={f.start_at}
                                        onChange={e => up('start_at', e.target.value)}
                                        aria-label="计划开始时间"
                                    />
                                )}
                            </div>

                            <p style={S.hint}>每个采集窗口持续 {f.duration}s / 采样频率 {f.frequency}Hz。采样时长必须小于采样间隔，否则相邻窗口会重叠。<InfoTooltip>窗口是一次实际采样的持续时间；间隔是两次采样开始之间的时间。</InfoTooltip></p>
                            {nextRunEstimate && (
                                <p style={S.ok}>下一次采集：{nextRunEstimate.toLocaleString()}（{intervalHumanLabel(f.interval_seconds)}一次）</p>
                            )}
                            {overlapRisk && (
                                <p style={S.warn}>采样时长（{f.duration}s）大于或等于采样间隔（{f.interval_seconds}s），相邻窗口会重叠：请调短采样时长，或加长采样间隔。</p>
                            )}
                        </div>
                    )}
                </div>

                {err && <p style={S.err}>{err}</p>}
                {ok && <div style={S.ok}>{ok} {cid && (isSch
                    ? <Link to={scheduleSuccessLink} style={{ color: '#4a6cf7', fontWeight: 'bold' }}>去时间轴 (SID: {cid})</Link>
                    : <Link to={`/task/result?tid=${cid}`} style={{ color: '#4a6cf7', fontWeight: 'bold' }}>查看任务 {cid}</Link>)}
                </div>}

                <div style={{ marginTop: 16, display: 'flex', gap: 10 }}>
                    <button style={{ ...S.btn, opacity: sub || kload || kindList.length === 0 ? 0.6 : 1 }} onClick={submit} disabled={sub || kload || kindList.length === 0}>
                        {sub ? '提交中...' : f.continuous ? '创建周期性深度采样' : '提交任务'}
                    </button>
                    <button style={{ ...S.btn, background: '#999' }} onClick={onClose} disabled={sub}>取消</button>
                </div>
            </div>
        </div>
    );
}
