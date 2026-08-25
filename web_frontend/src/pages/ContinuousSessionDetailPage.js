import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { Link, useParams, useSearchParams } from 'react-router-dom';
import { continuous, profiles } from '../api';
import ContinuousProfilingPanel from '../components/ContinuousProfilingPanel';
import SentinelCard from '../components/SentinelCard';
import { continuousStateColor, continuousStateLabel, decodeJSONField, formatRelativeTime, selectorIdentity, selectorModeLabel } from '../utils/continuous';

const S = {
    container: { width: '100%', maxWidth: 1320, minWidth: 0, margin: '0 auto', padding: '22px 28px 36px', fontFamily: 'Arial, sans-serif', color: '#101828' },
    head: { display: 'flex', justifyContent: 'space-between', alignItems: 'flex-end', gap: 16, flexWrap: 'wrap', marginBottom: 14 },
    eyebrow: { margin: '0 0 6px', color: '#667085', fontSize: 13 },
    title: { margin: 0, fontSize: 28, letterSpacing: 0 },
    actions: { display: 'flex', gap: 10, flexWrap: 'wrap' },
    back: { background: '#fff', color: '#315efb', border: '1px solid #c7d2fe', borderRadius: 6, padding: '8px 12px', textDecoration: 'none', fontSize: 13, fontWeight: 700 },
    stop: { background: '#fff', color: '#b42318', border: '1px solid #fda29b', borderRadius: 6, padding: '8px 12px', fontSize: 13, fontWeight: 700, cursor: 'pointer' },
    card: { minWidth: 0, maxWidth: '100%', background: '#fff', borderRadius: 8, padding: 16, border: '1px solid #e5e7eb', boxShadow: '0 1px 2px rgba(16,24,40,.04)', marginBottom: 14 },
    meta: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit,minmax(min(170px,100%),1fr))', minWidth: 0, maxWidth: '100%', borderTop: '1px solid #eef2f6', marginTop: 14 },
    metric: { padding: '12px 14px 0 0', minWidth: 0 },
    label: { color: '#667085', fontSize: 12, marginBottom: 4 },
    value: { color: '#101828', fontSize: 14, fontWeight: 700, wordBreak: 'break-word', lineHeight: 1.45 },
    badge: { display: 'inline-flex', borderRadius: 999, padding: '3px 8px', fontSize: 12, fontWeight: 700 },
    warn: { color: '#b54708', background: '#fffaeb', border: '1px solid #fedf89', borderRadius: 6, padding: 11, fontSize: 13, lineHeight: 1.5, marginBottom: 14 },
    error: { color: '#b42318', background: '#fff6f5', border: '1px solid #fda29b', borderRadius: 6, padding: 11, fontSize: 13, marginBottom: 14 },
    loading: { textAlign: 'center', padding: 48, color: '#667085' },
    instances: { display: 'flex', flexWrap: 'wrap', gap: 7, marginTop: 10 },
    instance: { border: '1px solid #eaecf0', background: '#f8fafc', color: '#344054', borderRadius: 6, padding: '5px 8px', fontSize: 12 },
    details: { marginTop: 12, borderTop: '1px solid #eef2f6', paddingTop: 10 },
    summary: { cursor: 'pointer', color: '#475467', fontSize: 13, fontWeight: 700 },
};

