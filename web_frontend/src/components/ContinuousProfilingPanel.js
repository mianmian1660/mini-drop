import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { continuous, profiles } from '../api';
import InteractiveFlamegraph, { countProfileNodes } from './InteractiveFlamegraph';
import {
    formatMetricValue,
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
    metaLine: { display: 'flex', flexWrap: 'wrap', gap: 10, alignItems: 'center', marginTop: 12 },
    metaItem: { display: 'inline-flex', alignItems: 'center', gap: 6, border: '1px solid #eaecf0', background: '#fff', color: '#344054', borderRadius: 999, padding: '5px 9px', fontSize: 12, fontWeight: 700 },
    metaItemWarn: { borderColor: '#fda29b', background: '#fff6f5', color: '#b42318' },
    metaKey: { color: '#667085', fontWeight: 700 },
    chipWrap: { display: 'flex', flexWrap: 'wrap', gap: 8 },
    chip: { display: 'inline-flex', alignItems: 'center', gap: 6, border: '1px solid #eaecf0', background: '#fff', color: '#344054', borderRadius: 999, padding: '4px 8px', fontSize: 12, fontWeight: 700 },
    chipKey: { color: '#667085', fontWeight: 700 },
    sectionHead: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 12, marginBottom: 12, flexWrap: 'wrap' },
    subtle: { color: '#667085', fontSize: 12 },
    inlineNote: { color: '#667085', fontSize: 12, lineHeight: 1.5 },
    flameBox: { height: 560, overflowX: 'auto', overflowY: 'auto', border: '1px solid #eaecf0', borderRadius: 8, background: '#fff', padding: 6 },
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

