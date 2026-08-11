import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import { profiles } from '../api';

const S = {
    container: { maxWidth: 1280, margin: '0 auto', padding: 24, fontFamily: 'Arial, sans-serif', color: '#202124' },
    pageHead: { display: 'flex', justifyContent: 'space-between', gap: 16, alignItems: 'flex-end', marginBottom: 18 },
    eyebrow: { margin: '0 0 6px 0', color: '#667085', fontSize: 13 },
    title: { margin: 0, fontSize: 28, lineHeight: 1.2 },
    band: { background: '#fff', border: '1px solid #e5e7eb', borderRadius: 8, padding: 18, marginBottom: 16, boxShadow: '0 1px 3px rgba(16,24,40,0.08)' },
    controls: { display: 'grid', gridTemplateColumns: 'minmax(220px, 1.4fr) repeat(3, minmax(150px, 0.8fr)) auto', gap: 12, alignItems: 'end' },
    label: { display: 'block', color: '#475467', fontSize: 12, fontWeight: 700, marginBottom: 6 },
    select: { width: '100%', padding: '9px 10px', border: '1px solid #d0d7de', borderRadius: 6, background: '#fff', fontSize: 14 },
    input: { width: '100%', boxSizing: 'border-box', padding: '9px 10px', border: '1px solid #d0d7de', borderRadius: 6, fontSize: 14 },
    btn: { background: '#315efb', color: '#fff', border: 'none', padding: '10px 16px', borderRadius: 6, cursor: 'pointer', fontSize: 14, fontWeight: 700 },
    btnSecondary: { background: '#fff', color: '#315efb', border: '1px solid #c7d2fe', padding: '9px 12px', borderRadius: 6, cursor: 'pointer', fontSize: 13, fontWeight: 700, textDecoration: 'none' },
    contextGrid: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(150px, 1fr))', gap: 10 },
    contextItem: { background: '#f8fafc', border: '1px solid #edf0f3', borderRadius: 6, padding: 12 },
    contextLabel: { color: '#667085', fontSize: 12, marginBottom: 5 },
    contextValue: { color: '#111827', fontSize: 14, fontWeight: 700, wordBreak: 'break-word' },
    status: { display: 'inline-flex', padding: '3px 8px', borderRadius: 999, fontSize: 12, fontWeight: 700 },
    grid: { display: 'grid', gridTemplateColumns: 'minmax(0, 1.35fr) minmax(320px, 0.65fr)', gap: 16 },
    sectionHead: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 12, marginBottom: 12 },
    sectionTitle: { margin: 0, fontSize: 18 },
    subtle: { color: '#667085', fontSize: 12 },
    empty: { textAlign: 'center', padding: 46, color: '#667085', background: '#fbfcfe', border: '1px dashed #d0d7de', borderRadius: 8 },
    error: { background: '#fff3f3', border: '1px solid #ffcdd2', color: '#b42318', borderRadius: 8, padding: 12, marginBottom: 16 },
    flameWrap: { display: 'grid', gap: 8 },
    flameRow: { display: 'grid', gridTemplateColumns: 'minmax(140px, 220px) minmax(0, 1fr) 86px', gap: 10, alignItems: 'center', fontSize: 13 },
    barTrack: { height: 26, background: '#eef2f7', borderRadius: 6, overflow: 'hidden', border: '1px solid #e5e7eb' },
    bar: { height: '100%', background: '#2f6fed' },
    table: { width: '100%', borderCollapse: 'collapse' },
    th: { textAlign: 'left', padding: '10px 12px', borderBottom: '1px solid #d0d7de', color: '#475467', fontSize: 12, background: '#f8fafc' },
    td: { padding: '10px 12px', borderBottom: '1px solid #edf0f3', fontSize: 13 },
};

