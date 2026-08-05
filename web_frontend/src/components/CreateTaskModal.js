import React, { useState, useEffect, useMemo } from 'react';
import { Link } from 'react-router-dom';
import { tasks, agents, schedules, taskKinds } from '../api';

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
    err: { color: '#f44336', fontSize: 13, marginTop: 12 },
    ok: { color: '#4caf50', fontSize: 13, marginTop: 12 },
    hint: { fontSize: 11, color: '#888', marginTop: 2, marginBottom: 8 },
    chk: { display: 'flex', alignItems: 'center', gap: 8, marginBottom: 12 },
    presetBtn: (active) => ({
        padding: '4px 10px', fontSize: 12, borderRadius: 4, cursor: 'pointer',
        background: active ? '#4a6cf7' : '#e0e0e0', color: active ? '#fff' : '#333', border: 'none',
    }),
};

const WINDOW_PRESETS = [
    { label: '5 分钟窗口(默认)', cron: '*/5 * * * *', windowSeconds: 300, duration: 290, frequency: 19 },
    { label: '1 分钟窗口', cron: '*/1 * * * *', windowSeconds: 60, duration: 50, frequency: 19 },
    { label: '10 分钟窗口', cron: '*/10 * * * *', windowSeconds: 600, duration: 580, frequency: 19 },
    { label: '30 分钟窗口', cron: '*/30 * * * *', windowSeconds: 1800, duration: 1740, frequency: 19 },
];

function valuesFromKind(kind) {
    const out = {};
    (kind?.schema || []).forEach(field => {
        if (field.default !== undefined) out[field.name] = field.default;
    });
    return { ...(kind?.default || {}), ...out };
}

function coerceField(field, value) {
    if (field.type === 'number') return parseInt(value, 10) || 0;
    if (field.type === 'boolean') return Boolean(value);
    return String(value ?? '');
}

