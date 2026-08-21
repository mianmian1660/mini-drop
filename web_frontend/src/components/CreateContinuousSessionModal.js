import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { continuous } from '../api';
import { CONTINUOUS_SIGNALS, formatBytes, signalLabel } from '../utils/continuous';

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
    actions: { display: 'flex', justifyContent: 'flex-end', gap: 10, marginTop: 20 },
    cancel: { background: '#fff', color: '#475467', border: '1px solid #d0d5dd', borderRadius: 6, padding: '8px 14px', fontWeight: 700, cursor: 'pointer' },
    submit: disabled => ({ background: disabled ? '#e5e7eb' : '#315efb', color: disabled ? '#98a2b3' : '#fff', border: 0, borderRadius: 6, padding: '9px 14px', fontWeight: 700, cursor: disabled ? 'not-allowed' : 'pointer' }),
    grid: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit,minmax(170px,1fr))', gap: 12, marginTop: 12 },
};

export default function CreateContinuousSessionModal({ target, onClose, onSuccess }) {
    const [scope, setScope] = useState('process');
    const [name, setName] = useState('');
    const [processes, setProcesses] = useState([]);
    const [agentState, setAgentState] = useState(null);
    const [agentFresh, setAgentFresh] = useState(false);
    const [keyword, setKeyword] = useState('');
    const [selectedExe, setSelectedExe] = useState('');
    const [loading, setLoading] = useState(true);
    const [submitting, setSubmitting] = useState(false);
    const [allowDegraded, setAllowDegraded] = useState(false);
    const [error, setError] = useState('');
    const [sampleRate, setSampleRate] = useState(19);
    const [aggregationWindow, setAggregationWindow] = useState(10);
    const [uploadBatch, setUploadBatch] = useState(60);
    const [retentionHours, setRetentionHours] = useState(24);

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
        const byExe = new Map();
        processes.forEach(process => {
            if (!process.exe) return;
            const current = byExe.get(process.exe) || { exe: process.exe, comm: process.comm, instances: [], rss: 0 };
            current.instances.push(process);
            current.rss += Number(process.rss_bytes) || 0;
            byExe.set(process.exe, current);
        });
        const query = keyword.trim().toLowerCase();
        return Array.from(byExe.values()).filter(group => !query || `${group.comm} ${group.exe}`.toLowerCase().includes(query))
            .sort((a, b) => b.rss - a.rss || a.exe.localeCompare(b.exe));
    }, [processes, keyword]);

    const needsDegradedConfirmation = !agentState?.strict_capable;
    const numericSettingsValid = Number(sampleRate) >= 1 && Number(sampleRate) <= 999
        && Number(aggregationWindow) >= 5 && Number(aggregationWindow) <= 300
        && Number(uploadBatch) >= Number(aggregationWindow) && Number(uploadBatch) <= 3600
        && Number(retentionHours) >= 1 && Number(retentionHours) <= 720;
    const valid = agentFresh && numericSettingsValid && name.trim() && (scope === 'host' || selectedExe)
        && (!needsDegradedConfirmation || allowDegraded);

    const submit = async () => {
        if (!valid || submitting) return;
        setSubmitting(true);
        setError('');
        try {
            const response = await continuous.createSession({
                name: name.trim(), target_ip: target.ip, hostname: target.hostname || '', service_name: target.service_name || 'hotmethod',
                scope, selector_exe: scope === 'process' ? selectedExe : '', selector_mode: 'all_instances',
                signals: CONTINUOUS_SIGNALS, continuity_mode: 'strict', allow_degraded: allowDegraded,
                sample_rate_hz: Number(sampleRate), aggregation_window_sec: Number(aggregationWindow),
                upload_batch_sec: Number(uploadBatch), retention_hours: Number(retentionHours),
            });
            if (response.code !== 0) throw new Error(response.message || '创建持续采集失败');
            onSuccess?.(response.data?.session);
        } catch (err) {
            setError(err?.response?.data?.message || err?.message || '创建持续采集失败');
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
                <p style={S.subtle}>{scope === 'process' ? '按可执行文件路径跟随全部实例；PID 变化和进程重启后会自动重新附着。' : '采集该主机全部进程，详情中仍可按进程查询。'}</p>
            </div>

            {scope === 'process' && <div style={S.section}>
                <div className="continuous-modal-toolbar" style={S.toolbar}>
                    <input style={{ ...S.input, flex: 1 }} value={keyword} onChange={event => setKeyword(event.target.value)} placeholder="搜索进程名或 exe 路径" />
                    <button style={S.cancel} onClick={loadProcesses} disabled={loading}>{loading ? '刷新中' : '刷新'}</button>
                </div>
                <div style={S.processList}>
                    {loading ? <div style={{ padding: 24, textAlign: 'center', ...S.subtle }}>正在读取 Agent 进程列表...</div>
                        : groups.length === 0 ? <div style={{ padding: 24, textAlign: 'center', ...S.subtle }}>当前没有可选择的进程</div>
                            : groups.map(group => <label className="continuous-process-row" key={group.exe} style={S.process(selectedExe === group.exe)}>
                                <input type="radio" name="continuous-exe" checked={selectedExe === group.exe} onChange={() => setSelectedExe(group.exe)} />
                                <span><strong>{group.comm || group.exe.split('/').pop()}</strong><div style={S.mono}>{group.exe}</div><div style={S.subtle}>跟随该 exe 的全部实例</div></span>
                                <span className="continuous-process-meta" style={{ textAlign: 'right', ...S.subtle }}>{group.instances.length} 个实例<br />{formatBytes(group.rss)}</span>
                            </label>)}
                </div>
            </div>}

            <div style={S.section}><label style={S.label}>采集信号</label><div style={S.chips}>{CONTINUOUS_SIGNALS.map(signal => <span key={signal} style={S.chip}>{signalLabel(signal)}</span>)}</div></div>

            <details style={S.section}>
                <summary style={{ cursor: 'pointer', color: '#344054', fontSize: 13, fontWeight: 700 }}>高级设置</summary>
                <div style={S.grid}>
                    <NumberField label="采样频率 Hz" value={sampleRate} onChange={setSampleRate} min={1} max={999} />
                    <NumberField label="聚合窗口 s" value={aggregationWindow} onChange={setAggregationWindow} min={5} max={300} />
                    <NumberField label="上传周期 s" value={uploadBatch} onChange={setUploadBatch} min={5} max={3600} />
                    <NumberField label="保留时间 h" value={retentionHours} onChange={setRetentionHours} min={1} max={720} />
                </div>
            </details>

            {!loading && !agentFresh && <div style={S.error}>目标 Agent 尚未连接持续采集控制面，当前不能创建持续任务。请确认 Agent 在线且已启用 Native Continuous Profiling。</div>}
            {needsDegradedConfirmation && <div style={S.warn}>
                当前 Agent 尚未提供严格的常驻 perf + CO-RE 无缝切窗能力，将使用 PID 范围受限的滚动 perf/bpftrace 回退。任务不会退化为整机采集，但窗口切换可能产生短暂空档。
                <label style={{ display: 'flex', gap: 8, marginTop: 10, alignItems: 'flex-start', fontWeight: 700 }}><input type="checkbox" checked={allowDegraded} onChange={event => setAllowDegraded(event.target.checked)} />我已了解并允许降级运行</label>
            </div>}
            {error && <div style={S.error}>{error}</div>}
            <div style={S.actions}><button style={S.cancel} onClick={onClose} disabled={submitting}>取消</button><button style={S.submit(!valid || submitting)} onClick={submit} disabled={!valid || submitting}>{submitting ? '创建中...' : '创建并开始采集'}</button></div>
        </div>
    </div>;
}

function NumberField({ label, value, onChange, min, max }) {
    return <label><span style={S.label}>{label}</span><input type="number" style={S.input} min={min} max={max} value={value} onChange={event => onChange(event.target.value)} /></label>;
}