export default function ProfilesPage() {
    const [searchParams, setSearchParams] = useSearchParams();
    const [targets, setTargets] = useState([]);
    const [targetId, setTargetId] = useState(searchParams.get('target_id') || '');
    const [range, setRange] = useState('30m');
    const [profileType, setProfileType] = useState('cpu');
    const [flamegraph, setFlamegraph] = useState(null);
    const [topn, setTopn] = useState(null);
    const [loading, setLoading] = useState(true);
    const [querying, setQuerying] = useState(false);
    const [error, setError] = useState('');

    const selectedTarget = useMemo(() => targets.find(t => t.id === targetId) || null, [targets, targetId]);
    const hasExplicitTarget = Boolean(searchParams.get('target_id'));
    const timeWindow = useMemo(() => makeTimeWindow(range), [range]);

    const loadTargets = useCallback(async () => {
        setLoading(true);
        setError('');
        try {
            const res = await profiles.targets();
            if (res.code === 0) {
                const list = res.data?.targets || [];
                setTargets(list);
                const requested = searchParams.get('target_id');
                if (requested && list.some(t => t.id === requested)) {
                    setTargetId(requested);
                } else if (hasExplicitTarget && !targetId && list.length > 0) {
                    setTargetId(list[0].id);
                }
            } else {
                setError(res.message || '加载可观测对象失败');
            }
        } catch (err) {
            setError(err?.message || '加载可观测对象失败');
        } finally {
            setLoading(false);
        }
    }, [searchParams, targetId, hasExplicitTarget]);

    const refreshProfile = useCallback(async () => {
        if (!selectedTarget) return;
        setQuerying(true);
        setError('');
        const params = {
            target_id: selectedTarget.id,
            host: selectedTarget.ip,
            service: selectedTarget.service_name || 'hotmethod',
            from: timeWindow.from,
            to: timeWindow.to,
            profile_type: profileType,
        };
        try {
            const [fgRes, topRes] = await Promise.all([
                profiles.flamegraph(params),
                profiles.topn(params),
            ]);
            if (fgRes.code === 0) setFlamegraph(fgRes.data);
            if (topRes.code === 0) setTopn(topRes.data);
            if (fgRes.code !== 0) setError(fgRes.message || '加载火焰图失败');
            if (topRes.code !== 0) setError(topRes.message || '加载 TopN 失败');
        } catch (err) {
            setFlamegraph(null);
            setTopn(null);
            setError(err?.message || '持续 profiling 查询失败');
        } finally {
            setQuerying(false);
        }
    }, [selectedTarget, timeWindow, profileType]);

    useEffect(() => {
        loadTargets();
    }, [loadTargets]);

    useEffect(() => {
        if (!hasExplicitTarget || !targetId) return;
        setSearchParams({ target_id: targetId });
    }, [hasExplicitTarget, targetId, setSearchParams]);

    useEffect(() => {
        refreshProfile();
    }, [refreshProfile]);

    return (
        <div style={S.container}>
            <div style={S.pageHead}>
                <div>
                    <p style={S.eyebrow}>Continuous Profiling</p>
                    <h2 style={S.title}>持续 profiling</h2>
                </div>
                <Link to="/" style={S.btnSecondary}>返回主页</Link>
            </div>

            {error && <div style={S.error}>{error}</div>}

            {!hasExplicitTarget && (
                <section style={S.band}>
                    <h3 style={{ ...S.sectionTitle, marginBottom: 8 }}>请先选择主机</h3>
                    <p style={{ ...S.subtle, margin: '0 0 12px' }}>持续 profiling 已迁入主机性能中心。请选择一个主机后再查看 profile、TopN 和时间范围。</p>
                    <Link to="/" style={S.btnSecondary}>返回主机列表</Link>
                </section>
            )}

            {hasExplicitTarget && selectedTarget && (
                <section style={S.band}>
                    <p style={{ ...S.subtle, margin: 0 }}>
                        兼容入口：建议在 <Link to={`/hosts/${encodeURIComponent(selectedTarget.id)}?tab=profiling`} style={{ color: '#315efb', fontWeight: 700 }}>主机性能中心</Link> 查看持续 profiling。
                    </p>
                </section>
            )}

            {hasExplicitTarget && (
                <>
                    <section style={S.band}>
                        <div style={S.controls}>
                            <Field label="观测对象">
                                <select style={S.select} value={targetId} onChange={e => setTargetId(e.target.value)} disabled={loading || targets.length === 0}>
                                    {targets.length === 0 && <option value="">暂无可观测对象</option>}
                                    {targets.map(t => (
                                        <option key={t.id} value={t.id}>{t.hostname || t.ip} · {t.ip} · {t.service_name || 'hotmethod'}</option>
                                    ))}
                                </select>
                            </Field>
                            <Field label="时间范围">
                                <select style={S.select} value={range} onChange={e => setRange(e.target.value)}>
                                    <option value="15m">最近 15 分钟</option>
                                    <option value="30m">最近 30 分钟</option>
                                    <option value="1h">最近 1 小时</option>
                                    <option value="6h">最近 6 小时</option>
                                </select>
                            </Field>
                            <Field label="Profile 类型">
                                <select style={S.select} value={profileType} onChange={e => setProfileType(e.target.value)}>
                                    <option value="cpu">CPU</option>
                                    <option value="memory">Memory</option>
                                </select>
                            </Field>
                            <Field label="窗口">
                                <input style={S.input} value={`${formatTime(timeWindow.from)} - ${formatTime(timeWindow.to)}`} readOnly />
                            </Field>
                            <button style={S.btn} onClick={refreshProfile} disabled={!selectedTarget || querying}>{querying ? '刷新中' : '刷新'}</button>
                        </div>
                    </section>

                    <TargetContext target={selectedTarget} range={timeWindow} profileType={profileType} />

                    {!selectedTarget ? (
                        <div style={S.empty}>{loading ? '正在加载可观测对象...' : '暂无可观测对象。启动 drop_agent 或创建过按需任务后会出现在这里。'}</div>
                    ) : (
                        <div style={S.grid}>
                            <section style={S.band}>
                                <div style={S.sectionHead}>
                                    <h3 style={S.sectionTitle}>火焰图</h3>
                                    <span style={S.subtle}>{flamegraph?.source || 'mini-drop'} · {flamegraph?.unit || 'samples'}</span>
                                </div>
                                <FlamegraphView data={flamegraph} loading={querying} />
                            </section>
                            <section style={S.band}>
                                <div style={S.sectionHead}>
                                    <h3 style={S.sectionTitle}>TopN 热点</h3>
                                    <span style={S.subtle}>{topn?.items?.length || 0} functions</span>
                                </div>
                                <TopNTable data={topn} loading={querying} />
                            </section>
                        </div>
                    )}
                </>
            )}
        </div>
    );
}

