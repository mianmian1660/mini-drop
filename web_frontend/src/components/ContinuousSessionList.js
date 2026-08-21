import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { Link } from 'react-router-dom';
import { continuous } from '../api';
import { continuousStateColor, continuousStateLabel, decodeJSONField, formatRelativeTime, signalLabel } from '../utils/continuous';

const S = {
    card: { minWidth: 0, maxWidth: '100%', background: '#fff', borderRadius: 8, padding: 20, border: '1px solid #e5e7eb', boxShadow: '0 1px 2px rgba(16,24,40,.04)' },
    head: { display: 'flex', justifyContent: 'space-between', gap: 12, alignItems: 'center', flexWrap: 'wrap', marginBottom: 14 },
    title: { margin: 0, fontSize: 18, color: '#101828' },
    subtle: { color: '#667085', fontSize: 12 },
    toolbar: { display: 'flex', justifyContent: 'space-between', gap: 10, flexWrap: 'wrap', marginBottom: 14 },
    filters: { display: 'flex', gap: 8, flexWrap: 'wrap', flex: '1 1 560px' },
    input: { minWidth: 220, flex: '1 1 240px', height: 36, padding: '7px 10px', boxSizing: 'border-box', border: '1px solid #d0d5dd', borderRadius: 6, fontSize: 13 },
    select: { height: 36, padding: '7px 10px', border: '1px solid #d0d5dd', borderRadius: 6, background: '#fff', fontSize: 13 },
    button: { height: 36, padding: '0 12px', color: '#315efb', background: '#fff', border: '1px solid #c7d2fe', borderRadius: 6, fontWeight: 700, cursor: 'pointer' },
    tableWrap: { width: '100%', minWidth: 0, maxWidth: '100%', overflowX: 'auto', overflowY: 'hidden' },
    table: { width: '100%', borderCollapse: 'collapse', minWidth: 920 },
    th: { textAlign: 'left', padding: '10px 12px', borderBottom: '1px solid #d0d5dd', color: '#475467', background: '#f8fafc', fontSize: 12, whiteSpace: 'nowrap' },
    td: { padding: '11px 12px', borderBottom: '1px solid #edf0f3', color: '#344054', fontSize: 13, verticalAlign: 'top' },
    name: { color: '#101828', fontWeight: 700, marginBottom: 3 },
    mono: { maxWidth: 300, color: '#475467', fontSize: 12, fontFamily: 'ui-monospace,SFMono-Regular,Menlo,monospace', wordBreak: 'break-all' },
    badge: { display: 'inline-flex', borderRadius: 999, padding: '3px 8px', fontSize: 12, fontWeight: 700, whiteSpace: 'nowrap' },
    chips: { display: 'flex', flexWrap: 'wrap', gap: 4, maxWidth: 220 },
    chip: { display: 'inline-flex', background: '#f2f4f7', color: '#475467', borderRadius: 999, padding: '2px 6px', fontSize: 11, fontWeight: 700 },
    link: { color: '#315efb', fontWeight: 700, textDecoration: 'none', marginRight: 10, whiteSpace: 'nowrap' },
    stop: { color: '#b42318', background: 'transparent', border: 0, padding: 0, fontWeight: 700, cursor: 'pointer', whiteSpace: 'nowrap' },
    warn: { marginBottom: 14, color: '#b54708', background: '#fffaeb', border: '1px solid #fedf89', borderRadius: 6, padding: 10, fontSize: 13 },
    error: { marginBottom: 14, color: '#b42318', background: '#fff6f5', border: '1px solid #fda29b', borderRadius: 6, padding: 10, fontSize: 13 },
    empty: { textAlign: 'center', color: '#667085', padding: 38, border: '1px dashed #d0d5dd', borderRadius: 8 },
};

