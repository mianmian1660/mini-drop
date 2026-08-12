import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { select } from 'd3-selection';
import { flamegraph as createFlamegraph } from 'd3-flame-graph';
import 'd3-flame-graph/dist/d3-flamegraph.css';
import { profiles } from '../api';
import {
    PROFILE_SAMPLE_PERIOD_MS,
    formatFlamegraphLabel,
    formatMetricValue,
    formatProfileTotal,
    formatRawMetric,
    isCPUTimeUnit,
    metricColumnLabel,
    profileUnitLabel,
} from '../utils/profileMetrics';

const S = {
    panel: { display: 'grid', gap: 14 },
    card: { background: '#fff', borderRadius: 8, padding: 16, border: '1px solid #e5e7eb', boxShadow: '0 1px 2px rgba(16,24,40,0.04)' },
    head: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 16, flexWrap: 'wrap' },
    titleLine: { display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap' },
    title: { margin: 0, fontSize: 18, letterSpacing: 0 },
    subtitle: { margin: '5px 0 0', color: '#667085', fontSize: 13 },
    actions: { display: 'flex', gap: 8, flexWrap: 'wrap', justifyContent: 'flex-end' },
    controls: { display: 'flex', gap: 12, alignItems: 'end', flexWrap: 'wrap' },
    field: { minWidth: 180, flex: '1 1 180px' },
    fieldWide: { minWidth: 260, flex: '2 1 300px' },
    label: { display: 'block', color: '#475467', fontSize: 12, fontWeight: 700, marginBottom: 6 },
    select: { width: '100%', padding: '8px 10px', border: '1px solid #d0d7de', borderRadius: 6, background: '#fff', fontSize: 13, height: 36 },
    btn: { background: '#315efb', color: '#fff', border: 'none', padding: '8px 12px', borderRadius: 6, cursor: 'pointer', fontSize: 13, fontWeight: 700 },
    btnSecondary: { background: '#fff', color: '#315efb', border: '1px solid #c7d2fe', padding: '7px 11px', borderRadius: 6, cursor: 'pointer', fontSize: 13, fontWeight: 700, textDecoration: 'none' },
    segmented: { display: 'inline-flex', border: '1px solid #d0d7de', borderRadius: 6, overflow: 'hidden', height: 36, background: '#fff' },
    segment: (active) => ({ border: 'none', borderRight: '1px solid #d0d7de', background: active ? '#eef2ff' : '#fff', color: active ? '#315efb' : '#475467', padding: '0 12px', cursor: 'pointer', fontSize: 13, fontWeight: 700 }),
    textInput: { width: '100%', padding: '8px 10px', border: '1px solid #d0d7de', borderRadius: 6, background: '#fff', fontSize: 13, height: 36, boxSizing: 'border-box' },
    stateBadge: { display: 'inline-flex', alignItems: 'center', border: '1px solid #abefc6', background: '#ecfdf3', color: '#067647', borderRadius: 999, padding: '3px 8px', fontSize: 12, fontWeight: 700 },
    summaryGrid: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(170px, 1fr))', gap: 0, borderTop: '1px solid #eef2f6', borderBottom: '1px solid #eef2f6' },
    metric: { padding: '10px 14px 10px 0', minWidth: 0 },
    metricLabel: { color: '#667085', fontSize: 12, marginBottom: 4 },
    metricValue: { color: '#111827', fontSize: 16, fontWeight: 700, wordBreak: 'break-word', lineHeight: 1.35 },
    chipWrap: { display: 'flex', flexWrap: 'wrap', gap: 8 },
    chip: { display: 'inline-flex', alignItems: 'center', gap: 6, border: '1px solid #eaecf0', background: '#fff', color: '#344054', borderRadius: 999, padding: '4px 8px', fontSize: 12, fontWeight: 700 },
    chipKey: { color: '#667085', fontWeight: 700 },
    sectionHead: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 12, marginBottom: 12, flexWrap: 'wrap' },
    subtle: { color: '#667085', fontSize: 12 },
    inlineNote: { color: '#667085', fontSize: 12, lineHeight: 1.5 },
    flameBox: { height: 520, overflowX: 'auto', overflowY: 'hidden', border: '1px solid #eaecf0', borderRadius: 8, background: '#fff', padding: 6 },
    empty: { textAlign: 'center', padding: 44, color: '#667085', background: '#fff', border: '1px dashed #d0d7de', borderRadius: 8 },
    error: { background: '#fff3f3', border: '1px solid #ffcdd2', color: '#b42318', borderRadius: 8, padding: 12 },
    warn: { color: '#b42318', background: '#fff6f5', border: '1px solid #fda29b', borderRadius: 6, padding: '9px 12px', fontSize: 13 },
    info: { color: '#475467', background: '#fff', border: '1px solid #eaecf0', borderRadius: 6, padding: '8px 10px', fontSize: 12, lineHeight: 1.55 },
    tableWrap: { overflowX: 'auto' },
    table: { width: '100%', borderCollapse: 'collapse' },
    th: { textAlign: 'left', padding: '9px 10px', borderBottom: '1px solid #eaecf0', color: '#475467', fontSize: 12, background: '#fff' },
    td: { padding: '8px 10px', borderBottom: '1px solid #f2f4f7', fontSize: 13, verticalAlign: 'top', lineHeight: 1.45 },
    details: { borderTop: '1px solid #eaecf0', padding: '10px 0 0', background: '#fff' },
    detailsSummary: { cursor: 'pointer', color: '#475467', fontSize: 13, fontWeight: 700 },
    mono: { margin: '10px 0 0', padding: 10, background: '#111827', color: '#e5e7eb', borderRadius: 6, overflowX: 'auto', fontSize: 12, lineHeight: 1.5 },
};

