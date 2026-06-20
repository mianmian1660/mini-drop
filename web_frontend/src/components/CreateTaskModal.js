// ============================================================
// components/CreateTaskModal.js — 新建任务弹窗（完整版）
// 支持: 4种采集器 + eBPF模式 + 持续采集(Continuous Profiling)
// task_type 根据 profiler_type + event 自动推导
// ============================================================

import React, { useState, useEffect } from 'react';
import { Link } from 'react-router-dom';
import { tasks, agents, schedules } from '../api';

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

function deriveTaskType(pt, ev) {
    if (pt === 3 && (ev === 'io' || ev === 'blk' || ev === 'sched')) return 5;
    return 0;
}

const CRON_PRESETS = [
    { label: '每1分钟', value: '*/1 * * * *' },
    { label: '每5分钟', value: '*/5 * * * *' },
    { label: '每10分钟', value: '*/10 * * * *' },
    { label: '每30分钟', value: '*/30 * * * *' },
];

export default function CreateTaskModal({ onClose, onSuccess }) {
    const [f, setF] = useState({
        name: '', target_ip: '', target_pid: '', duration: 10, frequency: 99,
        profiler_type: 0, callgraph: 'fp', event: '',
        continuous: false, cron_expr: '*/5 * * * *',
    });
    const [sub, setSub] = useState(false);
    const [err, setErr] = useState('');
    const [ok, setOk] = useState('');
    const [cid, setCid] = useState('');
    const [isSch, setIsSch] = useState(false);
    const [agentList, setAgentList] = useState([]);
    const [aload, setAload] = useState(true);

    useEffect(() => {
        agents.list().then(r => {
            if (r.code === 0) {
                const list = r.data?.agents || [];
                setAgentList(list);
                const on = list.filter(a => a.online);
                if (on.length > 0)
                    setF(p => p.target_ip ? p : ({ ...p, target_ip: on[0].ip_addr }));
            }
        }).catch(() => { }).finally(() => setAload(false));
    }, []);

    const up = (k, v) => setF(p => {
        const n = { ...p, [k]: v };
        if (k === 'profiler_type' && v === 3 && !p.event) n.event = 'cpu';
        return n;
    });

    const submit = async () => {
        if (!f.name.trim()) { setErr('请输入任务名称'); return; }
        if (!f.target_ip) { setErr('请选择目标 Agent'); return; }
        const pid = parseInt(f.target_pid) || 0;
        const dur = parseInt(f.duration) || 10;
        const hz = parseInt(f.frequency) || 99;
        if (dur < 1 || dur > 3600) { setErr('时长需为 1-3600s'); return; }
        if (f.continuous && !f.cron_expr) { setErr('请输入 cron 表达式'); return; }

        setSub(true); setErr(''); setOk('');
        const tt = deriveTaskType(f.profiler_type, f.event);

        try {
            if (f.continuous) {
                const r = await schedules.create({
                    name: f.name.trim(), cron_expr: f.cron_expr, task_type: tt,
                    profiler_type: f.profiler_type, target_ip: f.target_ip,
                    target_pid: pid, duration: dur, frequency: hz,
                    callgraph: f.callgraph, event: f.event,
                });
                if (r.code === 0) {
                    setCid(r.data?.sid || ''); setIsSch(true); setOk('持续采集已创建！');
                    setTimeout(() => onSuccess?.(), 3000);
                }
                else setErr(r.message || '创建失败');
            } else {
                const r = await tasks.create({
                    name: f.name.trim(), target_ip: f.target_ip, target_pid: pid,
                    duration: dur, frequency: hz, task_type: tt,
                    profiler_type: f.profiler_type, callgraph: f.callgraph, event: f.event,
                });
                if (r.code === 0) {
                    setCid(r.data?.tid || ''); setIsSch(false); setOk('任务创建成功！');
                    setTimeout(() => onSuccess?.(), 2000);
                }
                else setErr(r.message || '创建失败');
            }
        } catch (e) { setErr('请求失败: ' + (e.message || '无法连接后端')); }
        finally { setSub(false); }
    };

    const modeLabel = f.profiler_type === 3
        ? (f.event === 'io' ? 'IO延迟直方图' : f.event === 'sched' ? '调度延迟直方图' : 'eBPF CPU火焰图')
        : f.profiler_type === 1 ? 'Java async-profiler'
            : f.profiler_type === 2 ? 'Go pprof'
                : 'perf CPU火焰图';

    return (
        <div style={S.overlay} onClick={onClose}>
            <div style={S.card} onClick={e => e.stopPropagation()}>
                <div style={S.header}>
                    <div>
                        <h3 style={S.title}>新建采样任务</h3>
                        <div style={S.hint}>选择 Agent 和采集器后提交，任务会自动进入状态流转。</div>
                    </div>
                    <button style={S.close} onClick={onClose} disabled={sub} aria-label="关闭">×</button>
                </div>

            <div style={{ marginBottom: 16 }}>
                <label style={S.label}>目标 Agent *</label>
                {aload ? <p style={{ fontSize: 12, color: '#999' }}>加载中...</p>
                    : agentList.length === 0 ? <p style={{ fontSize: 12, color: '#f44' }}>⚠️ 没有在线 Agent</p>
                        : <select style={S.select} value={f.target_ip} onChange={e => up('target_ip', e.target.value)}>
                            <option value="">-- 选择 Agent --</option>
                            {agentList.map(a => <option key={a.ip_addr} value={a.ip_addr}>{a.hostname} ({a.ip_addr}) {a.online ? '🟢' : '🔴'}</option>)}
                        </select>}
            </div>

            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16 }}>
                <div><label style={S.label}>任务名称 *</label><input style={S.input} placeholder="CPU采样-nginx" value={f.name} onChange={e => up('name', e.target.value)} /></div>
                <div><label style={S.label}>目标 PID（留空=整机）</label><input style={S.input} type="number" placeholder="留空" value={f.target_pid} onChange={e => up('target_pid', e.target.value)} /></div>
                <div><label style={S.label}>采样时长（秒）</label><input style={S.input} type="number" value={f.duration} onChange={e => up('duration', e.target.value)} /></div>
                <div><label style={S.label}>采样频率（Hz）</label><input style={S.input} type="number" value={f.frequency} onChange={e => up('frequency', e.target.value)} /></div>
                <div>
                    <label style={S.label}>采集器类型</label>
                    <select style={S.select} value={f.profiler_type} onChange={e => up('profiler_type', parseInt(e.target.value))}>
                        <option value={0}>perf (CPU采样)</option>
                        <option value={1}>async-profiler (Java)</option>
                        <option value={2}>pprof (Go)</option>
                        <option value={3}>eBPF (内核探针)</option>
                    </select>
                </div>
                <div>
                    <label style={S.label}>{f.profiler_type === 3 ? 'eBPF 追踪模式' : '调用图模式'}</label>
                    {f.profiler_type === 3 ? (
                        <select style={S.select} value={f.event} onChange={e => up('event', e.target.value)}>
                            <option value="cpu">CPU 性能分析 (火焰图)</option>
                            <option value="io">IO 延迟分布 (直方图)</option>
                            <option value="sched">调度延迟分布 (直方图)</option>
                        </select>
                    ) : (
                        <select style={S.select} value={f.callgraph} onChange={e => up('callgraph', e.target.value)}>
                            <option value="fp">fp (frame pointer)</option>
                            <option value="dwarf">dwarf (DWARF)</option>
                            <option value="lbr">lbr (LBR)</option>
                        </select>
                    )}
                </div>
            </div>

            <p style={S.hint}>📌 将生成: {modeLabel}</p>

            {/* 持续采集 */}
            <div style={{ ...S.section, background: f.continuous ? '#e8f0ff' : '#fafafa', border: f.continuous ? '1px solid #4a6cf7' : '1px solid #e0e0e0' }}>
                <label style={S.chk}>
                    <input type="checkbox" checked={f.continuous} onChange={e => up('continuous', e.target.checked)} />
                    <span style={{ fontWeight: 'bold', fontSize: 14 }}>🔄 持续采集 (Continuous Profiling)</span>
                </label>
                <p style={S.hint}>自动定时采集，可在"时间轴"页面回溯历史</p>
                {f.continuous && (
                    <div>
                        <label style={S.label}>Cron 周期</label>
                        <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', marginBottom: 8 }}>
                            {CRON_PRESETS.map(p => (
                                <button key={p.value} style={S.presetBtn(f.cron_expr === p.value)}
                                    onClick={() => up('cron_expr', p.value)}>{p.label}</button>
                            ))}
                        </div>
                        <input style={S.input} value={f.cron_expr} onChange={e => up('cron_expr', e.target.value)} placeholder="*/5 * * * *" />
                    </div>
                )}
            </div>

            {err && <p style={S.err}>❌ {err}</p>}
            {ok && <div style={S.ok}>✅ {ok} {cid && (isSch
                ? <Link to="/timeline" style={{ color: '#4a6cf7', fontWeight: 'bold' }}>去时间轴 → (SID: {cid})</Link>
                : <Link to={`/task/result?tid=${cid}`} style={{ color: '#4a6cf7', fontWeight: 'bold' }}>查看任务 → {cid}</Link>)}
            </div>}

            <div style={{ marginTop: 16, display: 'flex', gap: 10 }}>
                <button style={{ ...S.btn, opacity: sub ? 0.6 : 1 }} onClick={submit} disabled={sub}>
                    {sub ? '提交中...' : f.continuous ? '创建持续采集' : '提交任务'}
                </button>
                <button style={{ ...S.btn, background: '#999' }} onClick={onClose} disabled={sub}>取消</button>
            </div>
            </div>
        </div>
    );
}
