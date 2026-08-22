// ============================================================
// pages/ScheduleDetailPage.js — 周期任务独立时间轴详情页
// ============================================================
// 统一支持两个路由入口（复用同一详情组件）：
//   /hosts/:targetId/schedules/:sid   （主机入口，返回该主机）
//   /schedules/:sid                   （/timeline 旧兼容入口，返回周期任务列表）
// 页面顶部展示计划状态与关键参数，下方为时间轴工作区
// （时间范围 / 状态 / 采集器 / 结果筛选、时间轴图、窗口列表、取消、基线对比）。
// ============================================================

import React, { useCallback, useEffect, useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { schedules } from '../api';
import ScheduleTimeline from '../components/ScheduleTimeline';
import { collectorLabelFromTask, parseRequestParams } from '../utils/collectors';
import { cronHumanLabel } from '../utils/cron';
import { formatDateTime } from '../utils/time';

const S = {
    container: { width: '100%', maxWidth: 1320, minWidth: 0, margin: '0 auto', padding: '22px 28px 36px', fontFamily: 'Arial, sans-serif', color: '#101828' },
    pageHead: { display: 'flex', justifyContent: 'space-between', gap: 16, alignItems: 'flex-end', flexWrap: 'wrap', marginBottom: 14 },
    eyebrow: { margin: '0 0 6px 0', color: '#667085', fontSize: 13 },
    title: { margin: 0, fontSize: 26, lineHeight: 1.25, letterSpacing: 0 },
    titleMeta: { color: '#667085', fontSize: 13, fontWeight: 400, marginTop: 4, wordBreak: 'break-all' },
    actions: { display: 'flex', gap: 10, flexWrap: 'wrap', justifyContent: 'flex-end' },
    btnSecondary: { background: '#fff', color: '#315efb', border: '1px solid #c7d2fe', padding: '8px 12px', borderRadius: 6, cursor: 'pointer', fontSize: 13, fontWeight: 700, textDecoration: 'none' },
    btnDanger: { background: '#fff', color: '#b42318', border: '1px solid #fda29b', padding: '8px 12px', borderRadius: 6, cursor: 'pointer', fontSize: 13, fontWeight: 700 },
    card: { minWidth: 0, maxWidth: '100%', background: '#fff', borderRadius: 8, padding: 20, marginBottom: 16, border: '1px solid #e5e7eb', boxShadow: '0 1px 2px rgba(16,24,40,.04)' },
    grid: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))', gap: '12px 24px', marginTop: 6 },
    item: { minWidth: 0 },
    label: { color: '#667085', fontSize: 12, marginBottom: 5 },
    value: { color: '#111827', fontSize: 14, fontWeight: 650, wordBreak: 'break-word', lineHeight: 1.45 },
    badge: { display: 'inline-flex', alignItems: 'center', padding: '3px 9px', borderRadius: 999, fontSize: 12, fontWeight: 700 },
    cron: { display: 'inline-flex', background: '#f2f4f7', color: '#475467', borderRadius: 6, padding: '2px 7px', fontSize: 12, fontFamily: 'ui-monospace,SFMono-Regular,Menlo,monospace' },
    loading: { textAlign: 'center', padding: 60, color: '#667085' },
    error: { background: '#fff3f3', border: '1px solid #ffcdd2', color: '#b42318', borderRadius: 8, padding: 12, marginBottom: 16 },
};