function Field({ label, children }) {
    return (
        <label>
            <span style={S.label}>{label}</span>
            {children}
        </label>
    );
}

function TargetContext({ target, range, profileType }) {
    if (!target) return null;
    return (
        <section style={S.band}>
            <div style={S.contextGrid}>
                <ContextItem label="host" value={target.hostname || '-'} />
                <ContextItem label="ip" value={target.ip || '-'} />
                <ContextItem label="service" value={target.service_name || '-'} />
                <ContextItem label="environment" value={target.environment || '-'} />
                <ContextItem label="time range" value={`${formatTime(range.from)} - ${formatTime(range.to)}`} />
                <ContextItem label="profile type" value={profileType} />
                <ContextItem label="drop_agent" value={<StatusBadge value={target.drop_agent_status} />} />
                <ContextItem label="parca_agent" value={<StatusBadge value={target.parca_agent_status} />} />
            </div>
            {target.drop_agent_status !== 'online' && (
                <p style={{ ...S.subtle, margin: '12px 0 0' }}>drop_agent 当前不可用，持续 profiling 仍可查看；按需采样需要 Agent 恢复在线。</p>
            )}
            {target.parca_agent_status !== 'online' && (
                <p style={{ ...S.subtle, margin: '8px 0 0' }}>Parca agent 状态未知或未配置，画像区域会显示无数据或依赖错误。</p>
            )}
        </section>
    );
}