const RANGE_OPTIONS = [
    ['15m', '最近 15 分钟'],
    ['30m', '最近 30 分钟'],
    ['1h', '最近 1 小时'],
    ['6h', '最近 6 小时'],
];

const FLAMEGRAPH_MAX_DEPTH = 80;
const FLAMEGRAPH_MAX_NODES = 3500;

export default function ContinuousProfilingPanel({ target, targets = [], targetId = '', onTargetChange, showTargetSelect = false }) {
    const [range, setRange] = useState('30m');
    const [profileType, setProfileType] = useState('cpu');
    const [flamegraph, setFlamegraph] = useState(null);
    const [topn, setTopn] = useState(null);
    const [querying, setQuerying] = useState(false);
    const [error, setError] = useState('');
    const [resetKey, setResetKey] = useState(0);
    const [scope, setScope] = useState('host');
    const [selectedComm, setSelectedComm] = useState('');
    const [commValues, setCommValues] = useState([]);
    const [commAvailable, setCommAvailable] = useState(false);
    const [commMessage, setCommMessage] = useState('');
    const [commLoading, setCommLoading] = useState(false);
    const timeWindow = useMemo(() => makeTimeWindow(range), [range]);
    const parcaURL = flamegraph?.parca_url || topn?.parca_url || target?.parca_query_url || target?.parca_ui_url;
    const hasFlamegraph = flamegraph && !flamegraph.empty && Array.isArray(flamegraph.nodes) && flamegraph.nodes.length > 0;
    const sampleState = sampleStateForTarget(target, flamegraph, topn);
    const unit = flamegraph?.unit || topn?.unit || '';
    const activeFilters = useMemo(() => (scope === 'process' && selectedComm.trim() ? { comm: selectedComm.trim() } : {}), [scope, selectedComm]);
    const activeFilterText = activeFilters.comm ? `comm=${activeFilters.comm}` : '';
    const scopeLabel = activeFilters.comm ? `进程级持续 profiling / ${activeFilterText} / CPU 占用时长` : '主机级持续 profiling / CPU 占用时长';

    const refresh = useCallback(async () => {
        if (!target) return;
        setQuerying(true);
        setError('');
        const params = {
            target_id: target.id,
            host: target.ip,
            service: target.service_name || 'hotmethod',
            from: timeWindow.from,
            to: timeWindow.to,
            profile_type: profileType,
        };
        if (Object.keys(activeFilters).length > 0) {
            params.filters = JSON.stringify(activeFilters);
        }
        try {
            const [fgRes, topRes] = await Promise.all([
                profiles.flamegraph(params),
                profiles.topn(params),
            ]);
            if (fgRes.code === 0) setFlamegraph(fgRes.data);
            if (topRes.code === 0) setTopn(topRes.data);
            if (fgRes.code !== 0 || topRes.code !== 0) {
                setError(fgRes.message || topRes.message || '持续 profiling 查询失败');
            }
        } catch (err) {
            setFlamegraph(null);
            setTopn(null);
            setError(err?.message || '持续 profiling 查询失败');
        } finally {
            setQuerying(false);
        }
    }, [target, timeWindow, profileType, activeFilters]);

    useEffect(() => {
        refresh();
    }, [refresh]);

    useEffect(() => {
        setSelectedComm('');
        setScope('host');
    }, [target?.id]);

    useEffect(() => {
        if (!target || profileType !== 'cpu') {
            setCommValues([]);
            setCommAvailable(false);
            setCommMessage('');
            return undefined;
        }
        let cancelled = false;
        setCommLoading(true);
        setCommMessage('');
        profiles.labelValues({
            target_id: target.id,
            host: target.ip,
            service: target.service_name || 'hotmethod',
            from: timeWindow.from,
            to: timeWindow.to,
            profile_type: profileType,
            label: 'comm',
        }).then(res => {
            if (cancelled) return;
            if (res.code !== 0) {
                setCommValues([]);
                setCommAvailable(false);
                setCommMessage(res.message || '加载进程标签失败');
                return;
            }
            const data = res.data || {};
            const values = prioritizeProcessNames(data.values || []);
            setCommValues(values);
            setCommAvailable(Boolean(data.available));
            setCommMessage(data.message || (values.length === 0 ? '当前时间范围没有可选进程' : ''));
            if (selectedComm && !values.includes(selectedComm)) {
                setSelectedComm('');
            }
        }).catch(err => {
            if (cancelled) return;
            setCommValues([]);
            setCommAvailable(false);
            setCommMessage(err?.message || '加载进程标签失败');
        }).finally(() => {
            if (!cancelled) setCommLoading(false);
        });
        return () => {
            cancelled = true;
        };
    }, [target, timeWindow, profileType, selectedComm]);

    if (!target) {
        return <div style={S.empty}>暂无可观测对象。启动 drop_agent 或创建过按需任务后会出现在这里。</div>;
    }

    return (
        <div style={S.panel}>
            <section style={S.card}>
                <div style={S.head}>
                    <div>
                        <div style={S.titleLine}>
                            <h3 style={S.title}>持续 profiling</h3>
                            <span style={S.stateBadge}>{sampleState}</span>
                        </div>
                        <p style={S.subtitle}>{target.hostname || target.ip} · {target.service_name || 'hotmethod'}</p>
                    </div>
                    <div style={S.actions}>
                        {parcaURL && <a href={parcaURL} target="_blank" rel="noreferrer" style={S.btnSecondary}>打开 Parca</a>}
                        <button style={S.btnSecondary} onClick={() => setResetKey(v => v + 1)} disabled={!hasFlamegraph}>重置缩放</button>
                        <button style={S.btn} onClick={refresh} disabled={querying}>{querying ? '刷新中' : '刷新'}</button>
                    </div>
                </div>
                <div style={{ ...S.controls, marginTop: 14 }}>
                    {showTargetSelect && (
                        <Field label="观测对象" wide>
                            <select style={S.select} value={targetId} onChange={e => onTargetChange?.(e.target.value)} disabled={targets.length === 0}>
                                {targets.map(t => <option key={t.id} value={t.id}>{t.hostname || t.ip} · {t.ip} · {t.service_name || 'hotmethod'}</option>)}
                            </select>
                        </Field>
                    )}
                    <Field label="时间范围">
                        <select style={S.select} value={range} onChange={e => setRange(e.target.value)}>
                            {RANGE_OPTIONS.map(([value, label]) => <option key={value} value={value}>{label}</option>)}
                        </select>
                    </Field>
                    <Field label="Profile 类型">
                        <select style={S.select} value={profileType} onChange={e => setProfileType(e.target.value)}>
                            <option value="cpu">CPU</option>
                            <option value="memory">Memory</option>
                        </select>
                    </Field>
                    <Field label="范围">
                        <span style={S.segmented}>
                            <button type="button" style={S.segment(scope === 'host')} onClick={() => setScope('host')}>整机</button>
                            <button type="button" style={{ ...S.segment(scope === 'process'), borderRight: 'none' }} onClick={() => setScope('process')} disabled={!commAvailable}>进程</button>
                        </span>
                    </Field>
                    {scope === 'process' && (
                        <Field label="进程 comm" wide>
                            <input
                                style={S.textInput}
                                list="profile-comm-values"
                                value={selectedComm}
                                onChange={e => setSelectedComm(e.target.value)}
                                disabled={!commAvailable || commLoading}
                                placeholder={commLoading ? '加载进程...' : '选择或输入 comm'}
                            />
                            <datalist id="profile-comm-values">
                                {commValues.map(value => <option key={value} value={value} />)}
                            </datalist>
                        </Field>
                    )}
                </div>
                <div style={{ ...S.summaryGrid, marginTop: 14 }}>
                    <Metric label="样本状态" value={sampleState} />
                    <Metric label="时间窗口" value={`${formatTime(timeWindow.from)} - ${formatTime(timeWindow.to)}`} />
                    <Metric label="总占用" value={formatProfileTotal(flamegraph?.total || topn?.total, unit)} />
                    <Metric label="热点函数" value={`${topn?.items?.length || 0} 个`} />
                </div>
                <div style={{ ...S.info, marginTop: 14 }}>
                    {scopeLabel}；非样本数、非单次请求耗时。comm 是 Linux task comm，可能被截断到约 15 字符；19Hz 采样约 {PROFILE_SAMPLE_PERIOD_MS.toFixed(1)} 毫秒/样本。
                    {scope === 'process' && commMessage ? ` ${commMessage}` : ''}
                </div>
                <LabelChips target={target} />
            </section>

            {error && <div style={S.error}>{error}</div>}

            <section style={S.card}>
                <div style={S.sectionHead}>
                    <div>
                        <h3 style={S.title}>火焰图</h3>
                        <div style={S.subtle}>
                            {flamegraph?.source || 'mini-drop'} · {profileUnitLabel(flamegraph?.unit || unit)} · 宽度按 {isCPUTimeUnit(flamegraph?.unit || unit) ? 'CPU 占用时长' : '原始 value'} 计算
                        </div>
                    </div>
                    <span style={S.subtle}>{hasFlamegraph ? `${countNodes(flamegraph.nodes)} 个栈帧节点` : '暂无栈帧节点'}</span>
                </div>
                <FlamegraphCanvas key={resetKey} data={flamegraph} loading={querying} parcaURL={parcaURL} filterText={activeFilterText} />
            </section>

            <section style={S.card}>
                <div style={S.sectionHead}>
                    <h3 style={S.title}>热点 TopN</h3>
                    <span style={S.subtle}>{topn?.items?.length || 0} functions · {profileUnitLabel(topn?.unit || unit)}</span>
                </div>
                <TopNTable data={topn} loading={querying} parcaURL={parcaURL} filterText={activeFilterText} />
            </section>

            <details style={S.details}>
                <summary style={S.detailsSummary}>诊断信息</summary>
                <pre style={S.mono}>{diagnosticText({ target, flamegraph, topn, timeWindow, profileType, filters: activeFilters })}</pre>
            </details>
        </div>
    );
}