export default function ScheduleDetailPage() {
    const { targetId: rawTargetId, sid } = useParams();
    const navigate = useNavigate();
    const targetId = rawTargetId ? decodeURIComponent(rawTargetId) : null;
    const [schedule, setSchedule] = useState(null);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState('');
    const [busy, setBusy] = useState(false);

    const loadDetail = useCallback(async () => {
        if (!sid) return;
        setLoading(true);
        setError('');
        try {
            const res = await schedules.detail(sid);
            if (res.code !== 0) throw new Error(res.message || '加载周期任务失败');
            setSchedule(res.data || null);
        } catch (err) {
            setError(err?.message || '加载周期任务失败');
        } finally {
            setLoading(false);
        }
    }, [sid]);

    useEffect(() => { loadDetail(); }, [loadDetail]);

    const toggle = async () => {
        if (!schedule) return;
        setBusy(true);
        setError('');
        try {
            const res = await schedules.toggle(schedule.sid);
            if (res.code !== 0) throw new Error(res.message || '切换状态失败');
            await loadDetail();
        } catch (err) {
            setError(err?.message || '切换状态失败');
        } finally {
            setBusy(false);
        }
    };

    const remove = async () => {
        if (!schedule) return;
        if (!window.confirm(`确定删除周期任务「${schedule.name}」？相关历史采集窗口保留。`)) return;
        setBusy(true);
        setError('');
        try {
            const res = await schedules.delete(schedule.sid);
            if (res.code !== 0) throw new Error(res.message || '删除失败');
            navigate(targetId ? `/hosts/${encodeURIComponent(targetId)}?tab=timeline` : '/timeline');
        } catch (err) {
            setError(err?.message || '删除失败');
            setBusy(false);
        }
    };

    const backLink = targetId
        ? { to: `/hosts/${encodeURIComponent(targetId)}?tab=timeline`, label: '返回该主机' }
        : { to: '/timeline', label: '返回周期任务列表' };

    if (loading) return <div style={S.container}><div style={S.loading}>⏳ 加载周期任务详情...</div></div>;

    if (!schedule) {
        return (
            <div style={S.container}>
                <div style={S.error}>{error || '周期任务不存在'}</div>
                <Link to={backLink.to} style={S.btnSecondary}>{backLink.label}</Link>
            </div>
        );
    }

    const params = parseRequestParams(schedule.request_params);
    const collector = collectorLabelFromTask({ task_kind: schedule.task_kind, type: schedule.task_type, profiler_type: schedule.profiler_type, request_params: schedule.request_params });
    const running = schedule.enabled === true;

    return (
        <div style={S.container}>
            <div style={S.pageHead}>
                <div>
                    <p style={S.eyebrow}>Periodic Deep Sampling</p>
                    <h2 style={S.title}>{schedule.name}</h2>
                    <div style={S.titleMeta}>SID：{schedule.sid}</div>
                </div>
                <div style={S.actions}>
                    <Link to={backLink.to} style={S.btnSecondary}>{backLink.label}</Link>
                    {schedule.can_manage && (
                        <>
                            <button style={S.btnSecondary} disabled={busy} onClick={toggle}>
                                {running ? '停用计划' : '启用计划'}
                            </button>
                            <button style={S.btnDanger} disabled={busy} onClick={remove}>删除计划</button>
                        </>
                    )}
                </div>
            </div>

            {error && <div style={S.error}>{error}</div>}

            <section style={S.card}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap', marginBottom: 4 }}>
                    <span style={{ ...S.badge, background: running ? '#16a34a' : '#64748b', color: '#fff' }}>{running ? '启用' : '停用'}</span>
                    <span style={S.cron} title={schedule.cron_expr}>{cronHumanLabel(schedule.cron_expr)}</span>
                </div>
                <div style={S.grid}>
                    <ContextItem label="目标 IP" value={schedule.target_ip || '-'} />
                    <ContextItem label="采集器" value={collector || '-'} />
                    <ContextItem label="采样参数" value={paramSummary(params)} />
                    <ContextItem label="创建者" value={schedule.user_name || '系统'} />
                    <ContextItem label="创建时间" value={formatDateTime(schedule.created_at) || '-'} />
                    <ContextItem label="最近运行" value={formatDateTime(schedule.last_run_at) || '-'} />
                    <ContextItem label="下次运行" value={running ? (formatDateTime(schedule.next_run_at) || '-') : '已停用'} />
                </div>
            </section>

            <ScheduleTimeline sid={schedule.sid} />
        </div>
    );
}

function ContextItem({ label, value }) {
    return (
        <div style={S.item}>
            <div style={S.label}>{label}</div>
            <div style={S.value}>{value}</div>
        </div>
    );
}

function paramSummary(params) {
    const parts = [];
    if (params.duration) parts.push(`${params.duration}s`);
    if (params.frequency) parts.push(`${params.frequency}Hz`);
    if (params.event) parts.push(params.event);
    if (params.callgraph) parts.push(`callgraph=${params.callgraph}`);
    if (params.pprof_url) parts.push('pprof');
    if (params.target_pid) parts.push(`pid=${params.target_pid}`);
    return parts.length ? parts.join(' · ') : '默认';
}
