// ============================================================
// pages/HomePage.js — 主机优先 Dashboard
// ============================================================

import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { Link } from 'react-router-dom';
import { profiles } from '../api';

const styles = {
    container: { maxWidth: 1280, margin: '0 auto', padding: 24, fontFamily: 'Arial, sans-serif', color: '#202124' },
    pageHead: { display: 'flex', justifyContent: 'space-between', gap: 16, alignItems: 'flex-end', marginBottom: 18 },
    eyebrow: { margin: '0 0 6px 0', color: '#667085', fontSize: 13 },
    title: { margin: 0, fontSize: 28, lineHeight: 1.2 },
    card: { background: '#fff', borderRadius: 8, padding: 24, marginBottom: 16, border: '1px solid #e5e7eb', boxShadow: '0 1px 3px rgba(16,24,40,0.08)' },
    metricGrid: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(180px, 1fr))', gap: 12, marginBottom: 16 },
    metric: { background: '#fff', border: '1px solid #e5e7eb', borderRadius: 8, padding: 16 },
    metricLabel: { color: '#667085', fontSize: 12, marginBottom: 8 },
    metricValue: { fontSize: 24, fontWeight: 700 },
    toolbar: { display: 'flex', justifyContent: 'space-between', gap: 12, alignItems: 'center', marginBottom: 14 },
    input: { flex: 1, minWidth: 220, padding: '9px 12px', border: '1px solid #d0d7de', borderRadius: 6, fontSize: 14 },
    btnSecondary: { background: '#fff', color: '#315efb', border: '1px solid #c7d2fe', padding: '8px 12px', borderRadius: 6, cursor: 'pointer', fontSize: 13, fontWeight: 700, textDecoration: 'none' },
    table: { width: '100%', borderCollapse: 'collapse' },
    th: { textAlign: 'left', padding: '12px 16px', borderBottom: '1px solid #d0d7de', color: '#475467', fontSize: 12, background: '#f8fafc' },
    td: { padding: '12px 16px', borderBottom: '1px solid #edf0f3', fontSize: 14, verticalAlign: 'top' },
    rowLink: { color: '#315efb', fontWeight: 700, textDecoration: 'none' },
    badge: { display: 'inline-flex', alignItems: 'center', padding: '3px 9px', borderRadius: 999, fontSize: 12, fontWeight: 'bold' },
    empty: { textAlign: 'center', padding: 40, color: '#667085' },
    loading: { textAlign: 'center', padding: 40, color: '#999' },
    error: { background: '#fff3f3', border: '1px solid #ffcdd2', color: '#b42318', borderRadius: 8, padding: 12, marginBottom: 16 },
    subtle: { color: '#667085', fontSize: 12 },
};