export default function ContinuousSessionList({ target, refreshToken = 0 }) {
    const [sessions, setSessions] = useState([]);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState('');
    const [keyword, setKeyword] = useState('');
    const [status, setStatus] = useState('');
    const [scope, setScope] = useState('');
    const [stopping, setStopping] = useState('');

    const load = useCallback(async (silent = false) => {
        if (!silent) setLoading(true);
        setError('');
        try {
            const response = await continuous.sessions({ target_ip: target.ip, page: 1, page_size: 100 });
            if (response.code !== 0) throw new Error(response.message || '加载持续采集任务失败');
            setSessions(response.data?.sessions || []);
        } catch (err) {
            setError(err?.message || '加载持续采集任务失败');
        } finally {
            if (!silent) setLoading(false);
        }
    }, [target.ip]);

    useEffect(() => { load(); }, [load, refreshToken]);
    useEffect(() => {
        const timer = window.setInterval(() => load(true), 5000);
        return () => window.clearInterval(timer);
    }, [load]);

    const filtered = useMemo(() => {
        const query = keyword.trim().toLowerCase();
        return sessions.filter(session => (!status || session.observed_state === status)
            && (!scope || normalizedScope(session) === scope)
            && (!query || `${session.name} ${session.selector_exe}`.toLowerCase().includes(query)));
    }, [sessions, keyword, status, scope]);

    const stop = async session => {
        if (!window.confirm(`停止持续采集“${session.name}”？停止后不会自动恢复。`)) return;
        setStopping(session.sid);
        setError('');
        try {
            const response = await continuous.stopSession(session.sid);
            if (response.code !== 0) throw new Error(response.message || '停止失败');
            await load(true);
        } catch (err) {
            setError(err?.response?.data?.message || err?.message || '停止失败');
        } finally {
            setStopping('');
        }
    };

    const degradedCount = sessions.filter(session => session.continuity_mode === 'degraded' && session.desired_state === 'running').length;
    return <section style={S.card}>
        <div style={S.head}><div><h3 style={S.title}>持续采集</h3><span style={S.subtle}>任务按用户期望持续运行；等待进程和 Agent 离线不会自动终止任务</span></div><span style={S.subtle}>共 {sessions.length} 条</span></div>
        {degradedCount > 0 && <div style={S.warn}>{degradedCount} 个活动任务正在降级运行。任务仍严格限制采集范围，但滚动采集窗口可能存在短暂空档。</div>}
        {error && <div style={S.error}>{error}</div>}
        <div className="continuous-list-toolbar" style={S.toolbar}>
            <div className="continuous-list-filters" style={S.filters}>
                <input style={S.input} value={keyword} onChange={event => setKeyword(event.target.value)} placeholder="搜索名称 / exe" />
                <select style={S.select} value={status} onChange={event => setStatus(event.target.value)}><option value="">全部状态</option><option value="running">运行中</option><option value="waiting">等待进程</option><option value="degraded">降级运行</option><option value="pending">待启动</option><option value="stopping">停止中</option><option value="stopped">已停止</option><option value="offline">Agent 离线</option><option value="error">异常</option></select>
                <select style={S.select} value={scope} onChange={event => setScope(event.target.value)}><option value="">全部范围</option><option value="host">整机</option><option value="process">进程</option></select>
            </div>
            <button style={S.button} onClick={() => load()} disabled={loading}>{loading ? '刷新中' : '刷新'}</button>
        </div>
        {loading ? <div style={S.empty}>正在加载持续采集任务...</div> : filtered.length === 0 ? <div style={S.empty}>{sessions.length ? '没有匹配的持续采集任务' : '暂无持续采集任务。点击右上角“新建持续采集”开始。'}</div> : <div className="table-scroll" style={S.tableWrap}><table style={S.table}>
            <thead><tr><th style={S.th}>名称</th><th style={S.th}>范围与目标</th><th style={S.th}>状态</th><th style={S.th}>信号</th><th style={S.th}>最近数据</th><th style={S.th}>持续时间 / 创建人</th><th style={S.th}>操作</th></tr></thead>
            <tbody>{filtered.map(session => {
                const state = session.observed_state || 'pending';
                const [background, color] = continuousStateColor(state);
                const signals = decodeJSONField(session.signals, ['cpu_profile', 'io_latency', 'io_syscall_latency', 'sched_latency']);
                const active = decodeJSONField(session.active_processes, []);
                const running = session.desired_state === 'running';
                return <tr key={session.sid}>
                    <td style={S.td}><div style={S.name}>{session.name}</div><span style={S.subtle}>{shortSID(session.sid)}</span></td>
                    <td style={S.td}>{normalizedScope(session) === 'process' ? <><strong>进程 · {active.length} 个活动实例</strong><div style={S.mono} title={session.selector_exe}>{session.selector_exe}</div></> : <strong>整机</strong>}</td>
                    <td style={S.td}><span style={{ ...S.badge, background, color }}>{continuousStateLabel(state)}</span>{session.continuity_mode === 'degraded' && <div style={{ ...S.subtle, color: '#b54708', marginTop: 4 }}>降级连续性</div>}</td>
                    <td style={S.td}><div style={S.chips}>{signals.map(signal => <span key={signal} style={S.chip}>{signalLabel(signal)}</span>)}</div></td>
                    <td style={S.td}>{formatRelativeTime(session.last_upload_at)}</td>
                    <td style={S.td}>{duration(session.started_at, session.stopped_at)}<div style={S.subtle}>{session.user_name || 'system'}</div></td>
                    <td style={S.td}><Link style={S.link} to={`/hosts/${encodeURIComponent(target.id)}/continuous/${encodeURIComponent(session.sid)}`}>查看</Link>{running && <button style={S.stop} disabled={stopping === session.sid} onClick={() => stop(session)}>{stopping === session.sid ? '停止中' : '停止'}</button>}</td>
                </tr>;
            })}</tbody>
        </table></div>}
    </section>;
}

function normalizedScope(session) { return session.scope === 'process' ? 'process' : 'host'; }
function shortSID(value) { return String(value || '').length > 18 ? `${value.slice(0, 10)}...${value.slice(-4)}` : value; }
function duration(start, end) {
    const from = new Date(start).getTime();
    const to = end ? new Date(end).getTime() : Date.now();
    if (!Number.isFinite(from) || !Number.isFinite(to)) return '-';
    const seconds = Math.max(0, Math.floor((to - from) / 1000));
    if (seconds < 3600) return `${Math.floor(seconds / 60)} 分钟`;
    if (seconds < 86400) return `${(seconds / 3600).toFixed(1)} 小时`;
    return `${(seconds / 86400).toFixed(1)} 天`;
}