function ContextItem({ label, value }) {
    return (
        <div style={S.contextItem}>
            <div style={S.contextLabel}>{label}</div>
            <div style={S.contextValue}>{value}</div>
        </div>
    );
}

function StatusBadge({ value }) {
    const status = String(value || 'unknown');
    const tone = status === 'online'
        ? { background: '#dcfce7', color: '#166534' }
        : status === 'offline'
            ? { background: '#fee4e2', color: '#b42318' }
            : { background: '#f1f5f9', color: '#475467' };
    return <span style={{ ...S.status, ...tone }}>{status}</span>;
}

function FlamegraphView({ data, loading }) {
    if (loading && !data) return <div style={S.empty}>正在查询持续 profiling...</div>;
    if (!data || data.empty || !Array.isArray(data.nodes) || data.nodes.length === 0) {
        return <div style={S.empty}>{data?.message || '所选时间范围没有 profile 数据'}</div>;
    }
    const rows = flattenNodes(data.nodes).slice(0, 24);
    const max = Math.max(...rows.map(row => row.value), 1);
    return (
        <div style={S.flameWrap}>
            {rows.map((row, index) => (
                <div key={`${row.name}-${index}`} style={{ ...S.flameRow, paddingLeft: Math.min(row.depth * 18, 90) }}>
                    <span title={row.name}>{truncate(row.name, 34)}</span>
                    <div style={S.barTrack}>
                        <div style={{ ...S.bar, width: `${Math.max(4, (row.value / max) * 100)}%`, background: barColor(row.depth) }} />
                    </div>
                    <strong>{formatNumber(row.value)}</strong>
                </div>
            ))}
        </div>
    );
}

function TopNTable({ data, loading }) {
    if (loading && !data) return <div style={S.empty}>正在查询 TopN...</div>;
    const items = data?.items || [];
    if (data?.empty || items.length === 0) {
        return <div style={S.empty}>{data?.message || '暂无热点函数'}</div>;
    }
    return (
        <table style={S.table}>
            <thead>
                <tr>
                    <th style={S.th}>函数</th>
                    <th style={S.th}>累计</th>
                    <th style={S.th}>Self</th>
                </tr>
            </thead>
            <tbody>
                {items.slice(0, 12).map((item, index) => (
                    <tr key={`${item.name}-${index}`}>
                        <td style={S.td} title={item.name}>{truncate(item.name, 36)}</td>
                        <td style={S.td}>{formatNumber(item.value)} {item.unit || data.unit || ''}</td>
                        <td style={S.td}>{formatNumber(item.self)}</td>
                    </tr>
                ))}
            </tbody>
        </table>
    );
}

function makeTimeWindow(range) {
    const to = new Date();
    const minutes = { '15m': 15, '30m': 30, '1h': 60, '6h': 360 }[range] || 30;
    const from = new Date(to.getTime() - minutes * 60 * 1000);
    return { from: from.toISOString(), to: to.toISOString() };
}

function flattenNodes(nodes, depth = 0) {
    return nodes.flatMap(node => [
        { ...node, depth },
        ...flattenNodes(node.children || [], depth + 1),
    ]);
}

function formatTime(value) {
    if (!value) return '-';
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return String(value);
    return date.toLocaleString();
}

function formatNumber(value) {
    const num = Number(value);
    if (!Number.isFinite(num)) return '0';
    if (Math.abs(num) >= 1000) return num.toFixed(0);
    return num.toFixed(1);
}

function truncate(value, limit) {
    const text = String(value || '-');
    return text.length > limit ? `${text.slice(0, limit - 1)}...` : text;
}

function barColor(depth) {
    return ['#2f6fed', '#18a058', '#d97706', '#7c3aed', '#c2410c'][depth % 5];
}