export default function ContinuousSessionDetailPage() {
    const { targetId: rawTargetId, sid: rawSID } = useParams();
    const [searchParams] = useSearchParams();
    const targetId = decodeURIComponent(rawTargetId || '');
    const sid = decodeURIComponent(rawSID || '');
    const [target, setTarget] = useState(null);
    const [session, setSession] = useState(null);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState('');
    const [stopping, setStopping] = useState(false);
    const initialQuery = useMemo(() => ({
        from: searchParams.get('from') || '',
        to: searchParams.get('to') || '',
        profileType: searchParams.get('profile_type') || 'cpu',
        stackScope: searchParams.get('stack_scope') || 'all',
        filters: parseFilters(searchParams.get('filters')),
    }), [searchParams]);

    const load = useCallback(async (silent = false) => {
        if (!silent) setLoading(true);
        setError('');
        try {
            const [targetsResponse, sessionResponse] = await Promise.all([profiles.targets(), continuous.detail(sid)]);
            if (targetsResponse.code !== 0) throw new Error(targetsResponse.message || '加载主机失败');
            if (sessionResponse.code !== 0) throw new Error(sessionResponse.message || '加载持续采集任务失败');
            const loadedSession = sessionResponse.data?.session || null;
            if (!loadedSession) throw new Error('未找到持续采集任务或无权限访问');
            const targets = targetsResponse.data?.targets || [];
            const selected = targetId
                ? targets.find(item => item.id === targetId)
                : targets.find(item => item.ip === loadedSession.target_ip);
            if (!selected) throw new Error('未找到当前主机或无权限访问');
            if (targetId && loadedSession.target_ip !== selected.ip) throw new Error('持续采集任务不属于当前主机');
            setTarget(selected);
            setSession(loadedSession);
        } catch (err) {
            setError(err?.message || '加载持续采集详情失败');
        } finally {
            if (!silent) setLoading(false);
        }
    }, [sid, targetId]);

    useEffect(() => { load(); }, [load]);
    useEffect(() => {
        const timer = window.setInterval(() => load(true), 5000);
        return () => window.clearInterval(timer);
    }, [load]);

    const activeProcesses = useMemo(() => decodeJSONField(session?.active_processes, []), [session?.active_processes]);
    const sessionSignals = useMemo(() => decodeJSONField(session?.signals, ['cpu_profile']), [session?.signals]);
    const identity = useMemo(() => selectorIdentity(session), [session]);

    const stop = async () => {
        if (!window.confirm(`停止持续采集“${session.name}”？停止后不会自动恢复。`)) return;
        setStopping(true);
        try {
            const response = await continuous.stopSession(session.sid);
            if (response.code !== 0) throw new Error(response.message || '停止失败');
            await load(true);
        } catch (err) {
            setError(err?.response?.data?.message || err?.message || '停止失败');
        } finally {
            setStopping(false);
        }
    };

    if (loading) return <div style={S.container}><div style={S.loading}>正在加载持续采集详情...</div></div>;
    if (error && (!target || !session)) return <div style={S.container}><div style={S.error}>{error}</div><Link style={S.back} to={`/hosts/${encodeURIComponent(targetId)}?tab=profiling`}>返回持续采集列表</Link></div>;
    if (!target || !session) return null;
    const state = session.observed_state || 'pending';
    const [stateBackground, stateColor] = continuousStateColor(state);
    const running = session.desired_state === 'running';

    return <div className="performance-page" style={S.container}>
        <div className="performance-page-header" style={S.head}>
            <div><p style={S.eyebrow}>Performance Center · 持续采集</p><h2 style={S.title}>{session.name}</h2></div>
            <div className="performance-page-actions" style={S.actions}><Link style={S.back} to={`/hosts/${encodeURIComponent(target.id)}?tab=profiling`}>返回持续采集列表</Link>{running && session.can_manage && <button style={S.stop} onClick={stop} disabled={stopping}>{stopping ? '停止中...' : '停止持续采集'}</button>}</div>
        </div>
        {error && <div style={S.error}>{error}</div>}
        {session.continuity_mode === 'degraded' && <div style={S.warn}><strong>降级连续性：</strong>{session.degradation_reason || '当前使用 PID 范围受限的滚动采集回退；窗口切换可能产生短暂空档。'} 采集范围不会退化为整机。</div>}
        <section style={S.card}>
            <div><span style={{ ...S.badge, background: stateBackground, color: stateColor }}>{continuousStateLabel(state)}</span></div>
            <div style={S.meta}>
                <Metric label="期望状态" value={session.desired_state === 'running' ? '持续运行' : '已请求停止'} />
                <Metric label="实际状态" value={continuousStateLabel(state)} />
                <Metric label="采集范围" value={session.scope === 'process' ? `进程 · ${selectorModeLabel(identity.mode)}` : '整机'} />
                <Metric label="创建者" value={session.user_name || '系统'} />
                <Metric label="最近上传" value={formatRelativeTime(session.last_upload_at)} />
                <Metric label="连续性" value={session.continuity_mode === 'strict' ? '严格连续' : '降级'} />
                <Metric label="活动实例" value={session.scope === 'process' ? `${activeProcesses.length} 个` : '整机'} />
            </div>
            {session.scope === 'process' && <details style={S.details}>
                <summary style={S.summary}>进程实例明细</summary>
                <div style={{ ...S.label, marginTop: 12 }}>selector 类型</div>
                <div style={S.value}>{selectorModeLabel(identity.mode)}</div>
                <div style={{ ...S.label, marginTop: 12 }}>精确身份</div>
                <div style={S.value}>{identity.exe || identity.detail}</div>
                {identity.detail && identity.mode !== 'exe_all_instances' && <div style={{ ...S.label, marginTop: 8 }}>{identity.detail}</div>}
                <div style={{ ...S.label, marginTop: 12 }}>重启跟随策略</div>
                <div style={S.value}>{identity.follow}</div>
                {state === 'waiting' && session.degradation_reason && <div style={{ ...S.label, marginTop: 12 }}>等待原因</div>}
                {state === 'waiting' && session.degradation_reason && <div style={S.value}>{session.degradation_reason}</div>}
                <div style={S.instances}>{activeProcesses.length ? activeProcesses.map(process => <span key={`${process.pid}-${process.process_start_ms}`} style={S.instance}>PID {process.pid} · {formatStart(process.process_start_ms)}</span>) : <span style={S.instance}>等待匹配进程</span>}</div>
            </details>}
        </section>
        <section style={S.card}><SentinelCard targetIP={session.target_ip} signals={sessionSignals} /></section>
        <ContinuousProfilingPanel target={target} fixedSession={session} initialQuery={initialQuery} />
    </div>;
}

function Metric({ label, value }) { return <div style={S.metric}><div style={S.label}>{label}</div><div style={S.value}>{value || '-'}</div></div>; }
function formatStart(value) { const date = new Date(Number(value)); return Number.isNaN(date.getTime()) ? `start ${value}` : date.toLocaleString(); }
function parseFilters(value) {
    if (!value) return {};
    try {
        const parsed = JSON.parse(value);
        return parsed && typeof parsed === 'object' ? parsed : {};
    } catch (error) {
        return {};
    }
}