function Field({ label, children, wide = false }) {
    return <label style={wide ? S.fieldWide : S.field}><span style={S.label}>{label}</span>{children}</label>;
}

function Metric({ label, value }) {
    return <div style={S.metric}><div style={S.metricLabel}>{label}</div><div style={S.metricValue}>{value || '-'}</div></div>;
}

function LabelChips({ target }) {
    const labels = labelEntries(target);
    return (
        <div style={{ ...S.chipWrap, marginTop: 14 }}>
            {labels.map(([key, value]) => (
                <span key={key} style={S.chip}><span style={S.chipKey}>{key}</span>{value || '-'}</span>
            ))}
        </div>
    );
}

function FlamegraphCanvas({ data, loading, parcaURL, filterText = '' }) {
    const graphRef = useRef(null);
    const chartRef = useRef(null);
    const [renderError, setRenderError] = useState('');
    const renderData = useMemo(() => data ? prepareFlamegraphForRender(data) : null, [data]);

    useEffect(() => {
        if (!graphRef.current || !renderData || renderData.empty || !Array.isArray(renderData.nodes) || renderData.nodes.length === 0) return undefined;
        const selection = select(graphRef.current);
        selection.selectAll('*').remove();
        setRenderError('');
        try {
            const width = Math.max(graphRef.current.clientWidth - 16 || 1120, 1120);
            const height = Math.max(320, Math.min(500, (maxDepth(renderData.nodes) + 3) * 18));
            const chart = createFlamegraph()
                .width(width)
                .height(height)
                .cellHeight(17)
                .transitionDuration(120)
                .minFrameSize(0.8)
                .sort(true)
                .label(d => formatFlamegraphLabel(d, renderData.unit))
                .title('');
            chartRef.current = chart;
            selection.datum(miniDropToD3Flamegraph(renderData)).call(chart);
        } catch (err) {
            chartRef.current = null;
            setRenderError(err?.message || '火焰图渲染失败');
        }
        return () => {
            chartRef.current = null;
            selection.selectAll('*').remove();
        };
    }, [renderData]);

    if (loading && !data) return <div style={S.empty}>正在查询持续 profiling...</div>;
    if (!data || data.empty || !Array.isArray(data.nodes) || data.nodes.length === 0) {
        return <ProfileEmpty message={data?.message || (filterText ? `该时间范围内 ${filterText} 无样本` : '所选时间范围没有 profile 数据')} url={data?.parca_url || parcaURL} />;
    }

    return (
        <>
            {renderData?.render_note && <div style={{ ...S.inlineNote, marginBottom: 8 }}>{renderData.render_note}</div>}
            <div ref={graphRef} style={S.flameBox} />
            {renderError && <div style={{ ...S.warn, marginTop: 12 }}>火焰图渲染失败：{renderError}</div>}
            {renderError && <ProfileEmpty message="仍可通过 Parca 查询入口或下方 TopN 排查热点。" url={data?.parca_url || parcaURL} />}
        </>
    );
}

