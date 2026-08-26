import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { Link } from 'react-router-dom';
import { continuous, sentinelRules } from '../api';
import { CONTINUOUS_SIGNALS, DEFAULT_CONTINUOUS_SIGNALS, SENTINEL_SIGNALS, formatBytes, selectorModeLabel, signalLabel } from '../utils/continuous';
import InfoTooltip from './InfoTooltip';

// 采集信号的悬停说明（与 drop/common/ContinuousSampler.cpp 的采集实现对应）
const SIGNAL_HINTS = {
    cpu_profile: '以固定频率对 CPU 调用栈采样，统计各函数占用 CPU 的比例，用于生成火焰图与热点 TopN，定位 CPU 热点。',
    io_latency: '通过 eBPF 跟踪块设备层 IO 请求（block_rq_issue → block_rq_complete），记录每个请求从提交到完成的耗时（微秒），统计延迟分布，定位磁盘读写延迟。',
    io_syscall_latency: '通过 eBPF 跟踪 read / write / pread64 / pwrite64 等系统调用从进入到返回的耗时（微秒），统计延迟分布，定位 IO 相关系统调用慢在哪。',
    sched_latency: '通过 eBPF 跟踪任务进入可运行队列到被调度上 CPU 的等待时间（微秒），统计调度/排队延迟分布，定位线程等待调度导致的延迟。',
};

const S = {
    overlay: { position: 'fixed', inset: 0, zIndex: 1000, background: 'rgba(16,24,40,.5)', display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 20 },
    modal: { width: 'min(760px, 100%)', maxHeight: '90vh', overflowY: 'auto', background: '#fff', borderRadius: 8, border: '1px solid #e5e7eb', boxShadow: '0 20px 40px rgba(16,24,40,.22)', padding: 20 },
    head: { display: 'flex', justifyContent: 'space-between', gap: 16, marginBottom: 18 },
    title: { margin: 0, fontSize: 18, color: '#101828' },
    subtitle: { margin: '5px 0 0', color: '#667085', fontSize: 13 },
    close: { border: 0, background: 'transparent', color: '#667085', cursor: 'pointer', fontSize: 24, lineHeight: 1 },
    label: { display: 'block', color: '#344054', fontSize: 13, fontWeight: 700, marginBottom: 6 },
    input: { width: '100%', height: 38, padding: '8px 10px', boxSizing: 'border-box', border: '1px solid #d0d5dd', borderRadius: 6, fontSize: 13 },
    segmented: { display: 'inline-flex', border: '1px solid #d0d5dd', borderRadius: 6, overflow: 'hidden', height: 38 },
    segment: active => ({ padding: '0 18px', border: 0, borderRight: '1px solid #d0d5dd', background: active ? '#eef2ff' : '#fff', color: active ? '#315efb' : '#475467', fontWeight: 700, cursor: 'pointer' }),
    section: { borderTop: '1px solid #eaecf0', paddingTop: 16, marginTop: 16 },
    toolbar: { display: 'flex', gap: 10, alignItems: 'center', marginBottom: 10 },
    processList: { border: '1px solid #eaecf0', borderRadius: 6, maxHeight: 245, overflowY: 'auto' },
    process: selected => ({ display: 'grid', gridTemplateColumns: '20px minmax(0,1fr) 90px', gap: 10, padding: '10px 12px', borderBottom: '1px solid #f2f4f7', background: selected ? '#f5f7ff' : '#fff', cursor: 'pointer', alignItems: 'start' }),
    mono: { color: '#344054', fontSize: 12, wordBreak: 'break-all', marginTop: 3, fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace' },
    subtle: { color: '#667085', fontSize: 12, lineHeight: 1.45 },
    chips: { display: 'flex', flexWrap: 'wrap', gap: 7 },
    chip: { background: '#f2f4f7', color: '#344054', borderRadius: 999, padding: '4px 8px', fontSize: 12, fontWeight: 700 },
    warn: { marginTop: 14, color: '#b54708', background: '#fffaeb', border: '1px solid #fedf89', borderRadius: 6, padding: 12, fontSize: 13, lineHeight: 1.5 },
    error: { marginTop: 12, color: '#b42318', background: '#fff6f5', border: '1px solid #fda29b', borderRadius: 6, padding: 10, fontSize: 13 },
    sentinelToggle: { display: 'flex', gap: 8, alignItems: 'flex-start', fontSize: 13, fontWeight: 700, color: '#344054', cursor: 'pointer' },
    sentinelFields: { marginTop: 12, padding: 14, border: '1px solid #c7d2fe', background: '#eef2ff', borderRadius: 8, display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12 },
    actions: { display: 'flex', justifyContent: 'flex-end', gap: 10, marginTop: 20 },
    cancel: { background: '#fff', color: '#475467', border: '1px solid #d0d5dd', borderRadius: 6, padding: '8px 14px', fontWeight: 700, cursor: 'pointer' },
    submit: disabled => ({ background: disabled ? '#e5e7eb' : '#315efb', color: disabled ? '#98a2b3' : '#fff', border: 0, borderRadius: 6, padding: '9px 14px', fontWeight: 700, cursor: disabled ? 'not-allowed' : 'pointer' }),
    grid: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit,minmax(170px,1fr))', gap: 12, marginTop: 12 },
    fieldHint: { color: '#667085', fontSize: 11, lineHeight: 1.4, margin: '4px 0 0' },
};