export default function ContinuousProfilingPanel({ target, targets = [], targetId = '', onTargetChange, showTargetSelect = false }) {
    const [range, setRange] = useState('30m');
    const [profileType, setProfileType] = useState('cpu');
    const [signalTab, setSignalTab] = useState('cpu');
    const [stackScope, setStackScope] = useState('all');
    const [flamegraph, setFlamegraph] = useState(null);
    const [topn, setTopn] = useState(null);
    const [histogram, setHistogram] = useState(null);
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
    const profileURL = flamegraph?.profile_url || topn?.profile_url || target?.profile_url;
    const hasFlamegraph = flamegraph && !flamegraph.empty && Array.isArray(flamegraph.nodes) && flamegraph.nodes.length > 0;
    const sampleState = sampleStateForTarget(target, flamegraph, topn);
    const sessionMeta = continuousSessionMeta(target);
    const uploadState = uploadFreshness(sessionMeta);
    const unit = flamegraph?.unit || topn?.unit || '';
    const activeFilters = useMemo(() => (scope === 'process' && selectedComm.trim() ? { comm: selectedComm.trim() } : {}), [scope, selectedComm]);
    const activeFilterText = activeFilters.comm ? `comm=${activeFilters.comm}` : '';
    const stackScopeLabel = stackScope === 'user' ? '用户栈' : stackScope === 'kernel' ? '内核栈' : '混合栈';
    const scopeLabel = activeFilters.comm ? `进程级 Native Continuous Profiling / ${activeFilterText} / ${stackScopeLabel}` : `整机 Native Continuous Profiling / ${stackScopeLabel}`;

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
            if (signalTab === 'cpu') {
                setHistogram(null);
                const cpuParams = { ...params };
                if (stackScope !== 'all') cpuParams.stack_scope = stackScope;
                const [fgRes, topRes] = await Promise.all([
                    profiles.flamegraph(cpuParams),
                    profiles.topn(cpuParams),
                ]);
                if (fgRes.code === 0) setFlamegraph(fgRes.data);
                if (topRes.code === 0) setTopn(topRes.data);
                if (fgRes.code !== 0 || topRes.code !== 0) {
                    setError(fgRes.message || topRes.message || 'Native Continuous Profiling 查询失败');
                }
            } else {
                setFlamegraph(null);
                setTopn(null);
                const signalType = signalTab === 'io' ? 'io_latency' : 'sched_latency';
                const histRes = await continuous.histogram({ ...params, signal_type: signalType });
                if (histRes.code === 0) setHistogram(histRes.data);
                if (histRes.code !== 0) {
                    setError(histRes.message || 'Native Continuous eBPF histogram 查询失败');
                }
            }
        } catch (err) {
            setFlamegraph(null);
            setTopn(null);
            setHistogram(null);
            setError(err?.message || 'Native Continuous Profiling 查询失败');
        } finally {
            setQuerying(false);
        }
    }, [target, timeWindow, profileType, activeFilters, signalTab, stackScope]);

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
                            <h3 style={S.title}>Native Continuous Profiling</h3>
                            <span style={S.stateBadge}>{sampleState}</span>
                        </div>
                        <p style={S.subtitle}>{target.hostname || target.ip} · {target.service_name || 'hotmethod'}</p>
                    </div>
                    <div style={S.actions}>
                        {profileURL && <a href={profileURL} target="_blank" rel="noreferrer" style={S.btnSecondary}>打开 Profile</a>}
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
                    <Field label="信号">
                        <span style={S.segmented}>
                            <button type="button" style={S.segment(signalTab === 'cpu')} onClick={() => setSignalTab('cpu')}>CPU</button>
                            <button type="button" style={S.segment(signalTab === 'io')} onClick={() => setSignalTab('io')}>IO 延迟</button>
                            <button type="button" style={{ ...S.segment(signalTab === 'sched'), borderRight: 'none' }} onClick={() => setSignalTab('sched')}>调度延迟</button>
                        </span>
                    </Field>
                    {signalTab === 'cpu' && (
                        <Field label="栈视图">
                            <span style={S.segmented}>
                                <button type="button" style={S.segment(stackScope === 'all')} onClick={() => setStackScope('all')}>混合栈</button>
                                <button type="button" style={S.segment(stackScope === 'user')} onClick={() => setStackScope('user')}>用户栈</button>
                                <button type="button" style={{ ...S.segment(stackScope === 'kernel'), borderRight: 'none' }} onClick={() => setStackScope('kernel')}>内核栈</button>
                            </span>
                        </Field>
                    )}
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
                    <Metric label="采集方式" value={sessionMeta.sampler} />
                    <Metric label="采样频率" value={formatRateHz(sessionMeta.sampleRateHz)} />
                    <Metric label="聚合窗口" value={formatDurationSec(sessionMeta.aggregationWindowSec)} />
                    <Metric label="上传周期" value={formatDurationSec(sessionMeta.uploadBatchSec)} />
                </div>
                <div style={S.metaLine}>
                    <span style={S.metaItem}><span style={S.metaKey}>数据保留</span>{formatHours(sessionMeta.retentionHours)}</span>
                    <span style={uploadState.warn ? { ...S.metaItem, ...S.metaItemWarn } : S.metaItem} title={uploadState.title}>
                        <span style={S.metaKey}>最近上传</span>{uploadState.label}
                    </span>
                    <span style={S.metaItem}><span style={S.metaKey}>Session</span>{shortSessionID(sessionMeta.sid)}</span>
                    <span style={S.metaItem}><span style={S.metaKey}>样本状态</span>{sampleState}</span>
                </div>
                <div style={{ ...S.info, marginTop: 14 }}>
                    {signalTab === 'cpu' ? scopeLabel : `${signalTab === 'io' ? '整机 IO 延迟' : '整机调度延迟'} / eBPF histogram`}；{sessionMeta.sampler} 以 {formatRateHz(sessionMeta.sampleRateHz)} 低频采样，当前查询窗口：{formatTime(timeWindow.from)} - {formatTime(timeWindow.to)}。
                    {signalTab === 'cpu' ? ' comm 是 Linux task comm，可能被截断到约 15 字符。' : ` 当前 backend：${histogram?.backend || signalBackend(sessionMeta, signalTab) || '-'}`}
                    {scope === 'process' && commMessage ? ` ${commMessage}` : ''}
                </div>
                <LabelChips target={target} />
            </section>

            {error && <div style={S.error}>{error}</div>}

            {signalTab === 'cpu' ? <section style={S.card}>
                <div style={S.sectionHead}>
                    <div>
                        <h3 style={S.title}>火焰图 · {stackScopeLabel}</h3>
                        <div style={S.subtle}>
                            {flamegraph?.source || 'mini-drop'} · {profileUnitLabel(flamegraph?.unit || unit)} · backend {flamegraph?.backend || topn?.backend || signalBackend(sessionMeta, 'cpu') || sessionMeta.sampler} · 宽度按 {isCPUTimeUnit(flamegraph?.unit || unit) ? 'CPU 占用时长' : '原始 value'} 计算
                        </div>
                    </div>
                    <span style={S.subtle}>{hasFlamegraph ? `${countProfileNodes(flamegraph.nodes)} 个栈帧节点` : '暂无栈帧节点'}</span>
                </div>
                <InteractiveFlamegraph
                    key={resetKey}
                    data={flamegraph}
                    loading={querying}
                    externalUrl={profileURL}
                    externalLabel="打开 Profile"
                    filterText={activeFilterText}
                    emptyMessage="所选时间范围没有 profile 数据"
                    loadingMessage="正在查询 Native profiling..."
                    boxStyle={S.flameBox}
                />
            </section> : <HistogramPanel data={histogram} loading={querying} title={signalTab === 'io' ? 'IO 延迟' : '调度延迟'} />}

            {signalTab === 'cpu' && <section style={S.card}>
                <div style={S.sectionHead}>
                    <h3 style={S.title}>热点 TopN</h3>
                    <span style={S.subtle}>{topn?.items?.length || 0} functions · {profileUnitLabel(topn?.unit || unit)}</span>
                </div>
                <TopNTable data={topn} loading={querying} profileURL={profileURL} filterText={activeFilterText} />
            </section>}

            <details style={S.details}>
                <summary style={S.detailsSummary}>诊断信息</summary>
                <pre style={S.mono}>{diagnosticText({ target, flamegraph, topn, timeWindow, profileType, stackScope, filters: activeFilters })}</pre>
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