function prepareFlamegraphForRender(data) {
    if (!data || data.empty || !Array.isArray(data.nodes)) return data;
    const originalNodes = countNodes(data.nodes);
    const originalDepth = maxDepth(data.nodes);
    if (originalNodes <= FLAMEGRAPH_MAX_NODES && originalDepth <= FLAMEGRAPH_MAX_DEPTH) {
        return data;
    }
    const budget = { remaining: FLAMEGRAPH_MAX_NODES };
    const nodes = pruneProfileNodes(data.nodes, 1, budget);
    const shownNodes = countNodes(nodes);
    const notes = [];
    if (originalDepth > FLAMEGRAPH_MAX_DEPTH) notes.push(`深度 ${originalDepth} -> ${maxDepth(nodes)}`);
    if (originalNodes > shownNodes) notes.push(`节点 ${shownNodes}/${originalNodes}`);
    return {
        ...data,
        nodes,
        render_note: `火焰图已按展示性能裁剪（${notes.join('，')}）。TopN 和诊断信息仍保留完整查询结果。`,
    };
}

function pruneProfileNodes(nodes, depth, budget) {
    if (!Array.isArray(nodes) || budget.remaining <= 0) return [];
    const out = [];
    const sorted = [...nodes].sort((a, b) => Number(b.value || 0) - Number(a.value || 0));
    for (const node of sorted) {
        if (budget.remaining <= 0) break;
        budget.remaining -= 1;
        const next = { ...node };
        if (depth >= FLAMEGRAPH_MAX_DEPTH || budget.remaining <= 0) {
            next.children = [];
        } else {
            next.children = pruneProfileNodes(node.children || [], depth + 1, budget);
        }
        out.push(next);
    }
    return out;
}