export default function CreateContinuousSessionModal({ target, onClose, onSuccess }) {
    const [scope, setScope] = useState('process');
    const [name, setName] = useState('');
    const [processes, setProcesses] = useState([]);
    const [agentState, setAgentState] = useState(null);
    const [agentFresh, setAgentFresh] = useState(false);
    const [keyword, setKeyword] = useState('');
    const [selectedExe, setSelectedExe] = useState('');
    // 阶段六：selector 类型与精确身份。默认选择单个具体进程实例
    //（pid_instance），可切换 exe 全实例 / cgroup / container_id。
    const [selectorMode, setSelectorMode] = useState('pid_instance');
    const [selectedInstance, setSelectedInstance] = useState(null);
    const [selectedCgroup, setSelectedCgroup] = useState('');
    const [selectedContainerId, setSelectedContainerId] = useState('');
    const [loading, setLoading] = useState(true);
    const [submitting, setSubmitting] = useState(false);
    const [allowDegraded, setAllowDegraded] = useState(false);
    const [selectedSignals, setSelectedSignals] = useState(DEFAULT_CONTINUOUS_SIGNALS);
    const [error, setError] = useState('');
    const [sampleRate, setSampleRate] = useState(19);
    const [aggregationWindow, setAggregationWindow] = useState(10);
    const [uploadBatch, setUploadBatch] = useState(60);
    const [retentionHours, setRetentionHours] = useState(24);
    const [conflictSession, setConflictSession] = useState(null);
    const [sentinelEnabled, setSentinelEnabled] = useState(false);
    const [sentinelSignal, setSentinelSignal] = useState('');
    const [sentinelFloor, setSentinelFloor] = useState('5');
    const [sentinelCooldownMin, setSentinelCooldownMin] = useState(15);
    const [sentinelError, setSentinelError] = useState('');
    // 会话已创建成功后缓存下来：如果哨兵规则创建失败，重试只重发哨兵请求，
    // 不会重新创建一次持续采集会话。
    const [createdSession, setCreatedSession] = useState(null);

    const loadProcesses = useCallback(async () => {
        setLoading(true);
        setError('');
        try {
            const response = await continuous.processes({ target_ip: target.ip });
            if (response.code !== 0) throw new Error(response.message || '加载进程失败');
            setProcesses(response.data?.processes || []);
            setAgentState(response.data?.agent_state || null);
            setAgentFresh(Boolean(response.data?.fresh));
        } catch (err) {
            setAgentFresh(false);
            setError(err?.message || '加载进程失败');
        } finally {
            setLoading(false);
        }
    }, [target.ip]);

    useEffect(() => { loadProcesses(); }, [loadProcesses]);

    const groups = useMemo(() => {
        const query = keyword.trim().toLowerCase();
        const matches = process => !query || `${process.comm || ''} ${process.exe || ''} ${process.cgroup_path || ''} ${process.container_id || ''}`.toLowerCase().includes(query);
        if (selectorMode === 'pid_instance') {
            // 默认模式：每个具体进程实例一行（PID + 启动时间 + exe）。
            return processes.filter(matches).sort((a, b) => (Number(b.rss_bytes) || 0) - (Number(a.rss_bytes) || 0) || a.pid - b.pid);
        }
        if (selectorMode === 'cgroup') {
            const byCgroup = new Map();
            processes.forEach(process => {
                if (!process.cgroup_path) return;
                const current = byCgroup.get(process.cgroup_path) || { cgroup: process.cgroup_path, instances: [], rss: 0 };
                current.instances.push(process);
                current.rss += Number(process.rss_bytes) || 0;
                byCgroup.set(process.cgroup_path, current);
            });
            return Array.from(byCgroup.values()).filter(group => !query || group.cgroup.toLowerCase().includes(query))
                .sort((a, b) => b.rss - a.rss || a.cgroup.localeCompare(b.cgroup));
        }
        if (selectorMode === 'container_id') {
            const byContainer = new Map();
            processes.forEach(process => {
                if (!process.container_id) return;
                const current = byContainer.get(process.container_id) || { container: process.container_id, instances: [], rss: 0 };
                current.instances.push(process);
                current.rss += Number(process.rss_bytes) || 0;
                byContainer.set(process.container_id, current);
            });
            return Array.from(byContainer.values()).filter(group => !query || group.container.toLowerCase().includes(query))
                .sort((a, b) => b.rss - a.rss || a.container.localeCompare(b.container));
        }
        // exe_all_instances：按 exe 分组（原有逻辑）。
        const byExe = new Map();
        processes.forEach(process => {
            if (!process.exe) return;
            const current = byExe.get(process.exe) || { exe: process.exe, comm: process.comm, instances: [], rss: 0 };
            current.instances.push(process);
            current.rss += Number(process.rss_bytes) || 0;
            byExe.set(process.exe, current);
        });
        return Array.from(byExe.values()).filter(group => !query || `${group.comm} ${group.exe}`.toLowerCase().includes(query))
            .sort((a, b) => b.rss - a.rss || a.exe.localeCompare(b.exe));
    }, [processes, keyword, selectorMode]);

    const needsDegradedConfirmation = !agentState?.strict_capable;
    const numericSettingsValid = Number(sampleRate) >= 1 && Number(sampleRate) <= 999
        && Number(aggregationWindow) >= 5 && Number(aggregationWindow) <= 300
        && Number(uploadBatch) >= Number(aggregationWindow) && Number(uploadBatch) <= 3600
        // 保留时间与后端一致：原始数据最长保留 24 小时（后端 1–24h 校验）。
        && Number(retentionHours) >= 1 && Number(retentionHours) <= 24;
    const eligibleSentinelSignals = useMemo(
        () => selectedSignals.filter(signal => SENTINEL_SIGNALS.includes(signal)),
        [selectedSignals],
    );
    // 已选信号变化后，如果当前哨兵信号不再可选（或还没选），自动落到第一个可选项；
    // 一个可选信号都没有时关闭哨兵栏，避免用户对着一个提交了也不会生效的选项瞎填。
    useEffect(() => {
        if (eligibleSentinelSignals.length === 0) {
            setSentinelEnabled(false);
            setSentinelSignal('');
            return;
        }
        if (!eligibleSentinelSignals.includes(sentinelSignal)) setSentinelSignal(eligibleSentinelSignals[0]);
    }, [eligibleSentinelSignals, sentinelSignal]);
    const sentinelFieldsValid = !sentinelEnabled || (sentinelSignal && Number(sentinelFloor) > 0 && Number(sentinelCooldownMin) > 0);
    const selectorValid = scope === 'host' || (selectorMode === 'pid_instance' ? Boolean(selectedInstance)
        : selectorMode === 'exe_all_instances' ? Boolean(selectedExe)
            : selectorMode === 'cgroup' ? Boolean(selectedCgroup)
                : Boolean(selectedContainerId));
    const valid = agentFresh && numericSettingsValid && name.trim() && selectedSignals.length > 0 && selectorValid
        && (!needsDegradedConfirmation || allowDegraded) && sentinelFieldsValid;

    const toggleSignal = useCallback((signal) => {
        setSelectedSignals(current => {
            if (current.includes(signal)) {
                const next = current.filter(item => item !== signal);
                return next.length > 0 ? next : current;
            }
            return [...current, signal];
        });
    }, []);

    const submit = async () => {
        if (!valid || submitting) return;
        setSubmitting(true);
        setError('');
        setSentinelError('');
        setConflictSession(null);
        try {
            // 会话已经建好（上一次提交时创建成功，这次只是在重试哨兵规则）：跳过重复建会话。
            let session = createdSession;
            if (!session) {
                // 阶段六：按 selector 类型构造 payload。默认 pid_instance 选择
                // 具体进程实例；exe_all_instances 跟随同路径全部实例；
                // cgroup/container_id 从 Agent 快照选择。
                const selectorParams = scope === 'process' ? (selectorMode === 'pid_instance' ? {
                    pid: selectedInstance.pid, process_start_ms: selectedInstance.process_start_ms, exe: selectedInstance.exe,
                } : selectorMode === 'cgroup' ? { cgroup: selectedCgroup }
                    : selectorMode === 'container_id' ? { container_id: selectedContainerId }
                        : { exe: selectedExe }) : null;
                const response = await continuous.createSession({
                    name: name.trim(), target_ip: target.ip, hostname: target.hostname || '', service_name: target.service_name || 'hotmethod',
                    scope, selector_exe: scope === 'process' ? (selectorMode === 'pid_instance' ? selectedInstance.exe : selectorMode === 'exe_all_instances' ? selectedExe : '') : '',
                    selector_mode: scope === 'process' ? selectorMode : 'exe_all_instances',
                    selector_params: selectorParams,
                    signals: selectedSignals, continuity_mode: 'strict', allow_degraded: allowDegraded,
                    sample_rate_hz: Number(sampleRate), aggregation_window_sec: Number(aggregationWindow),
                    upload_batch_sec: Number(uploadBatch), retention_hours: Number(retentionHours),
                });
                if (response.code !== 0) throw new Error(response.message || '创建持续采集失败');
                session = response.data?.session;
                setCreatedSession(session);
            }
            if (sentinelEnabled) {
                try {
                    await sentinelRules.create({
                        name: `${name.trim()}（哨兵）`, target_ip: session.target_ip || target.ip,
                        signal: sentinelSignal, metric: 'p99',
                        // 后端 floor_value 单位是微秒(us)，前端输入框单位是毫秒(ms)，提交时 ×1000 换算。
                        floor_value: Number(sentinelFloor) * 1000, cooldown_seconds: Math.round(Number(sentinelCooldownMin) * 60),
                    });
                } catch (sentinelErr) {
                    // 会话本身已经创建成功，不因为哨兵规则失败而丢弃：只提示、留给用户选择重试或直接进入详情页处理。
                    setSentinelError(sentinelErr?.response?.data?.message || sentinelErr?.message || '哨兵规则创建失败');
                    return;
                }
            }
            onSuccess?.(session);
        } catch (err) {
            const payload = err?.response?.data;
            const existing = payload?.data?.existing_session;
            if (existing?.sid) setConflictSession(existing);
            setError(payload?.message || err?.message || '创建持续采集失败');
        } finally {
            setSubmitting(false);
        }
    };

    return <div className="continuous-modal-overlay" style={S.overlay} onMouseDown={event => event.target === event.currentTarget && !submitting && onClose()}>
        <div className="continuous-modal" style={S.modal} role="dialog" aria-modal="true" aria-label="新建持续采集">
            <div style={S.head}>
                <div><h3 style={S.title}>新建持续采集</h3><p style={S.subtitle}>{target.hostname || target.ip} · 创建后持续运行，直到用户停止</p></div>
                <button style={S.close} onClick={onClose} disabled={submitting} aria-label="关闭">×</button>
            </div>
            <label style={S.label}>任务名称 *</label>
            <input style={S.input} value={name} onChange={event => setName(event.target.value)} placeholder="例如：API 服务持续剖析" maxLength={256} />

            <div style={S.section}>
                <label style={S.label}>采集范围 *</label>
                <span style={S.segmented}>
                    <button type="button" style={S.segment(scope === 'host')} onClick={() => setScope('host')}>整机</button>
                    <button type="button" style={{ ...S.segment(scope === 'process'), borderRight: 0 }} onClick={() => setScope('process')}>按进程</button>
                </span>
                <p style={S.subtle}>{scope === 'process' ? '默认选择单个具体进程实例；可切换为同路径全部实例、cgroup 或容器。' : '采集该主机全部进程，详情中仍可按进程查询。'}</p>
            </div>

            {scope === 'process' && <div style={S.section}>
                <label style={S.label}>selector 类型 *</label>
                <span style={S.segmented}>
                    {['pid_instance', 'exe_all_instances', 'cgroup', 'container_id'].map((mode, index) => (
                        <button key={mode} type="button" style={{ ...S.segment(selectorMode === mode), borderRight: index === 3 ? 0 : '1px solid #d0d5dd' }}
                            onClick={() => setSelectorMode(mode)}>{selectorModeLabel(mode)}</button>
                    ))}
                </span>
                <p style={S.subtle}>{selectorMode === 'pid_instance' ? '只采集这一个进程实例；进程退出后进入等待，PID 复用不会误采新进程。'
                    : selectorMode === 'exe_all_instances' ? '自动跟随同路径的全部实例（含重启后的新进程）。'
                        : selectorMode === 'cgroup' ? '采集该 cgroup 内的全部进程，跟随组内进程变化。'
                            : '采集该容器内的全部进程，跟随容器内进程变化。'}</p>
                <div className="continuous-modal-toolbar" style={S.toolbar}>
                    <input style={{ ...S.input, flex: 1 }} value={keyword} onChange={event => setKeyword(event.target.value)} placeholder="搜索进程名 / exe / cgroup / 容器 ID" />
                    <button style={S.cancel} onClick={loadProcesses} disabled={loading}>{loading ? '刷新中' : '刷新'}</button>
                </div>
                <div style={S.processList}>
                    {loading ? <div style={{ padding: 24, textAlign: 'center', ...S.subtle }}>正在读取 Agent 进程列表...</div>
                        : groups.length === 0 ? <div style={{ padding: 24, textAlign: 'center', ...S.subtle }}>当前没有可选择的{selectorMode === 'cgroup' ? ' cgroup' : selectorMode === 'container_id' ? ' 容器' : '进程'}</div>
                            : selectorMode === 'pid_instance' ? groups.map(process => {
                                const selected = selectedInstance?.pid === process.pid && selectedInstance?.process_start_ms === process.process_start_ms;
                                return <label className="continuous-process-row" key={`${process.pid}-${process.process_start_ms}`} style={S.process(selected)}>
                                    <input type="radio" name="continuous-instance" checked={selected} onChange={() => setSelectedInstance(process)} />
                                    <span><strong>{process.comm || (process.exe || '').split('/').pop()}</strong><div style={S.mono}>{process.exe}</div><div style={S.subtle}>PID {process.pid} · 启动 {formatStart(process.process_start_ms)} · 不跟随重启</div></span>
                                    <span className="continuous-process-meta" style={{ textAlign: 'right', ...S.subtle }}>{formatBytes(process.rss_bytes)}</span>
                                </label>;
                            })
                                : selectorMode === 'cgroup' ? groups.map(group => (
                                    <label className="continuous-process-row" key={group.cgroup} style={S.process(selectedCgroup === group.cgroup)}>
                                        <input type="radio" name="continuous-cgroup" checked={selectedCgroup === group.cgroup} onChange={() => setSelectedCgroup(group.cgroup)} />
                                        <span><strong>{group.cgroup.split('/').pop() || group.cgroup}</strong><div style={S.mono}>{group.cgroup}</div><div style={S.subtle}>跟随组内 {group.instances.length} 个进程</div></span>
                                        <span className="continuous-process-meta" style={{ textAlign: 'right', ...S.subtle }}>{group.instances.length} 个实例<br />{formatBytes(group.rss)}</span>
                                    </label>
                                ))
                                    : selectorMode === 'container_id' ? groups.map(group => (
                                        <label className="continuous-process-row" key={group.container} style={S.process(selectedContainerId === group.container)}>
                                            <input type="radio" name="continuous-container" checked={selectedContainerId === group.container} onChange={() => setSelectedContainerId(group.container)} />
                                            <span><strong>容器 {group.container.slice(0, 12)}</strong><div style={S.mono}>{group.container}</div><div style={S.subtle}>跟随容器内 {group.instances.length} 个进程</div></span>
                                            <span className="continuous-process-meta" style={{ textAlign: 'right', ...S.subtle }}>{group.instances.length} 个实例<br />{formatBytes(group.rss)}</span>
                                        </label>
                                    ))
                                        : groups.map(group => <label className="continuous-process-row" key={group.exe} style={S.process(selectedExe === group.exe)}>
                                            <input type="radio" name="continuous-exe" checked={selectedExe === group.exe} onChange={() => setSelectedExe(group.exe)} />
                                            <span><strong>{group.comm || group.exe.split('/').pop()}</strong><div style={S.mono}>{group.exe}</div><div style={S.subtle}>跟随该 exe 的全部实例</div></span>
                                            <span className="continuous-process-meta" style={{ textAlign: 'right', ...S.subtle }}>{group.instances.length} 个实例<br />{formatBytes(group.rss)}</span>
                                        </label>)}
                </div>
            </div>}

            <div style={S.section}>
                <label style={S.label}>采集信号 *</label>
                <div style={{ display: 'grid', gap: 8 }}>
                    {CONTINUOUS_SIGNALS.map(signal => (
                        <label key={signal} style={{ display: 'flex', gap: 8, alignItems: 'center', fontSize: 13, color: '#344054', fontWeight: 700 }}>
                            <input
                                type="checkbox"
                                checked={selectedSignals.includes(signal)}
                                onChange={() => toggleSignal(signal)}
                            />
                            <span>{signalLabel(signal)}</span>
                            <InfoTooltip label={`查看${signalLabel(signal)}采集说明`}>{SIGNAL_HINTS[signal]}</InfoTooltip>
                        </label>
                    ))}
                </div>
            </div>

            <div style={S.section}>
                {eligibleSentinelSignals.length === 0
                    ? <div style={S.subtle}>当前勾选的信号暂不支持自动告警（仅调度延迟 / IO 延迟支持哨兵规则）。</div>
                    : <>
                        <label style={S.sentinelToggle} title="哨兵规则：后台持续监控告警信号，超过阈值时自动触发一次 60 秒的深度诊断采样">
                            <input type="checkbox" checked={sentinelEnabled} disabled={Boolean(createdSession)}
                                onChange={event => setSentinelEnabled(event.target.checked)} style={{ marginTop: 2 }} />
                            <span>创建后台哨兵，超过阈值自动触发一次深度诊断</span>
                        </label>
                        {sentinelEnabled && <div style={S.sentinelFields}>
                            <label><span style={S.label}>监控信号</span>
                                <select style={S.input} value={sentinelSignal} disabled={Boolean(createdSession)}
                                    onChange={event => setSentinelSignal(event.target.value)}>
                                    {eligibleSentinelSignals.map(signal => <option key={signal} value={signal}>{signalLabel(signal)} · p99</option>)}
                                </select>
                            </label>
                            <NumberField label="告警阈值 ms" value={sentinelFloor} onChange={setSentinelFloor} min={0.1} max={100000} disabled={Boolean(createdSession)} />
                            <NumberField label="冷却期（分钟）" value={sentinelCooldownMin} onChange={setSentinelCooldownMin} min={1} max={1440} disabled={Boolean(createdSession)} />
                        </div>}
                    </>}
            </div>

            <details style={S.section}>
                <summary style={{ cursor: 'pointer', color: '#344054', fontSize: 13, fontWeight: 700 }}>高级设置</summary>
                <div style={S.grid}>
                    <NumberField
                        label="采样频率" hint="采样频率 Hz：数值越高火焰图越精细，但 CPU 开销越大（1–999）"
                        value={sampleRate} onChange={setSampleRate} min={1} max={999} fieldId="cps-sample-rate"
                    />
                    <NumberField
                        label="聚合窗口" hint="聚合窗口 s：每多少秒把采样结果汇总成一条记录（5–300）"
                        value={aggregationWindow} onChange={setAggregationWindow} min={5} max={300} fieldId="cps-agg-window"
                    />
                    <NumberField
                        label="上传周期" hint="上传周期 s：每多少秒把汇总结果上传到服务端，需不小于聚合窗口（最长 3600）"
                        value={uploadBatch} onChange={setUploadBatch} min={5} max={3600} fieldId="cps-upload-batch"
                    />
                    <NumberField
                        label="保留时间" hint="保留时间 h：原始采样数据最长保留 24 小时（1–24）"
                        value={retentionHours} onChange={setRetentionHours} min={1} max={24} fieldId="cps-retention"
                    />
                </div>
            </details>

            {!loading && !agentFresh && <div style={S.error}>目标 Agent 尚未连接持续采集控制面，当前不能创建持续任务。请确认 Agent 在线且已启用 Native Continuous Profiling。</div>}
            {needsDegradedConfirmation && <div style={S.warn} role="note" aria-label="降级模式说明">
                当前 Agent 尚未提供严格的常驻 perf + CO-RE 无缝切窗能力，将使用 PID 范围受限的滚动 perf/bpftrace 回退。任务不会退化为整机采集，但窗口切换可能产生短暂空档。
                <p style={{ ...S.fieldHint, margin: '8px 0 0', color: '#b54708' }}>严格模式：常驻 perf + CO-RE 无缝切窗，无采集空档；降级模式：滚动 perf/bpftrace 回退，窗口切换可能有短暂空档。</p>
                <label style={{ display: 'flex', gap: 8, marginTop: 10, alignItems: 'flex-start', fontWeight: 700 }}><input type="checkbox" checked={allowDegraded} onChange={event => setAllowDegraded(event.target.checked)} />我已了解并允许降级运行</label>
            </div>}
            {error && <div style={S.error}>{error}</div>}
            {createdSession && sentinelError && <div style={S.warn}>持续采集已创建，但哨兵规则创建失败：{sentinelError}。可以重试，也可以先关闭，之后在详情页补建哨兵。
                <div style={{ marginTop: 8 }}><button style={S.cancel} onClick={() => onSuccess?.(createdSession)}>先关闭，跳过哨兵</button></div>
            </div>}
            {conflictSession?.sid && <div style={S.warn}>已有活动会话由 {conflictSession.user_name || '系统'} 创建。<Link to={`/hosts/${encodeURIComponent(target.id)}/continuous/${encodeURIComponent(conflictSession.sid)}`} onClick={onClose}>查看已有会话</Link></div>}
            <div style={S.actions}><button style={S.cancel} onClick={onClose} disabled={submitting}>取消</button><button style={S.submit(!valid || submitting)} onClick={submit} disabled={!valid || submitting}>{submitting ? '创建中...' : createdSession ? '重试创建哨兵规则' : '创建并开始采集'}</button></div>
        </div>
    </div>;
}

function NumberField({ label, hint, value, onChange, min, max, disabled, fieldId }) {
    const id = fieldId || `field-${label}`;
    return (
        <label>
            <span style={S.label}>{label}{hint && <InfoTooltip>{hint}</InfoTooltip>}</span>
            <input
                type="number"
                style={S.input}
                min={min}
                max={max}
                value={value}
                disabled={disabled}
                id={id}
                title={hint}
                onChange={event => onChange(event.target.value)}
            />
        </label>
    );
}

function formatStart(value) {
    const ms = Number(value);
    if (!Number.isFinite(ms) || ms <= 0) return 'start 未知';
    const date = new Date(ms);
    return Number.isNaN(date.getTime()) ? `start ${value}` : date.toLocaleString();
}