export default function CreateTaskModal({ onClose, onSuccess }) {
    const [f, setF] = useState({
        name: '', target_ip: '', task_kind: '', target_pid: 0, duration: 10, frequency: 99,
        callgraph: 'fp', event: 'cpu-clock', pprof_url: '',
        continuous: false, cron_expr: '*/5 * * * *', window_seconds: 300,
    });
    const [sub, setSub] = useState(false);
    const [err, setErr] = useState('');
    const [ok, setOk] = useState('');
    const [cid, setCid] = useState('');
    const [isSch, setIsSch] = useState(false);
    const [agentList, setAgentList] = useState([]);
    const [kindList, setKindList] = useState([]);
    const [aload, setAload] = useState(true);
    const [kload, setKload] = useState(true);

    useEffect(() => {
        agents.list().then(r => {
            if (r.code === 0) {
                const list = r.data?.agents || [];
                setAgentList(list);
                const on = list.filter(a => a.online);
                if (on.length > 0) setF(p => p.target_ip ? p : ({ ...p, target_ip: on[0].ip_addr }));
            }
        }).catch(() => { }).finally(() => setAload(false));

        taskKinds.list().then(r => {
            if (r.code !== 0) throw new Error(r.message || '加载 TaskKind 失败');
            const list = r.data?.task_kinds || [];
            setKindList(list);
            if (list.length > 0) {
                setF(p => {
                    const kind = list.find(k => k.id === p.task_kind) || list[0];
                    return { ...p, task_kind: kind.id, ...valuesFromKind(kind) };
                });
            }
        }).catch(e => setErr(e.message || '任务类型元数据加载失败')).finally(() => setKload(false));
    }, []);

    const selectedKind = useMemo(
        () => kindList.find(k => k.id === f.task_kind) || null,
        [kindList, f.task_kind],
    );

    const up = (k, v) => setF(p => {
        if (k === 'task_kind') {
            const kind = kindList.find(item => item.id === v);
            return { ...p, task_kind: v, ...valuesFromKind(kind) };
        }
        const n = { ...p, [k]: v };
        if (k === 'continuous' && v === true && !p.continuous) {
            const preset = WINDOW_PRESETS[0];
            n.cron_expr = preset.cron;
            n.duration = preset.duration;
            n.frequency = preset.frequency;
            n.window_seconds = preset.windowSeconds;
        }
        return n;
    });

    const applyWindowPreset = (preset) => setF(p => ({
        ...p,
        cron_expr: preset.cron,
        duration: preset.duration,
        frequency: preset.frequency,
        window_seconds: preset.windowSeconds,
    }));

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
            <input
                style={S.input}
                type={field.type === 'url' ? 'url' : field.type}
                min={field.min}
                max={field.max}
                placeholder={field.placeholder || ''}
                value={value}
                onChange={e => up(field.name, coerceField(field, e.target.value))}
            />
        );
    };

    const submit = async () => {
        if (!f.name.trim()) { setErr('请输入任务名称'); return; }
        if (!f.target_ip) { setErr('请选择目标 Agent'); return; }
        if (!selectedKind) { setErr('请选择任务类型'); return; }
        const dur = parseInt(f.duration, 10) || 10;
        if (dur < 1 || dur > selectedKind.max_duration) { setErr(`时长需为 1-${selectedKind.max_duration}s`); return; }
        if (f.continuous && !f.cron_expr) { setErr('请输入 cron 表达式'); return; }
        if (f.continuous && f.window_seconds && dur >= f.window_seconds) {
            setErr(`采样时长(${dur}s)需小于窗口周期(${f.window_seconds}s)，否则相邻窗口会重叠`);
            return;
        }
        for (const field of selectedKind.schema || []) {
            const value = f[field.name];
            if (field.required && (value === '' || value === undefined || value === null || value === 0 && field.min > 0)) {
                setErr(`请填写${field.label}`);
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
                const r = await schedules.create({ ...payload, cron_expr: f.cron_expr, window_seconds: f.window_seconds });
                if (r.code === 0) {
                    setCid(r.data?.sid || ''); setIsSch(true); setOk('持续采集已创建！');
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
                        <h3 style={S.title}>新建采样任务</h3>
                        <div style={S.hint}>任务类型与参数由后端 TaskKind 契约加载。</div>
                    </div>
                    <button style={S.close} onClick={onClose} disabled={sub} aria-label="关闭">×</button>
                </div>

                <div style={{ marginBottom: 16 }}>
                    <label style={S.label}>目标 Agent *</label>
                    {aload ? <p style={{ fontSize: 12, color: '#999' }}>加载中...</p>
                        : agentList.length === 0 ? <p style={{ fontSize: 12, color: '#f44' }}>没有在线 Agent</p>
                            : <select style={S.select} value={f.target_ip} onChange={e => up('target_ip', e.target.value)}>
                                <option value="">-- 选择 Agent --</option>
                                {agentList.map(a => <option key={a.ip_addr} value={a.ip_addr}>{a.hostname} ({a.ip_addr}) {a.online ? '在线' : '离线'}</option>)}
                            </select>}
                </div>

                <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16 }}>
                    <div><label style={S.label}>任务名称 *</label><input style={S.input} placeholder="CPU采样-nginx" value={f.name} onChange={e => up('name', e.target.value)} /></div>
                    <div>
                        <label style={S.label}>任务类型</label>
                        {kload ? <p style={{ fontSize: 12, color: '#999' }}>加载中...</p> : (
                            <select style={S.select} value={f.task_kind} onChange={e => up('task_kind', e.target.value)}>
                                {kindList.map(kind => <option key={kind.id} value={kind.id}>{kind.display_name}</option>)}
                            </select>
                        )}
                    </div>
                    {(selectedKind?.schema || []).map(field => (
                        <div key={field.name}>
                            {field.type !== 'boolean' && <label style={S.label}>{field.label}{field.required ? ' *' : ''}</label>}
                            {renderField(field)}
                        </div>
                    ))}
                </div>

                <p style={S.hint}>将生成: {selectedKind?.display_name || '未选择任务类型'}</p>

                <div style={{ ...S.section, background: f.continuous ? '#e8f0ff' : '#fafafa', border: f.continuous ? '1px solid #4a6cf7' : '1px solid #e0e0e0' }}>
                    <label style={S.chk}>
                        <input type="checkbox" checked={f.continuous} onChange={e => up('continuous', e.target.checked)} />
                        <span style={{ fontWeight: 'bold', fontSize: 14 }}>持续采集 (Continuous Profiling)</span>
                    </label>
                    {f.continuous && (
                        <div>
                            <label style={S.label}>持续采集窗口预设</label>
                            <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', marginBottom: 8 }}>
                                {WINDOW_PRESETS.map(p => (
                                    <button key={p.cron} style={S.presetBtn(f.cron_expr === p.cron && f.window_seconds === p.windowSeconds)}
                                        onClick={() => applyWindowPreset(p)}>{p.label}</button>
                                ))}
                            </div>
                            <label style={S.label}>Cron 周期</label>
                            <input style={S.input} value={f.cron_expr} onChange={e => up('cron_expr', e.target.value)} placeholder="*/5 * * * *" />
                            <p style={S.hint}>窗口时长 {f.duration}s / 采样频率 {f.frequency}Hz。</p>
                        </div>
                    )}
                </div>

                {err && <p style={S.err}>{err}</p>}
                {ok && <div style={S.ok}>{ok} {cid && (isSch
                    ? <Link to="/timeline" style={{ color: '#4a6cf7', fontWeight: 'bold' }}>去时间轴 (SID: {cid})</Link>
                    : <Link to={`/task/result?tid=${cid}`} style={{ color: '#4a6cf7', fontWeight: 'bold' }}>查看任务 {cid}</Link>)}
                </div>}

                <div style={{ marginTop: 16, display: 'flex', gap: 10 }}>
                    <button style={{ ...S.btn, opacity: sub ? 0.6 : 1 }} onClick={submit} disabled={sub || kload}>
                        {sub ? '提交中...' : f.continuous ? '创建持续采集' : '提交任务'}
                    </button>
                    <button style={{ ...S.btn, background: '#999' }} onClick={onClose} disabled={sub}>取消</button>
                </div>
            </div>
        </div>
    );
}