function TopNTable({ data, loading, parcaURL, filterText = '' }) {
    if (loading && !data) return <div style={S.empty}>正在查询 TopN...</div>;
    const items = data?.items || [];
    if (data?.empty || items.length === 0) {
        return <ProfileEmpty message={data?.message || (filterText ? `该时间范围内 ${filterText} 无样本` : '暂无热点函数')} url={data?.parca_url || parcaURL} />;
    }
    return (
        <div style={S.tableWrap}>
            <table style={S.table}>
                <thead>
                    <tr>
                        <th style={{ ...S.th, width: '62%' }}>函数</th>
                        <th style={S.th}>{metricColumnLabel(data.unit, '累计占用时长')}</th>
                        <th style={S.th}>{metricColumnLabel(data.unit, '自身占用时长')}</th>
                    </tr>
                </thead>
                <tbody>
                    {items.slice(0, 20).map((item, index) => (
                        <tr key={`${item.name}-${index}`}>
                            <td style={S.td} title={item.name}>{truncate(item.name, 72)}</td>
                            <td style={S.td}>{formatMetricValue(item.value, item.unit || data.unit)}</td>
                            <td style={S.td}>{formatMetricValue(item.self, item.unit || data.unit)}</td>
                        </tr>
                    ))}
                </tbody>
            </table>
        </div>
    );
}

function ProfileEmpty({ message, url }) {
    return (
        <div style={S.empty}>
            <div style={{ fontWeight: 700, color: '#475467', marginBottom: 6 }}>{message}</div>
            {url && <a href={url} target="_blank" rel="noreferrer" style={S.btnSecondary}>打开 Parca</a>}
        </div>
    );
}