export default function HomePage() {
    const [targets, setTargets] = useState([]);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState('');
    const [keyword, setKeyword] = useState('');

    const loadTargets = useCallback(async () => {
        setLoading(true);
        setError('');
        try {
            const res = await profiles.targets();
            if (res.code === 0) {
                setTargets(res.data?.targets || []);
            } else {
                setError(res.message || '加载可观测对象失败');
            }
        } catch (err) {
            setError(err?.message || '加载可观测对象失败');
        } finally {
            setLoading(false);
        }
    }, []);

    useEffect(() => {
        loadTargets();
    }, [loadTargets]);

    const filteredTargets = useMemo(() => {
        const q = keyword.trim().toLowerCase();
        if (!q) return targets;
        return targets.filter(t => [
            t.hostname,
            t.ip,
            t.service_name,
            t.environment,
            t.drop_agent_status,
            t.parca_agent_status,
        ].some(value => String(value || '').toLowerCase().includes(q)));
    }, [targets, keyword]);

    const onlineDropAgents = targets.filter(t => t.drop_agent_status === 'online').length;
    const parcaReady = targets.filter(t => t.parca_agent_status === 'online').length;
    const pprofScrapeReady = targets.filter(t => t.pprof_scrape_status === 'available').length;

    if (loading) {
        return <div style={styles.container}><p style={styles.loading}>加载主机列表...</p></div>;
    }

    return (
        <div style={styles.container}>
            <div style={styles.pageHead}>
                <div>
                    <p style={styles.eyebrow}>Mini-Drop Dashboard</p>
                    <h2 style={styles.title}>选择主机或服务</h2>
                </div>
                <button style={styles.btnSecondary} onClick={loadTargets}>刷新</button>
            </div>

            <div style={styles.metricGrid}>
                <Metric label="可观测对象" value={targets.length} />
                <Metric label="drop_agent 在线" value={`${onlineDropAgents}/${targets.length}`} />
                <Metric label="pprof scrape 可用" value={`${pprofScrapeReady}/${targets.length}`} />
                <Metric label="eBPF agent 在线" value={`${parcaReady}/${targets.length}`} />
            </div>

            {error && <div style={styles.error}>{error}</div>}

            <section style={styles.card}>
                <div style={styles.toolbar}>
                    <input
                        style={styles.input}
                        placeholder="搜索主机名 / IP / 服务 / 环境"
                        value={keyword}
                        onChange={e => setKeyword(e.target.value)}
                    />
                    <span style={styles.subtle}>先进入主机，再查看该主机任务、时间轴和持续 profiling</span>
                </div>

                {filteredTargets.length === 0 ? (
                    <p style={styles.empty}>{targets.length === 0 ? '暂无可观测对象。启动 drop_agent 或创建按需任务后会出现在这里。' : '没有匹配的主机或服务'}</p>
                ) : (
                    <table style={styles.table}>
                        <thead>
                            <tr>
                                <th style={styles.th}>主机</th>
                                <th style={styles.th}>IP 地址</th>
                                <th style={styles.th}>服务</th>
                                <th style={styles.th}>环境</th>
                                <th style={styles.th}>drop_agent</th>
                                <th style={styles.th}>parca_agent</th>
                                <th style={styles.th}>最近活动</th>
                                <th style={styles.th}>操作</th>
                            </tr>
                        </thead>
                        <tbody>
                            {filteredTargets.map(target => (
                                <tr key={target.id}>
                                    <td style={styles.td}>
                                        <Link to={`/hosts/${encodeURIComponent(target.id)}`} style={styles.rowLink}>
                                            {target.hostname || target.ip || '-'}
                                        </Link>
                                    </td>
                                    <td style={styles.td}>{target.ip || '-'}</td>
                                    <td style={styles.td}>{target.service_name || 'hotmethod'}</td>
                                    <td style={styles.td}>{target.environment || '-'}</td>
                                    <td style={styles.td}><StatusPill value={target.drop_agent_status} /></td>
                                    <td style={styles.td}><StatusPill value={target.parca_agent_status} /></td>
                                    <td style={styles.td}>{formatTime(target.last_profile_at || target.last_seen) || '-'}</td>
                                    <td style={styles.td}>
                                        <Link to={`/hosts/${encodeURIComponent(target.id)}`} style={styles.rowLink}>进入主机</Link>
                                    </td>
                                </tr>
                            ))}
                        </tbody>
                    </table>
                )}
            </section>

            <p style={styles.subtle}>
                主机详情页会展示该主机的 Agent 状态、审计日志、按需任务、时间轴和持续 profiling。
            </p>
        </div>
    );
}

function Metric({ label, value }) {
    return (
        <div style={styles.metric}>
            <div style={styles.metricLabel}>{label}</div>
            <div style={styles.metricValue}>{value}</div>
        </div>
    );
}

function StatusPill({ value }) {
    const status = String(value || 'unknown');
    const color = status === 'online'
        ? '#16a34a'
        : status === 'offline'
            ? '#dc2626'
            : status === 'unconfigured'
                ? '#64748b'
                : '#7c3aed';
    return <span style={{ ...styles.badge, background: color, color: '#fff' }}>{status}</span>;
}

function formatTime(value) {
    if (!value) return '';
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return String(value);
    return date.toLocaleString();
}