function continuousSessionMeta(target) {
    const raw = target?.continuous_session || {};
    const caps = raw.capabilities || {};
    return {
        sid: raw.sid || '',
        name: raw.name || 'Native Continuous Profiling',
        status: raw.status || target?.profile_status || 'unknown',
        sampler: raw.sampler || caps.sampler || 'perf_event',
        sampleRateHz: numberOrDefault(raw.sample_rate_hz, 19),
        aggregationWindowSec: numberOrDefault(raw.aggregation_window_sec, 10),
        uploadBatchSec: numberOrDefault(raw.upload_batch_sec, 60),
        retentionHours: numberOrDefault(raw.retention_hours, 24),
        lastUploadAt: raw.last_upload_at || target?.last_profile_at || '',
        startedAt: raw.started_at || '',
        stoppedAt: raw.stopped_at || '',
        capabilities: caps,
    };
}

function uploadFreshness(meta) {
    const status = String(meta.status || '').toLowerCase();
    if (status !== 'running') {
        return {
            label: meta.lastUploadAt ? formatRelativeTime(meta.lastUploadAt) : '未上传',
            warn: false,
            title: meta.lastUploadAt ? formatTime(meta.lastUploadAt) : '',
        };
    }
    if (!meta.lastUploadAt) {
        return { label: '等待首次上传', warn: true, title: 'session 正在运行，但还没有登记 last_upload_at' };
    }
    const uploadedAt = new Date(meta.lastUploadAt);
    const uploadedMs = uploadedAt.getTime();
    if (Number.isNaN(uploadedMs)) {
        return { label: String(meta.lastUploadAt), warn: false, title: String(meta.lastUploadAt) };
    }
    const ageSec = Math.max(0, (Date.now() - uploadedMs) / 1000);
    const staleAfterSec = Math.max(numberOrDefault(meta.uploadBatchSec, 60) * 2, 120);
    return {
        label: formatRelativeTime(meta.lastUploadAt),
        warn: ageSec > staleAfterSec,
        title: formatTime(meta.lastUploadAt),
    };
}