function miniDropToD3Flamegraph(data) {
    return {
        name: 'root',
        value: Number(data?.total || sumNodeValues(data?.nodes || [])) || 1,
        children: (data?.nodes || []).map(toD3Node),
    };
}

function toD3Node(node) {
    return {
        name: node.name || 'unknown',
        value: Math.max(0, Number(node.value || 0)),
        children: (node.children || []).map(toD3Node),
    };
}

function maxDepth(nodes, depth = 1) {
    if (!Array.isArray(nodes) || nodes.length === 0) return depth;
    return Math.max(...nodes.map(node => maxDepth(node.children || [], depth + 1)));
}

function countNodes(nodes) {
    return (nodes || []).reduce((sum, node) => sum + 1 + countNodes(node.children || []), 0);
}

function sumNodeValues(nodes) {
    return nodes.reduce((sum, node) => sum + Number(node.value || 0), 0);
}

function sampleStateForTarget(target, flamegraph, topn) {
    const status = String(target?.parca_agent_status || 'unknown');
    if (status === 'online_with_samples') return '有样本';
    if (status === 'online_no_samples') return '在线但暂无样本';
    if (status === 'online') return '在线';
    if (status === 'parca_unreachable') return 'Parca 不可达';
    if (status === 'query_unsupported') return '查询不兼容';
    if (flamegraph?.empty || topn?.empty) return '暂无样本';
    if (status === 'offline') return '离线';
    return '未知';
}

function labelEntries(target) {
    const labels = target?.labels || {};
    return [
        ['job', labels.job || target?.service_name],
        ['instance', labels.instance || target?.ip],
        ['node', labels.node || target?.hostname],
        ['env', labels.env || target?.environment],
    ];
}

function labelSelectorForTarget(target, filters = {}) {
    const labels = { ...(target?.labels || {}) };
    if (!labels.node && target?.hostname) labels.node = target.hostname;
    if (!labels.instance && target?.ip) labels.instance = target.ip;
    if (!labels.job && target?.service_name) labels.job = target.service_name;
    if (!labels.env && target?.environment) labels.env = target.environment;
    Object.entries(filters || {}).forEach(([key, value]) => {
        if (value) labels[key] = value;
    });
    return `{${Object.entries(labels).map(([k, v]) => `${k}="${v}"`).join(', ')}}`;
}

function diagnosticText({ target, flamegraph, topn, timeWindow, profileType, filters = {} }) {
    return [
        `target: ${target?.id || '-'}`,
        `profile_type: ${profileType}`,
        `time_range: ${timeWindow.from} -> ${timeWindow.to}`,
        `selector: ${labelSelectorForTarget(target, filters)}`,
        `filters: ${JSON.stringify(filters || {})}`,
        `query: ${flamegraph?.query || topn?.query || '-'}`,
        `unit: ${flamegraph?.unit || topn?.unit || '-'}`,
        `total_raw_value: ${formatRawMetric(flamegraph?.total || topn?.total || 0, flamegraph?.unit || topn?.unit || '')}`,
        `parca_url: ${flamegraph?.parca_url || topn?.parca_url || target?.parca_query_url || target?.parca_ui_url || '-'}`,
    ].join('\n');
}

function makeTimeWindow(range) {
    const to = new Date();
    const minutes = { '15m': 15, '30m': 30, '1h': 60, '6h': 360 }[range] || 30;
    const from = new Date(to.getTime() - minutes * 60 * 1000);
    return { from: from.toISOString(), to: to.toISOString() };
}

function formatTime(value) {
    if (!value) return '-';
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return String(value);
    return date.toLocaleString();
}

function truncate(value, limit) {
    const text = String(value || '-');
    return text.length > limit ? `${text.slice(0, limit - 1)}...` : text;
}

function prioritizeProcessNames(values) {
    const preferred = ['test_c_profilin', 'test_python_pro', 'python3', 'drop_agent', 'parca-agent'];
    const unique = Array.from(new Set((values || []).filter(Boolean).map(String)));
    return unique.sort((a, b) => {
        const ai = preferred.indexOf(a);
        const bi = preferred.indexOf(b);
        if (ai >= 0 || bi >= 0) {
            if (ai < 0) return 1;
            if (bi < 0) return -1;
            return ai - bi;
        }
        return a.localeCompare(b);
    });
}