function numberOrDefault(value, fallback) {
    const num = Number(value);
    return Number.isFinite(num) && num > 0 ? num : fallback;
}

function formatRateHz(value) {
    return `${numberOrDefault(value, 19)} Hz`;
}

function formatDurationSec(value) {
    return `${numberOrDefault(value, 0)} s`;
}

function formatHours(value) {
    return `${numberOrDefault(value, 24)} h`;
}

function shortSessionID(value) {
    const text = String(value || '');
    if (!text) return '-';
    return text.length > 14 ? `${text.slice(0, 8)}...${text.slice(-4)}` : text;
}

function formatRelativeTime(value) {
    if (!value) return '-';
    const date = new Date(value);
    const ms = date.getTime();
    if (Number.isNaN(ms)) return String(value);
    const diffSec = Math.max(0, Math.round((Date.now() - ms) / 1000));
    if (diffSec < 60) return `${diffSec} 秒前`;
    const diffMin = Math.round(diffSec / 60);
    if (diffMin < 60) return `${diffMin} 分钟前`;
    const diffHour = Math.round(diffMin / 60);
    if (diffHour < 24) return `${diffHour} 小时前`;
    return formatTime(value);
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

function TopNTable({ data, loading, profileURL, filterText = '' }) {
    if (loading && !data) return <div style={S.empty}>正在查询 TopN...</div>;
    const items = data?.items || [];
    if (data?.empty || items.length === 0) {
        return <ProfileEmpty message={data?.message || (filterText ? `该时间范围内 ${filterText} 无样本` : '暂无热点函数')} url={data?.profile_url || profileURL} />;
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
            {url && <a href={url} target="_blank" rel="noreferrer" style={S.btnSecondary}>打开 Profile</a>}
        </div>
    );
}

function HistogramPanel({ data, loading, title }) {
    if (loading && !data) return <section style={S.card}><div style={S.empty}>正在查询 {title} histogram...</div></section>;
    const buckets = data?.buckets || [];
    const trend = data?.trend || [];
    const maxCount = Math.max(1, ...buckets.map(b => Number(b.count) || 0));
    const summary = data?.summary || {};
    return (
        <section style={S.card}>
            <div style={S.sectionHead}>
                <div>
                    <h3 style={S.title}>{title} Histogram</h3>
                    <div style={S.subtle}>{data?.source || 'mini-drop-native'} · backend {data?.backend || '-'} · unit {data?.unit || 'us'}</div>
                </div>
                <span style={S.subtle}>{formatMetricValue(data?.event_count || 0, 'samples')} events</span>
            </div>
            {data?.empty || buckets.length === 0 ? (
                <ProfileEmpty message={data?.message || `${title} 暂无 histogram 样本`} url={data?.profile_url} />
            ) : (
                <>
                    <div style={S.summaryGrid}>
                        <Metric label="P50" value={formatLatency(summary.p50, data?.unit)} />
                        <Metric label="P95" value={formatLatency(summary.p95, data?.unit)} />
                        <Metric label="P99" value={formatLatency(summary.p99, data?.unit)} />
                        <Metric label="事件数" value={formatMetricValue(data?.event_count || 0, 'samples')} />
                    </div>
                    <div style={{ ...S.tableWrap, marginTop: 14 }}>
                        <table style={S.table}>
                            <thead>
                                <tr>
                                    <th style={{ ...S.th, width: '24%' }}>延迟桶</th>
                                    <th style={S.th}>分布</th>
                                    <th style={{ ...S.th, width: 120 }}>事件数</th>
                                </tr>
                            </thead>
                            <tbody>
                                {buckets.map((bucket, index) => (
                                    <tr key={`${bucket.range}-${index}`}>
                                        <td style={S.td}>{bucket.range}</td>
                                        <td style={S.td}>
                                            <div style={S.barTrack}>
                                                <div style={{ ...S.bar, width: `${Math.max(3, (Number(bucket.count) || 0) / maxCount * 100)}%`, background: '#12b76a' }} />
                                            </div>
                                        </td>
                                        <td style={S.td}>{formatMetricValue(bucket.count, 'samples')}</td>
                                    </tr>
                                ))}
                            </tbody>
                        </table>
                    </div>
                    <div style={{ marginTop: 16 }}>
                        <div style={S.sectionHead}>
                            <h3 style={S.title}>P95/P99 趋势</h3>
                            <span style={S.subtle}>{trend.length} 个窗口</span>
                        </div>
                        <div style={S.tableWrap}>
                            <table style={S.table}>
                                <thead>
                                    <tr>
                                        <th style={S.th}>窗口</th>
                                        <th style={S.th}>P50</th>
                                        <th style={S.th}>P95</th>
                                        <th style={S.th}>P99</th>
                                        <th style={S.th}>事件数</th>
                                    </tr>
                                </thead>
                                <tbody>
                                    {trend.slice(-20).map((point, index) => (
                                        <tr key={`${point.window_start}-${index}`}>
                                            <td style={S.td}>{formatTime(point.window_start)}</td>
                                            <td style={S.td}>{formatLatency(point.p50, data?.unit)}</td>
                                            <td style={S.td}>{formatLatency(point.p95, data?.unit)}</td>
                                            <td style={S.td}>{formatLatency(point.p99, data?.unit)}</td>
                                            <td style={S.td}>{formatMetricValue(point.event_count || 0, 'samples')}</td>
                                        </tr>
                                    ))}
                                </tbody>
                            </table>
                        </div>
                    </div>
                </>
            )}
        </section>
    );
}

function sampleStateForTarget(target, flamegraph, topn) {
    const status = String(target?.profile_status || 'unknown');
    if (status === 'online_with_samples') return '有样本';
    if (status === 'online_no_samples') return '在线但暂无样本';
    if (status === 'online') return '在线';
    if (status === 'running') return '运行中';
    if (status === 'stopped') return '已停止';
    if (status === 'no_session') return '暂无 session';
    if (status === 'query_unsupported') return '查询不兼容';
    if (flamegraph?.empty || topn?.empty) return '暂无样本';
    if (status === 'offline') return '离线';
    return '未知';
}

function signalBackend(meta, signal) {
    const caps = meta?.capabilities || {};
    if (signal === 'cpu') return caps.cpu_backend || caps.sampler || '';
    if (signal === 'io') return caps.io_backend || '';
    if (signal === 'sched') return caps.sched_backend || '';
    return '';
}

function formatLatency(value, unit = 'us') {
    const num = Number(value);
    if (!Number.isFinite(num)) return '-';
    return `${Math.round(num * 100) / 100} ${unit || 'us'}`;
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

function diagnosticText({ target, flamegraph, topn, timeWindow, profileType, stackScope = 'all', filters = {} }) {
    return [
        `target: ${target?.id || '-'}`,
        `profile_type: ${profileType}`,
        `stack_scope: ${stackScope}`,
        `continuous_session: ${JSON.stringify(target?.continuous_session || {})}`,
        `time_range: ${timeWindow.from} -> ${timeWindow.to}`,
        `selector: ${labelSelectorForTarget(target, filters)}`,
        `filters: ${JSON.stringify(filters || {})}`,
        `backend: ${flamegraph?.backend || topn?.backend || '-'}`,
        `query: ${flamegraph?.query || topn?.query || '-'}`,
        `unit: ${flamegraph?.unit || topn?.unit || '-'}`,
        `total_raw_value: ${formatRawMetric(flamegraph?.total || topn?.total || 0, flamegraph?.unit || topn?.unit || '')}`,
        `profile_url: ${flamegraph?.profile_url || topn?.profile_url || target?.profile_url || '-'}`,
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
    const preferred = ['test_c_profilin', 'python3', 'test_go_profili', 'java', 'node', 'drop_agent'];
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
