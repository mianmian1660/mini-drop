import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { continuous, profiles, sentinelRules } from '../api';
import InteractiveFlamegraph, { countProfileNodes } from './InteractiveFlamegraph';
import HistogramTrendChart from './HistogramTrendChart';
import { localDateTimeToISO } from '../utils/time';
import { SENTINEL_SIGNALS, decodeJSONField, signalLabel } from '../utils/continuous';
import {
    formatMetricValue,
    formatRawMetric,
    isCPUTimeUnit,
    metricColumnLabel,
    profileUnitLabel,
} from '../utils/profileMetrics';

const S = {
    panel: { display: 'grid', gridTemplateColumns: 'minmax(0, 1fr)', gap: 14, minWidth: 0, maxWidth: '100%' },
    card: { minWidth: 0, maxWidth: '100%', background: '#fff', borderRadius: 8, padding: 16, border: '1px solid #e5e7eb', boxShadow: '0 1px 2px rgba(16,24,40,0.04)' },
    head: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 16, flexWrap: 'wrap' },
    titleLine: { display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap' },
    title: { margin: 0, fontSize: 18, letterSpacing: 0 },
    subtitle: { margin: '5px 0 0', color: '#667085', fontSize: 13 },
    actions: { display: 'flex', gap: 8, flexWrap: 'wrap', justifyContent: 'flex-end' },
    controls: { display: 'flex', gap: 12, alignItems: 'end', flexWrap: 'wrap' },
    customRange: { display: 'grid', gridTemplateColumns: 'minmax(0, 1fr) auto', gap: 12, alignItems: 'end', marginTop: 10, paddingTop: 10, borderTop: '1px solid #eef2f6' },
    field: { minWidth: 180, flex: '1 1 180px' },
    fieldWide: { minWidth: 260, flex: '2 1 300px' },
    label: { display: 'block', color: '#475467', fontSize: 12, fontWeight: 700, marginBottom: 6 },
    select: { width: '100%', padding: '8px 10px', border: '1px solid #d0d7de', borderRadius: 6, background: '#fff', fontSize: 13, height: 36 },
    btn: { background: '#315efb', color: '#fff', border: 'none', padding: '8px 12px', borderRadius: 6, cursor: 'pointer', fontSize: 13, fontWeight: 700 },
    btnSecondary: { background: '#fff', color: '#315efb', border: '1px solid #c7d2fe', padding: '7px 11px', borderRadius: 6, cursor: 'pointer', fontSize: 13, fontWeight: 700, textDecoration: 'none' },
    segmented: { display: 'inline-flex', border: '1px solid #d0d7de', borderRadius: 6, overflow: 'hidden', height: 36, background: '#fff' },
    segment: (active) => ({ border: 'none', borderRight: '1px solid #d0d7de', background: active ? '#eef2ff' : '#fff', color: active ? '#315efb' : '#475467', padding: '0 12px', cursor: 'pointer', fontSize: 13, fontWeight: 700 }),
    textInput: { width: '100%', padding: '8px 10px', border: '1px solid #d0d7de', borderRadius: 6, background: '#fff', fontSize: 13, height: 36, boxSizing: 'border-box' },
    searchInput: { width: 210, maxWidth: '100%', padding: '7px 10px', border: '1px solid #d0d7de', borderRadius: 6, background: '#fff', fontSize: 13, height: 34, boxSizing: 'border-box' },
    flameActions: { display: 'flex', alignItems: 'center', justifyContent: 'flex-end', gap: 10, flexWrap: 'wrap' },
    stateBadge: { display: 'inline-flex', alignItems: 'center', border: '1px solid #abefc6', background: '#ecfdf3', color: '#067647', borderRadius: 999, padding: '3px 8px', fontSize: 12, fontWeight: 700 },
    summaryGrid: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(min(170px, 100%), 1fr))', gap: 0, minWidth: 0, maxWidth: '100%', borderTop: '1px solid #eef2f6', borderBottom: '1px solid #eef2f6' },
    metric: { padding: '10px 14px 10px 0', minWidth: 0 },
    metricLabel: { color: '#667085', fontSize: 12, marginBottom: 4 },
    metricValue: { color: '#111827', fontSize: 16, fontWeight: 700, wordBreak: 'break-word', lineHeight: 1.35 },
    metaLine: { display: 'flex', flexWrap: 'wrap', gap: 10, alignItems: 'center', marginTop: 12 },
    metaItem: { display: 'inline-flex', alignItems: 'center', gap: 6, border: '1px solid #eaecf0', background: '#fff', color: '#344054', borderRadius: 999, padding: '5px 9px', fontSize: 12, fontWeight: 700 },
    metaItemWarn: { borderColor: '#fda29b', background: '#fff6f5', color: '#b42318' },
    metaKey: { color: '#667085', fontWeight: 700 },
    compactDetails: { marginTop: 12, borderTop: '1px solid #eef2f6', paddingTop: 10 },
    chipWrap: { display: 'flex', flexWrap: 'wrap', gap: 8 },
    chip: { display: 'inline-flex', alignItems: 'center', gap: 6, border: '1px solid #eaecf0', background: '#fff', color: '#344054', borderRadius: 999, padding: '4px 8px', fontSize: 12, fontWeight: 700 },
    chipKey: { color: '#667085', fontWeight: 700 },
    sectionHead: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 12, marginBottom: 12, flexWrap: 'wrap' },
    subtle: { color: '#667085', fontSize: 12 },
    inlineNote: { color: '#667085', fontSize: 12, lineHeight: 1.5 },
    flameBox: { width: '100%', maxWidth: '100%', minWidth: 0, height: 560, overflowX: 'auto', overflowY: 'auto', border: '1px solid #eaecf0', borderRadius: 8, background: '#fff', padding: 6 },
    empty: { textAlign: 'center', padding: 44, color: '#667085', background: '#fff', border: '1px dashed #d0d7de', borderRadius: 8 },
    error: { background: '#fff3f3', border: '1px solid #ffcdd2', color: '#b42318', borderRadius: 8, padding: 12 },
    warn: { color: '#b42318', background: '#fff6f5', border: '1px solid #fda29b', borderRadius: 6, padding: '9px 12px', fontSize: 13 },
    info: { color: '#475467', background: '#fff', border: '1px solid #eaecf0', borderRadius: 6, padding: '8px 10px', fontSize: 12, lineHeight: 1.55 },
    coverage: { marginTop: 14, borderTop: '1px solid #eaecf0', paddingTop: 12 },
    coverageWrap: { position: 'relative' },
    coverageBar: { display: 'flex', width: '100%', height: 14, overflow: 'hidden', borderRadius: 4, background: '#f2f4f7', border: '1px solid #d0d5dd' },
    coverageOK: { height: '100%', background: '#12b76a', cursor: 'default' },
    coverageGap: { height: '100%', background: '#d92d20', minWidth: 2, cursor: 'default' },
    coverageLegend: { display: 'flex', flexWrap: 'wrap', gap: 10, marginTop: 8, color: '#667085', fontSize: 12 },
    coverageLegendItem: { display: 'inline-flex', alignItems: 'center', gap: 5 },
    coverageDot: { width: 8, height: 8, borderRadius: 999, display: 'inline-block' },
    coverageSummary: { marginTop: 8, color: '#475467', fontSize: 12, lineHeight: 1.5 },
    coverageSummaryLabel: { color: '#111827', marginRight: 6 },
    coverageTooltip: { position: 'absolute', top: 22, zIndex: 5, minWidth: 220, maxWidth: 320, padding: 10, color: '#344054', background: '#fff', border: '1px solid #d0d5dd', borderRadius: 6, boxShadow: '0 8px 20px rgba(16,24,40,.12)', fontSize: 12, lineHeight: 1.45, pointerEvents: 'none' },
    coverageTooltipTitle: { color: '#111827', fontWeight: 700, marginBottom: 5 },
    coverageAlert: { marginTop: 12, borderRadius: 6, border: '1px solid #fedf89', background: '#fffaeb', padding: '10px 12px', color: '#92400e', fontSize: 13, lineHeight: 1.5 },
    coverageAlertWarn: { borderColor: '#fecd6f', background: '#fff7e6' },
    coverageAlertTitle: { fontWeight: 700, marginBottom: 4 },
    gapList: { display: 'grid', gap: 5, marginTop: 8, color: '#b42318', fontSize: 12 },
    gapToggle: { background: 'none', border: 'none', color: '#315efb', cursor: 'pointer', fontSize: 12, padding: 0, textAlign: 'left' },
    timeSlider: { minWidth: 0, display: 'grid', gap: 10 },
    sliderFrame: { border: '1px solid #eaecf0', borderRadius: 8, background: '#fbfcfe', padding: 12 },
    sliderTrack: { position: 'relative', height: 34, margin: '10px 8px 4px', touchAction: 'none' },
    sliderBase: { position: 'absolute', left: 0, right: 0, top: 15, height: 4, borderRadius: 999, background: '#d0d5dd' },
    sliderSelection: { position: 'absolute', top: 12, height: 10, borderRadius: 999, background: '#315efb', cursor: 'grab' },
    sliderWindowA: { position: 'absolute', top: 12, height: 10, borderRadius: '999px 0 0 999px', background: '#667085', cursor: 'grab' },
    sliderWindowB: { position: 'absolute', top: 12, height: 10, borderRadius: '0 999px 999px 0', background: '#315efb', cursor: 'grab' },
    sliderSplit: { position: 'absolute', top: 9, width: 2, height: 16, background: '#fff', borderRadius: 2, transform: 'translateX(-1px)', zIndex: 2 },
    sliderThumb: { position: 'absolute', top: 5, width: 22, height: 22, borderRadius: '50%', background: '#fff', border: '2px solid #315efb', boxShadow: '0 1px 4px rgba(16,24,40,.18)', transform: 'translateX(-50%)', cursor: 'ew-resize', zIndex: 3 },
    sliderThumbMuted: { borderColor: '#667085' },
    sliderMeta: { display: 'flex', justifyContent: 'space-between', gap: 10, flexWrap: 'wrap', color: '#667085', fontSize: 12 },
    sliderInputs: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit,minmax(min(210px,100%),1fr))', gap: 10, marginTop: 10 },
    diffWindows: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit,minmax(min(260px,100%),1fr))', gap: 10, marginTop: 10 },
    tableWrap: { width: '100%', minWidth: 0, maxWidth: '100%', overflowX: 'auto', overflowY: 'hidden' },
    table: { width: '100%', borderCollapse: 'collapse' },
    th: { textAlign: 'left', padding: '9px 10px', borderBottom: '1px solid #eaecf0', color: '#475467', fontSize: 12, background: '#fff' },
    td: { padding: '8px 10px', borderBottom: '1px solid #f2f4f7', fontSize: 13, verticalAlign: 'top', lineHeight: 1.45 },
    tdMuted: { color: '#667085', fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace' },
    tdNowrap: { whiteSpace: 'nowrap' },
    histogramBucketCell: { minWidth: 150, whiteSpace: 'nowrap', fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace', color: '#344054' },
    histogramBarCell: { minWidth: 220 },
    histogramCountCell: { width: 140, whiteSpace: 'nowrap', color: '#344054', fontWeight: 700 },
    barWithLabel: { display: 'grid', gridTemplateColumns: 'minmax(120px, 1fr) 56px', gap: 10, alignItems: 'center', minWidth: 0 },
    barTrack: { height: 10, minWidth: 120, borderRadius: 999, background: '#f2f4f7', border: '1px solid #eaecf0', overflow: 'hidden' },
    bar: { height: '100%', borderRadius: 999, minWidth: 2 },
    barPercent: { color: '#667085', fontSize: 12, textAlign: 'right', whiteSpace: 'nowrap', fontVariantNumeric: 'tabular-nums' },
    details: { width: '100%', minWidth: 0, maxWidth: '100%', borderTop: '1px solid #eaecf0', padding: '10px 0 0', background: '#fff' },
    detailsSummary: { cursor: 'pointer', color: '#475467', fontSize: 13, fontWeight: 700 },
    diagnosticToolbar: { display: 'flex', justifyContent: 'space-between', gap: 10, alignItems: 'center', margin: '10px 0 8px' },
    diagnosticCopy: { background: '#fff', color: '#315efb', border: '1px solid #c7d2fe', borderRadius: 6, padding: '6px 10px', cursor: 'pointer', fontSize: 12, fontWeight: 700 },
    mono: { width: '100%', minWidth: 0, maxWidth: '100%', margin: '10px 0 0', padding: 10, background: '#111827', color: '#e5e7eb', borderRadius: 6, overflowX: 'auto', whiteSpace: 'pre-wrap', overflowWrap: 'anywhere', wordBreak: 'break-word', fontSize: 12, lineHeight: 1.5 },
};

const SIGNAL_TAB_OPTIONS = [
    { tab: 'cpu', signal: 'cpu_profile', label: 'CPU' },
    { tab: 'io', signal: 'io_latency', label: '块 IO' },
    { tab: 'io_syscall', signal: 'io_syscall_latency', label: '系统调用 IO' },
    { tab: 'sched', signal: 'sched_latency', label: '调度延迟' },
    // db_snapshot 不是 ContinuousSamplerConfig.signals 里的常规信号（那四个是
    // perf/eBPF 的固定采集集合），而是由 session.labels.db_targets 是否配置
    // 决定——见 continuousSessionMeta，配了 db_targets 才会把 'db_snapshot'
    // 塞进这里复用的 signals 数组，不是服务端下发的。
    { tab: 'db', signal: 'db_snapshot', label: '数据库' },
];

const RANGE_OPTIONS = [
    ['15m', '最近 15 分钟', 15],
    ['30m', '最近 30 分钟', 30],
    ['1h', '最近 1 小时', 60],
    ['6h', '最近 6 小时', 360],
    ['12h', '最近 12 小时', 720],
    ['24h', '最近 24 小时', 1440],
];

export default function ContinuousProfilingPanel({ target, targets = [], targetId = '', onTargetChange, showTargetSelect = false, fixedSession = null, initialQuery = null }) {
    const initialWindow = initialQuery?.from && initialQuery?.to
        ? { from: initialQuery.from, to: initialQuery.to }
        : null;
    const initialFilters = initialQuery?.filters || {};
    const [range, setRange] = useState(() => initialWindow ? 'custom' : '30m');
    const [timeWindow, setTimeWindow] = useState(() => initialWindow || makeTimeWindow('30m'));
    const [customFrom, setCustomFrom] = useState(() => initialWindow ? toLocalDateTimeInput(initialWindow.from) : '');
    const [customTo, setCustomTo] = useState(() => initialWindow ? toLocalDateTimeInput(initialWindow.to) : '');
    const [customAnchorNow, setCustomAnchorNow] = useState(() => new Date().toISOString());
    const [appliedCustomWindow, setAppliedCustomWindow] = useState(null);
    // Memory profiling is not currently supported by continuous collection.
    // Keep legacy links/query state on the supported CPU view.
    const [profileType, setProfileType] = useState('cpu');
    const [signalTab, setSignalTab] = useState('cpu');
    const [stackScope, setStackScope] = useState(() => initialQuery?.stackScope || 'all');
    const [flamegraph, setFlamegraph] = useState(null);
    const [topn, setTopn] = useState(null);
    const [histogram, setHistogram] = useState(null);
    const [dbSnapshot, setDbSnapshot] = useState(null);
    const [querying, setQuerying] = useState(false);
    const [error, setError] = useState('');
    const [reliability, setReliability] = useState(null);
    const [resetKey, setResetKey] = useState(0);
    const [scope, setScope] = useState('host');
    const [selectedComm, setSelectedComm] = useState(() => String(initialFilters.comm || ''));
	const [selectedInstance, setSelectedInstance] = useState('');
	const [historicalInstanceValues, setHistoricalInstanceValues] = useState([]);
    const [commValues, setCommValues] = useState([]);
    const [commAvailable, setCommAvailable] = useState(false);
    const [commMessage, setCommMessage] = useState('');
    const [commLoading, setCommLoading] = useState(false);
    const [selectedRuntime, setSelectedRuntime] = useState(() => String(initialFilters.runtime || ''));
    const [runtimeValues, setRuntimeValues] = useState([]);
    const [rssSeries, setRssSeries] = useState([]);
    const [rssMeta, setRssMeta] = useState(null);
    const [memoryProfiles, setMemoryProfiles] = useState([]);
    const [memoryProfilesLoading, setMemoryProfilesLoading] = useState(false);
    const [maxNodes, setMaxNodes] = useState(5000);
    const [flameSearchInput, setFlameSearchInput] = useState('');
    const [flameSearchText, setFlameSearchText] = useState('');
    const [renderStats, setRenderStats] = useState({ rendered: 0, total: 0, mode: 'full' });
    const [searchStats, setSearchStats] = useState({ matches: 0, samplePercent: 0 });
    // diff selector state (baseline vs compare)
    const [diffOpen, setDiffOpen] = useState(false);
    const [diffRange, setDiffRange] = useState('15m');
    const [diffBaseFrom, setDiffBaseFrom] = useState('');
    const [diffBaseTo, setDiffBaseTo] = useState('');
    const [diffCompareFrom, setDiffCompareFrom] = useState('');
    const [diffCompareTo, setDiffCompareTo] = useState('');
    const [appliedDiffCustomWindows, setAppliedDiffCustomWindows] = useState(null);
    const [diffResult, setDiffResult] = useState(null);
    const [diffLoading, setDiffLoading] = useState(false);
    const [diffError, setDiffError] = useState('');
    // diffViewMode 控制"表格 / 火焰图"两种 diff 展现形式，和上面的
    // diffMode('quick'/'custom'，控制 baseline/compare 时间窗怎么选)是两个
    // 不同维度的开关，不要搞混。表格走 diffResult，火焰图走独立的
    // diffFlamegraphResult——两条查询互不影响，切换 tab 不会清掉另一个
    // tab 已经查出来的结果。
    const [diffViewMode, setDiffViewMode] = useState('table');
    const [diffFlamegraphResult, setDiffFlamegraphResult] = useState(null);
    const [diffFlamegraphLoading, setDiffFlamegraphLoading] = useState(false);
    const [diffFlamegraphError, setDiffFlamegraphError] = useState('');
    // Memory tab: recent Go pprof heap tasks
    const [heapTasks, setHeapTasks] = useState([]);
    const [heapTasksLoading, setHeapTasksLoading] = useState(false);
    const [symbolCheck, setSymbolCheck] = useState(null);
    const [symbolChecking, setSymbolChecking] = useState(false);
    const [symbolCheckError, setSymbolCheckError] = useState('');
    const querySequence = useRef(0);
    const initializedSessionWindow = useRef('');
    const targetKey = target?.id || '';
    const targetHost = target?.ip || '';
    const targetService = target?.service_name || 'hotmethod';
    const targetTitle = target?.hostname || target?.ip || '';
    const sessionTimeAnchor = useMemo(() => {
        const parsed = fixedSession?.stopped_at ? new Date(fixedSession.stopped_at) : new Date();
        return Number.isNaN(parsed.getTime()) ? new Date() : parsed;
    }, [fixedSession?.stopped_at]);
    const targetProfileURL = target?.profile_url || '';
    const profileURL = flamegraph?.profile_url || topn?.profile_url || targetProfileURL;
    const hasFlamegraph = flamegraph && !flamegraph.empty && Array.isArray(flamegraph.nodes) && flamegraph.nodes.length > 0;
	const sampleState = sampleStateForTarget(target, flamegraph, topn, fixedSession);
	const sessionMeta = continuousSessionMeta(target, fixedSession);
    const sessionSID = sessionMeta.sid;
    const signalKey = (sessionMeta.signals || []).join('|');
    const availableSignalTabs = useMemo(() => signalTabsForSession(signalKey), [signalKey]);
	const taskScope = fixedSession?.scope === 'process' ? 'process' : 'host';
	const activeProcesses = useMemo(() => decodeJSONField(fixedSession?.active_processes, []), [fixedSession?.active_processes]);
	const processInstances = useMemo(
		() => sampledProcessInstanceOptions(activeProcesses, historicalInstanceValues),
		[activeProcesses, historicalInstanceValues],
	);
    const activeProcessCount = activeProcesses.length;
    const rangeOptions = useMemo(() => rangeOptionsForRetention(sessionMeta.retentionHours), [sessionMeta.retentionHours]);
    const diffRangeOptions = useMemo(() => rangeOptionsForRetention(sessionMeta.retentionHours, true), [sessionMeta.retentionHours]);
    const uploadState = uploadFreshness(sessionMeta);
    const unit = flamegraph?.unit || topn?.unit || '';
    const profileData = flamegraph || topn;
    const sampleNotice = lowSampleGuidance(profileData, sessionMeta, querying);
    const symbolStatus = flamegraph?.symbol_status || topn?.symbol_status || '';
    const symbolNeedsCheck = symbolStatus === 'partial' || symbolStatus === 'missing';
	const activeFilters = useMemo(() => ({
		...(taskScope === 'process' && fixedSession?.selector_exe ? { exe: fixedSession.selector_exe } : {}),
		...(taskScope === 'process' && selectedInstance ? instanceFilters(selectedInstance) : {}),
		...(taskScope === 'host' && scope === 'process' && selectedComm.trim() ? { comm: selectedComm.trim() } : {}),
        ...(selectedRuntime ? { runtime: selectedRuntime } : {}),
	}), [taskScope, fixedSession?.selector_exe, selectedInstance, scope, selectedComm, selectedRuntime]);
    const activeFilterText = Object.entries(activeFilters).map(([key, value]) => `${key}=${value}`).join(', ');
    const stackScopeLabel = stackScope === 'user' ? '用户栈' : stackScope === 'kernel' ? '内核栈' : '混合栈';
    const scopeLabel = taskScope === 'process'
		? `进程持续采集 / ${fixedSession?.selector_exe || '-'} / ${selectedInstance ? '单实例' : '全部实例'} / ${stackScopeLabel}`
		: activeFilters.comm ? `整机任务查询过滤 / ${activeFilterText} / ${stackScopeLabel}` : `整机持续采集 / ${stackScopeLabel}`;
    const activeFiltersKey = useMemo(() => JSON.stringify(activeFilters), [activeFilters]);

    // 已停止的历史 session 默认查看其自身的最后保留窗口，而不是“最近 30 分钟”。
    // 否则任务详情打开在很久以后时，查询时间段已经落在 session 结束之后，历史样本会看起来像丢失。
    useEffect(() => {
        const stoppedAt = fixedSession?.stopped_at;
        if (!sessionSID || !stoppedAt || initializedSessionWindow.current === sessionSID || initialWindow) return;
        const end = new Date(stoppedAt);
        if (Number.isNaN(end.getTime())) return;
        const retentionHours = Math.max(1, numberOrDefault(fixedSession?.retention_hours, 24));
        const started = fixedSession?.started_at ? new Date(fixedSession.started_at) : null;
        const retentionStart = new Date(end.getTime() - retentionHours * 60 * 60 * 1000);
        const start = started && !Number.isNaN(started.getTime()) && started > retentionStart ? started : retentionStart;
        if (!(start < end)) return;
        const historicalWindow = { from: start.toISOString(), to: end.toISOString() };
        initializedSessionWindow.current = sessionSID;
        setRange('custom');
        setTimeWindow(historicalWindow);
        setCustomFrom(toLocalDateTimeInput(historicalWindow.from));
        setCustomTo(toLocalDateTimeInput(historicalWindow.to));
    }, [fixedSession?.retention_hours, fixedSession?.started_at, fixedSession?.stopped_at, initialWindow, sessionSID]);
    // 阶段九：当前选中信号（v1 signal_type），timeline 按信号独立计算覆盖率。
    const currentSignal = SIGNAL_TAB_OPTIONS.find(option => option.tab === signalTab)?.signal;
    const coverageAlert = useMemo(() => coverageAlertForReliability(reliability, currentSignal), [reliability, currentSignal]);

    useEffect(() => {
        const timer = setTimeout(() => setFlameSearchText(flameSearchInput.trim()), 250);
        return () => clearTimeout(timer);
    }, [flameSearchInput]);

    useEffect(() => {
        if (!availableSignalTabs.some(option => option.tab === signalTab)) {
            setSignalTab('cpu');
        }
    }, [availableSignalTabs, signalTab]);

    useEffect(() => {
        if (range !== 'custom' && !rangeOptions.some(([value]) => value === range)) {
            const fallback = rangeOptions.some(([value]) => value === '30m') ? '30m' : rangeOptions[0]?.[0] || '15m';
            setRange(fallback);
            setTimeWindow(makeTimeWindow(fallback, sessionTimeAnchor));
        }
        if (!diffRangeOptions.some(([value]) => value === diffRange)) {
            setDiffRange(diffRangeOptions[0]?.[0] || '15m');
        }
    }, [diffRange, diffRangeOptions, range, rangeOptions, sessionTimeAnchor.getTime()]);

    const queryProfiles = useCallback(async (queryWindow) => {
        if (!targetKey || !targetHost) return;
        const requestID = ++querySequence.current;
        setQuerying(true);
        setError('');
        setReliability(null);
        setSymbolCheck(null);
        setSymbolCheckError('');
        if (signalTab === 'cpu') {
            setFlamegraph(null);
            setTopn(null);
            setHistogram(null);
            setRssSeries([]);
            setRssMeta(null);
        } else {
            setFlamegraph(null);
            setTopn(null);
            setHistogram(null);
            setRssSeries([]);
            setRssMeta(null);
        }
        const parsedFilters = activeFiltersKey ? JSON.parse(activeFiltersKey) : {};
		const params = {
			session_sid: sessionSID,
            target_id: targetKey,
            host: targetHost,
            service: targetService,
            from: queryWindow.from,
            to: queryWindow.to,
            profile_type: profileType,
            max_nodes: maxNodes,
        };
			if (signalTab === 'cpu' && Object.keys(parsedFilters).length > 0) {
                params.filters = activeFiltersKey;
            }
        try {
            const timelinePromise = sessionSID
                ? continuous.timeline(sessionSID, { from: queryWindow.from, to: queryWindow.to, signal: currentSignal }).catch(() => null)
                : Promise.resolve(null);
            timelinePromise.then(timelineRes => {
                if (requestID === querySequence.current) {
                    setReliability(timelineRes?.code === 0 ? timelineRes.data : null);
                }
            });
            if (signalTab === 'cpu') {
                const cpuParams = { ...params };
                if (stackScope !== 'all') cpuParams.stack_scope = stackScope;
                const requests = [
                    profiles.flamegraph(cpuParams),
                    profiles.topn(cpuParams),
                ];
                if (profileType === 'memory') requests.push(profiles.timeseries({ ...cpuParams, metric: 'rss_bytes' }));
                const profileResults = await Promise.all(requests);
                if (requestID !== querySequence.current) return;
                const [fgRes, topRes, rssRes] = profileResults;
                if (fgRes.code === 0) setFlamegraph(fgRes.data);
                if (topRes.code === 0) setTopn(topRes.data);
                if (profileType === 'memory') {
                    setRssSeries(rssRes?.code === 0 ? (rssRes.data?.series || []) : []);
                    setRssMeta(rssRes?.code === 0 ? rssRes.data : null);
                }
                if (fgRes.code !== 0 || topRes.code !== 0) {
                    setError(fgRes.message || topRes.message || 'Native Continuous Profiling 查询失败');
                }
            } else if (signalTab === 'db') {
                setFlamegraph(null);
                setTopn(null);
                setHistogram(null);
                const [dbRes, timelineRes] = await Promise.all([
                    continuous.dbSnapshot(params),
                    timelinePromise,
                ]);
                if (requestID !== querySequence.current) return;
                setReliability(timelineRes?.code === 0 ? timelineRes.data : null);
                if (dbRes.code === 0) setDbSnapshot(dbRes.data);
                if (dbRes.code !== 0) {
                    setError(dbRes.message || '数据库快照查询失败');
                }
            } else {
				const signalType = signalTab === 'io' ? 'io_latency' : signalTab === 'io_syscall' ? 'io_syscall_latency' : 'sched_latency';
                const histRes = await continuous.histogram({ ...params, signal_type: signalType });
                if (requestID !== querySequence.current) return;
                if (histRes.code === 0) setHistogram(histRes.data);
                if (histRes.code !== 0) {
                    setError(histRes.message || 'Native Continuous eBPF histogram 查询失败');
                }
            }
        } catch (err) {
            if (requestID !== querySequence.current) return;
            setFlamegraph(null);
            setTopn(null);
            setHistogram(null);
            setDbSnapshot(null);
            setRssSeries([]);
            setRssMeta(null);
            setReliability(null);
            setError(err?.message || 'Native Continuous Profiling 查询失败');
        } finally {
            if (requestID === querySequence.current) setQuerying(false);
        }
    }, [targetKey, targetHost, targetService, profileType, activeFiltersKey, signalTab, stackScope, maxNodes, sessionSID]);

    const checkSymbols = useCallback(async () => {
        if (!sessionSID || symbolChecking) return;
        setSymbolChecking(true);
        setSymbolCheckError('');
        try {
            const response = await continuous.symbolCheck(sessionSID, { from: timeWindow.from, to: timeWindow.to });
            if (response.code !== 0) throw new Error(response.message || '符号检查失败');
            setSymbolCheck(response.data?.symbol_check || null);
        } catch (err) {
            setSymbolCheckError(err?.response?.data?.message || err?.message || '符号检查失败');
        } finally {
            setSymbolChecking(false);
        }
    }, [sessionSID, symbolChecking, timeWindow.from, timeWindow.to]);

    useEffect(() => {
        queryProfiles(timeWindow);
    }, [queryProfiles, timeWindow]);

    const refresh = useCallback(() => {
        if (range === 'custom') {
            queryProfiles(timeWindow);
            return;
        }
        setTimeWindow(makeTimeWindow(range, sessionTimeAnchor));
    }, [queryProfiles, range, sessionTimeAnchor, timeWindow]);

    const changeRange = useCallback((nextRange) => {
        setRange(nextRange);
        setError('');
        if (nextRange === 'custom') {
            const source = appliedCustomWindow || timeWindow;
            setCustomAnchorNow(new Date().toISOString());
            setCustomFrom(toLocalDateTimeInput(source.from));
            setCustomTo(toLocalDateTimeInput(source.to));
            return;
        }
        setTimeWindow(makeTimeWindow(nextRange, sessionTimeAnchor));
    }, [appliedCustomWindow, sessionTimeAnchor.getTime(), timeWindow]);

    const applyCustomRange = useCallback(() => {
        const result = validateCustomTimeWindow(customFrom, customTo, sessionMeta.retentionHours, '', sessionTimeAnchor);
        if (result.error) {
            setError(result.error);
            return;
        }
        setError('');
        setRange('custom');
        setAppliedCustomWindow(result.window);
        setTimeWindow(result.window);
    }, [customFrom, customTo, sessionMeta.retentionHours, sessionTimeAnchor.getTime()]);

    // Load recent Go pprof heap tasks for the Memory tab link.
    const loadHeapTasks = useCallback(async () => {
        if (!targetHost) return;
        setHeapTasksLoading(true);
        try {
            const res = await profiles.heapTasks({ host: targetHost, from: timeWindow.from, to: timeWindow.to, limit: 5 });
            if (res.code === 0) {
                setHeapTasks(res.data?.tasks || res.data || []);
            }
        } catch (e) {
            // Silent: Memory tab is best-effort.
        } finally {
            setHeapTasksLoading(false);
        }
    }, [targetHost, timeWindow.from, timeWindow.to]);

    // 阶段七：Memray allocation profile 元数据（SDK 接入状态/时间范围/错误）。
    const loadMemoryProfiles = useCallback(async (queryWindow) => {
        if (!targetKey || !targetHost) return;
        setMemoryProfilesLoading(true);
        try {
            const res = await profiles.memoryProfiles({
                target_id: targetKey,
                host: targetHost,
                service: targetService,
                session_sid: sessionSID,
                profile_type: 'memory',
				filters: activeFiltersKey,
                from: queryWindow.from,
                to: queryWindow.to,
            });
            if (res.code === 0) {
                setMemoryProfiles(res.data?.profiles || []);
            } else {
                setMemoryProfiles([]);
            }
        } catch (e) {
            // Silent: Memory tab is best-effort.
            setMemoryProfiles([]);
        } finally {
            setMemoryProfilesLoading(false);
        }
    }, [targetKey, targetHost, targetService, sessionSID, activeFiltersKey]);

    useEffect(() => {
        if (profileType === 'memory') loadHeapTasks();
    }, [profileType, loadHeapTasks]);

    useEffect(() => {
        if (profileType === 'memory') loadMemoryProfiles(timeWindow);
    }, [profileType, loadMemoryProfiles, timeWindow]);

    // buildDiffParams 是表格 diff 和火焰图 diff 共用的部分——算 baseline/compare
    // 时间窗、拼 target/filters 这些查询参数，两条路径只在要不要加
    // format=flamegraph 上分叉，不要各写一份容易漂移。
    const buildDiffParams = useCallback(() => {
        if (!targetKey || !targetHost) return { error: '缺少观测对象' };
        const baseResult = validateCustomTimeWindow(diffBaseFrom, diffBaseTo, sessionMeta.retentionHours, 'Baseline');
        const compareResult = validateCustomTimeWindow(diffCompareFrom, diffCompareTo, sessionMeta.retentionHours, 'Compare');
        if (baseResult.error || compareResult.error) {
            return { error: baseResult.error || compareResult.error };
        }
        const baseWindow = baseResult.window;
        const compareWindow = compareResult.window;
        const durationDelta = Math.abs(
            (new Date(baseWindow.to).getTime() - new Date(baseWindow.from).getTime())
            - (new Date(compareWindow.to).getTime() - new Date(compareWindow.from).getTime()),
        );
        if (durationDelta >= 1000) {
            return { error: 'Baseline 与 Compare 必须使用等长时间窗' };
        }
        setAppliedDiffCustomWindows({ baseWindow, compareWindow });
        const parsedFilters = activeFiltersKey ? JSON.parse(activeFiltersKey) : {};
        const params = {
            session_sid: sessionSID,
            target_id: targetKey,
            host: targetHost,
            service: targetService,
            profile_type: 'cpu',
            base_from: baseWindow.from,
            base_to: baseWindow.to,
            compare_from: compareWindow.from,
            compare_to: compareWindow.to,
            max_nodes: maxNodes,
        };
        if (stackScope !== 'all') params.stack_scope = stackScope;
        if (Object.keys(parsedFilters).length > 0) params.filters = activeFiltersKey;
        return { params };
    }, [targetKey, targetHost, targetService, sessionSID, diffBaseFrom, diffBaseTo, diffCompareFrom, diffCompareTo, sessionMeta.retentionHours, maxNodes, stackScope, activeFiltersKey]);

    const runDiff = useCallback(async () => {
        if (!targetKey) return;
        setDiffLoading(true);
        setDiffError('');
        setDiffResult(null);
        try {
            const { params, error } = buildDiffParams();
            if (error) {
                setDiffError(error);
                return;
            }
            const res = await profiles.diff(params);
            if (res.code === 0) {
                setDiffResult(res.data);
            } else {
                setDiffError(res.message || 'Diff 查询失败');
            }
        } catch (e) {
            setDiffError(e?.message || 'Diff 查询失败');
        } finally {
            setDiffLoading(false);
        }
    }, [targetKey, buildDiffParams]);

    const runDiffFlamegraph = useCallback(async () => {
        if (!targetKey) return;
        setDiffFlamegraphLoading(true);
        setDiffFlamegraphError('');
        setDiffFlamegraphResult(null);
        try {
            const { params, error } = buildDiffParams();
            if (error) {
                setDiffFlamegraphError(error);
                return;
            }
            const res = await profiles.diff({ ...params, format: 'flamegraph' });
            if (res.code === 0) {
                setDiffFlamegraphResult(res.data);
            } else {
                setDiffFlamegraphError(res.message || 'Diff 火焰图查询失败');
            }
        } catch (e) {
            setDiffFlamegraphError(e?.message || 'Diff 火焰图查询失败');
        } finally {
            setDiffFlamegraphLoading(false);
        }
    }, [targetKey, buildDiffParams]);

    const runActiveDiffView = useCallback(() => {
        if (diffViewMode === 'flamegraph') return runDiffFlamegraph();
        return runDiff();
    }, [diffViewMode, runDiff, runDiffFlamegraph]);

    // diffFlamegraphNodes 把 ProfileDiffFlamegraph.root.children 铺成
    // InteractiveFlamegraph 期待的 nodes 数组，同时把每个节点的 value 补成
    // max(base_value, compare_value)——差分火焰图的宽度用两边较大值，
    // 保证"消失的函数"(compare_value=0)也还是按它原来的权重占位可见，
    // 不会因为宽度塌成 0 而在图上完全消失、看不出"这里少了一块"。
    const diffFlamegraphNodes = useMemo(() => {
        if (!diffFlamegraphResult || diffFlamegraphResult.empty || !diffFlamegraphResult.root) return null;
        const convert = (node) => ({
            name: node.name,
            value: Math.max(Number(node.base_value) || 0, Number(node.compare_value) || 0),
            base_value: node.base_value,
            compare_value: node.compare_value,
            delta: node.delta,
            delta_percent: node.delta_percent,
            children: Array.isArray(node.children) ? node.children.map(convert) : [],
        });
        return (diffFlamegraphResult.root.children || []).map(convert);
    }, [diffFlamegraphResult]);

    const diffFlamegraphData = useMemo(() => {
        if (!diffFlamegraphResult) return null;
        return {
            nodes: diffFlamegraphNodes || [],
            total: Math.max(Number(diffFlamegraphResult.base_total) || 0, Number(diffFlamegraphResult.compare_total) || 0),
            unit: diffFlamegraphResult.unit,
            empty: diffFlamegraphResult.empty || !diffFlamegraphNodes || diffFlamegraphNodes.length === 0,
            message: diffFlamegraphResult.message,
        };
    }, [diffFlamegraphResult, diffFlamegraphNodes]);

    const applyDiffWindows = useCallback(({ baseWindow, compareWindow }) => {
        setDiffError('');
        setDiffBaseFrom(toLocalDateTimeInput(baseWindow.from));
        setDiffBaseTo(toLocalDateTimeInput(baseWindow.to));
        setDiffCompareFrom(toLocalDateTimeInput(compareWindow.from));
        setDiffCompareTo(toLocalDateTimeInput(compareWindow.to));
    }, []);

    const changeDiffManualInput = useCallback((field, value) => {
        setDiffError('');
        if (field === 'baseFrom') setDiffBaseFrom(value);
        if (field === 'baseTo') setDiffBaseTo(value);
        if (field === 'compareFrom') setDiffCompareFrom(value);
        if (field === 'compareTo') setDiffCompareTo(value);
        setAppliedDiffCustomWindows(null);
    }, []);

    useEffect(() => {
        const windows = appliedDiffCustomWindows || makeSequentialDiffWindows(diffRange);
        applyDiffWindows(windows);
    }, [diffRange, applyDiffWindows, appliedDiffCustomWindows]);

    useEffect(() => {
        setSelectedComm('');
		setSelectedRuntime('');
		setSelectedInstance('');
		setScope(taskScope === 'process' ? 'process' : 'host');
	}, [targetKey, sessionSID, taskScope]);

    useEffect(() => {
        if (!targetKey || !targetHost) {
            setCommValues([]);
            setCommAvailable(false);
            setCommMessage('');
            return undefined;
        }
        let cancelled = false;
        setCommLoading(true);
        setCommMessage('');
		profiles.labelValues({
			session_sid: sessionSID,
            target_id: targetKey,
            host: targetHost,
            service: targetService,
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
	}, [targetKey, targetHost, targetService, sessionSID, timeWindow, profileType, selectedComm]);

	useEffect(() => {
		if (!targetKey || !targetHost) return undefined;
        let cancelled = false;
		profiles.labelValues({ session_sid: sessionSID, target_id: targetKey, host: targetHost, service: targetService, from: timeWindow.from, to: timeWindow.to, profile_type: profileType, label: 'runtime' })
            .then(res => { if (!cancelled && res.code === 0) setRuntimeValues(res.data?.values || []); })
            .catch(() => { if (!cancelled) setRuntimeValues([]); });
        return () => { cancelled = true; };
	}, [targetKey, targetHost, targetService, sessionSID, timeWindow, profileType]);

	useEffect(() => {
		if (!targetKey || !targetHost || taskScope !== 'process' || !sessionSID || profileType !== 'cpu') {
			setHistoricalInstanceValues([]);
			return undefined;
		}
		let cancelled = false;
		profiles.labelValues({
			session_sid: sessionSID,
			target_id: targetKey,
			host: targetHost,
			service: targetService,
			from: timeWindow.from,
			to: timeWindow.to,
			profile_type: 'cpu',
			label: 'process_instance',
		}).then(res => {
			if (!cancelled && res.code === 0) setHistoricalInstanceValues(res.data?.values || []);
		}).catch(() => {
			if (!cancelled) setHistoricalInstanceValues([]);
		});
		return () => { cancelled = true; };
	}, [targetKey, targetHost, targetService, taskScope, sessionSID, profileType, timeWindow]);

	useEffect(() => {
		if (selectedInstance && !processInstances.some(instance => instance.value === selectedInstance)) {
			setSelectedInstance('');
		}
	}, [processInstances, selectedInstance]);

    useEffect(() => {
        if (profileType !== 'memory' || rssSeries.length === 0) return;
        const names = prioritizeProcessNames(rssSeries.map(series => series.comm));
        setCommValues(names);
        setCommAvailable(names.length > 0);
        setRuntimeValues(values => Array.from(new Set([...values, 'python'])));
    }, [profileType, rssSeries]);

    if (!target) {
        return <div style={S.empty}>暂无可观测对象。启动 drop_agent 或创建过按需任务后会出现在这里。</div>;
    }

    return (
        <div className="continuous-profiling-panel" style={S.panel}>
            <section style={S.card}>
                <div style={S.head}>
                    <div>
                        <div style={S.titleLine}>
							<h3 style={S.title}>{sessionMeta.name}</h3>
                            <span style={S.stateBadge}>{sampleState}</span>
                        </div>
                        <p style={S.subtitle}>{targetTitle} · {targetService}</p>
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
                        <select style={S.select} value={range} onChange={e => changeRange(e.target.value)}>
                            {rangeOptions.map(([value, label]) => <option key={value} value={value}>{label}</option>)}
                            <option value="custom">自定义时间</option>
                        </select>
                    </Field>
                    <Field label="Profile 类型">
                        <select style={S.select} value={profileType} onChange={e => setProfileType(e.target.value)}>
                            <option value="cpu">CPU</option>
                        </select>
                    </Field>
                    <Field label="语言">
                        <select style={S.select} value={selectedRuntime} onChange={e => setSelectedRuntime(e.target.value)}>
                            <option value="">全部语言</option>
                            {runtimeValues.map(value => <option key={value} value={value}>{runtimeLabel(value)}</option>)}
                        </select>
                    </Field>
                    {signalTab === 'cpu' && profileType === 'cpu' && (
                        <Field label="最大节点数">
                            <select style={S.select} value={String(maxNodes)} onChange={e => setMaxNodes(parseInt(e.target.value, 10))}>
                                <option value="1000">1000</option>
                                <option value="5000">5000（默认）</option>
                                <option value="10000">10000</option>
                                <option value="20000">20000（最大）</option>
                            </select>
                        </Field>
                    )}
                    <Field label="信号">
                        <span style={S.segmented}>
                            {availableSignalTabs.map((option, index) => (
                                <button
                                    key={option.tab}
                                    type="button"
                                    style={{ ...S.segment(signalTab === option.tab), ...(index === availableSignalTabs.length - 1 ? { borderRight: 'none' } : {}) }}
                                    onClick={() => setSignalTab(option.tab)}
                                >
                                    {option.label}
                                </button>
                            ))}
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
					{taskScope === 'host' && <Field label="查询范围">
						<span style={S.segmented}>
							<button type="button" style={S.segment(scope === 'host')} onClick={() => setScope('host')}>全部进程</button>
							<button type="button" style={{ ...S.segment(scope === 'process'), borderRight: 'none' }} onClick={() => setScope('process')} disabled={!commAvailable}>按 comm 筛选</button>
						</span>
					</Field>}
					{taskScope === 'host' && scope === 'process' && (
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
					{taskScope === 'process' && signalTab === 'cpu' && <Field label="进程实例" wide>
						<select style={S.select} value={selectedInstance} onChange={event => setSelectedInstance(event.target.value)}>
							<option value="">全部有样本实例</option>
							{processInstances.map(instance => <option key={instance.value} value={instance.value}>PID {instance.pid} · {formatTime(instance.processStartMs)}{instance.active ? ' · 活动' : ' · 历史'}</option>)}
						</select>
                        <div style={S.inlineNote}>
                            {processInstances.length === 0
                                ? '当前查询窗口没有可选进程实例样本。'
                                : `当前窗口 ${processInstances.length} 个实例有样本${activeProcessCount > processInstances.length ? `；另有 ${activeProcessCount - processInstances.length} 个活动实例当前无样本` : ''}。`}
                        </div>
					</Field>}
                </div>
                {range === 'custom' && (
                    <div style={S.customRange}>
                        <TimeRangeSlider
                            fromInput={customFrom}
                            toInput={customTo}
                            retentionHours={sessionMeta.retentionHours}
                            anchorNow={customAnchorNow}
                            onChange={({ fromInput, toInput }) => {
                                setCustomFrom(fromInput);
                                setCustomTo(toInput);
                            }}
                        />
                        <button type="button" style={S.btn} onClick={applyCustomRange} disabled={querying}>查询</button>
                    </div>
                )}
                <div style={{ ...S.summaryGrid, marginTop: 14 }}>
                    <Metric label="采集方式" value={sessionMeta.sampler} />
                    <Metric label="采样频率" value={formatRateHz(sessionMeta.sampleRateHz)} />
                    <Metric label="聚合窗口" value={formatDurationSec(sessionMeta.aggregationWindowSec)} />
                    <Metric label="上传周期" value={formatDurationSec(sessionMeta.uploadBatchSec)} />
                </div>
                <div style={{ ...S.info, marginTop: 14 }}>
					{signalTab === 'cpu' ? scopeLabel : signalTab === 'db' ? '数据库快照 / 系统视图轮询' : `${taskScope === 'process' ? '进程范围' : '整机范围'} ${signalTab === 'io' ? '块 IO 延迟' : signalTab === 'io_syscall' ? '系统调用 IO 延迟' : '调度延迟'} / eBPF histogram`}；{sessionMeta.sampler} 以 {formatRateHz(sessionMeta.sampleRateHz)} 低频采样，当前查询窗口：{formatTime(timeWindow.from)} - {formatTime(timeWindow.to)}。
                    {signalTab === 'cpu' ? ' comm 是 Linux task comm，可能被截断到约 15 字符。'
                        : signalTab === 'db' ? ' SQL 仅保留数据库归一化后的 digest（占位符形式），不含原始参数。'
                        : ` 当前 backend：${histogram?.backend || signalBackend(sessionMeta, signalTab) || '-'}`}
                    {scope === 'process' && commMessage ? ` ${commMessage}` : ''}
                </div>
                <details style={S.compactDetails}>
                    <summary style={S.detailsSummary}>采集元信息</summary>
                    <div style={S.metaLine}>
                        <span style={S.metaItem}><span style={S.metaKey}>数据保留</span>{formatHours(sessionMeta.retentionHours)}</span>
                        <span style={uploadState.warn ? { ...S.metaItem, ...S.metaItemWarn } : S.metaItem} title={uploadState.title}>
                            <span style={S.metaKey}>最近上传</span>{uploadState.label}
                        </span>
                        <span style={S.metaItem}><span style={S.metaKey}>Session</span>{shortSessionID(sessionMeta.sid)}</span>
                        <span style={S.metaItem}><span style={S.metaKey}>样本状态</span>{sampleState}</span>
                        <span style={S.metaItem} title="本次查询实际使用的存储层与最粗分辨率（v1=分钟 JSON 热窗口，v2=Parquet 历史块）">
                            <span style={S.metaKey}>存储来源</span>{storageSourceLabel(flamegraph?.storage_source || topn?.storage_source || rssMeta?.storage_source || 'parquet_v1')}
                        </span>
                        <span style={S.metaItem} title="查询使用的最粗时间分辨率（秒）">
                            <span style={S.metaKey}>分辨率</span>{resolutionLabel(flamegraph?.resolution_seconds || topn?.resolution_seconds || rssMeta?.resolution_seconds || 60)}
                        </span>
                        {(flamegraph?.mixed_resolution || topn?.mixed_resolution || rssMeta?.mixed_resolution) && (
                            <span style={S.metaItem}><span style={S.metaKey}>混合分辨率</span>是</span>
                        )}
                    </div>
                    <LabelChips target={target} />
                </details>
                <CoverageBand reliability={reliability} signal={currentSignal} />
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
                    <div style={S.flameActions}>
                        <input
                            type="search"
                            style={S.searchInput}
                            value={flameSearchInput}
                            onChange={e => setFlameSearchInput(e.target.value)}
                            placeholder="搜索函数名"
                            aria-label="搜索火焰图函数名"
                            disabled={!hasFlamegraph}
                        />
                        <span style={S.subtle}>
                            {flameSearchText
                                ? `${searchStats.matches} 帧 · ${formatSearchPercent(searchStats.samplePercent)}`
                                : hasFlamegraph
                                    ? `已渲染 ${renderStats.rendered}/${renderStats.total || countProfileNodes(flamegraph.nodes)}`
                                    : '暂无栈帧节点'}
                        </span>
                    </div>
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
                    searchText={flameSearchText}
                    onRenderStats={setRenderStats}
                    onSearchStats={setSearchStats}
                />
                {sampleNotice && <div style={{ ...S.info, marginTop: 12 }}>{sampleNotice}</div>}
                {coverageAlert && <CoverageAlert alert={coverageAlert} />}
			</section> : signalTab === 'db' ? <DBSnapshotPanel data={dbSnapshot} loading={querying} targetIP={target?.ip} timeWindow={timeWindow} sid={sessionSID} sessionLabels={decodeJSONField(fixedSession?.labels, {})} />
                : <HistogramPanel
                    data={histogram} loading={querying}
                    title={signalTab === 'io' ? '块 IO 延迟' : signalTab === 'io_syscall' ? '系统调用 IO 延迟' : '调度延迟'}
                    targetIP={target?.ip}
                    signal={SIGNAL_TAB_OPTIONS.find(option => option.tab === signalTab)?.signal}
                    timeWindow={timeWindow}
                />}

            {signalTab === 'cpu' && profileType === 'cpu' && (flamegraph?.truncated || flamegraph?.symbol_status) && (
                <div style={{ ...S.warn, marginTop: 10 }}>
                    {flamegraph?.truncated && <span>火焰图节点数超过 {maxNodes} 上限，已截断展示。请缩小时间范围或提高最大节点数以查看完整栈。</span>}
                    {flamegraph?.truncated && flamegraph?.symbol_status && ' · '}
                    {flamegraph?.symbol_status && flamegraph?.symbol_status !== 'not_applicable' && (
                        <span>
                            符号状态：{symbolStatusLabel(flamegraph.symbol_status, flamegraph.symbol_diagnostics)}
                            {symbolNeedsCheck && sessionSID && (
                                <button type="button" style={{ ...S.diagnosticCopy, marginLeft: 10 }} onClick={checkSymbols} disabled={symbolChecking}>
                                    {symbolChecking ? '检查中...' : '重新检查符号'}
                                </button>
                            )}
                            {symbolCheck && <div style={{ marginTop: 8, color: '#475467' }}>{symbolCheckSummary(symbolCheck)}</div>}
                            {symbolCheckError && <div style={{ marginTop: 8 }}>{symbolCheckError}</div>}
                        </span>
                    )}
                </div>
            )}

            {signalTab === 'cpu' && <RuntimeDiagnostics diagnostics={flamegraph?.runtime_diagnostics || topn?.runtime_diagnostics} />}

            {profileType === 'memory' && signalTab === 'cpu' && <RSSTrend series={rssSeries} meta={rssMeta} loading={querying} />}

            {signalTab === 'cpu' && <section style={S.card}>
                <div style={S.sectionHead}>
                    <h3 style={S.title}>热点 TopN</h3>
                    <span style={S.subtle}>{topn?.items?.length || 0} functions · {profileUnitLabel(topn?.unit || unit)}</span>
                </div>
                <TopNTable data={topn} loading={querying} profileURL={profileURL} filterText={activeFilterText} />
                {topn?.truncated && (
                    <div style={{ ...S.warn, marginTop: 8 }}>
                        TopN 结果超过 {maxNodes} 条上限，已截断展示。
                    </div>
                )}
            </section>}

            {signalTab === 'cpu' && profileType === 'cpu' && (
                <section style={S.card}>
                    <div style={S.sectionHead}>
                        <h3 style={S.title}>时间窗 Diff（Baseline vs Compare）</h3>
                        <button type="button" style={S.btn} onClick={() => setDiffOpen(o => !o)}>{diffOpen ? '收起' : '展开'}</button>
                    </div>
                    {diffOpen && (
                        <div>
                            <DiffWindowSlider
                                diffRange={diffRange}
                                diffRangeOptions={diffRangeOptions}
                                retentionHours={sessionMeta.retentionHours}
                                baseFromInput={diffBaseFrom}
                                compareToInput={diffCompareTo}
                                onRangeChange={setDiffRange}
                                onChange={(windows) => {
                                    setAppliedDiffCustomWindows(windows);
                                    applyDiffWindows(windows);
                                }}
                                onManualInput={changeDiffManualInput}
                            />
                            <div style={{ marginTop: 10, display: 'flex', gap: 10, alignItems: 'center', flexWrap: 'wrap' }}>
                                <span style={S.segmented}>
                                    <button type="button" style={S.segment(diffViewMode === 'table')} onClick={() => setDiffViewMode('table')}>表格</button>
                                    <button type="button" style={{ ...S.segment(diffViewMode === 'flamegraph'), borderRight: 'none' }} onClick={() => setDiffViewMode('flamegraph')}>火焰图</button>
                                </span>
                                <button type="button" style={S.btn} onClick={runActiveDiffView} disabled={(diffViewMode === 'flamegraph' ? diffFlamegraphLoading : diffLoading) || !target}>
                                    {(diffViewMode === 'flamegraph' ? diffFlamegraphLoading : diffLoading) ? '查询中...' : '执行 Diff'}
                                </button>
                            </div>
                            {diffViewMode === 'table' && diffError && <div style={{ ...S.error, marginTop: 8 }}>{diffError}</div>}
                            {diffViewMode === 'table' && diffResult && !diffResult.empty && Array.isArray(diffResult.items) && diffResult.items.length > 0 && (
                                <div className="table-scroll" style={{ ...S.tableWrap, marginTop: 12 }}>
                                    <table style={{ ...S.table, width: '100%' }}>
                                        <thead>
                                            <tr>
                                                <th style={S.th}>函数</th>
                                                <th style={S.th}>Baseline</th>
                                                <th style={S.th}>Compare</th>
                                                <th style={S.th}>Delta</th>
                                                <th style={S.th}>变化</th>
                                            </tr>
                                        </thead>
                                        <tbody>
                                            {diffResult.items.slice(0, 20).map((item, idx) => {
                                                const delta = (item.compare_value || 0) - (item.base_value || 0);
                                                const pct = item.base_value ? (delta / item.base_value * 100) : 0;
                                                const increased = delta > 0;
                                                return (
                                                    <tr key={idx}>
                                                        <td style={S.td}>{item.name || item.function}</td>
                                                        <td style={S.td}>{formatNum(item.base_value)}</td>
                                                        <td style={S.td}>{formatNum(item.compare_value)}</td>
                                                        <td style={{ ...S.td, color: increased ? '#B42318' : delta < 0 ? '#067647' : '#475467' }}>
                                                            {delta >= 0 ? '+' : ''}{formatNum(delta)}
                                                        </td>
                                                        <td style={{ ...S.td, color: increased ? '#B42318' : delta < 0 ? '#067647' : '#475467' }}>
                                                            {pct >= 0 ? '+' : ''}{pct.toFixed(1)}%
                                                        </td>
                                                    </tr>
                                                );
                                            })}
                                        </tbody>
                                    </table>
                                </div>
                            )}
                            {diffViewMode === 'table' && diffResult && (diffResult.empty || !diffResult.items || diffResult.items.length === 0) && (
                                <div style={{ ...S.warn, marginTop: 8 }}>
                                    Diff 结果为空：{diffResult.message || '所选时间窗内无匹配样本，请扩大时间范围或检查 backend 是否有数据。'}
                                </div>
                            )}
                            {diffViewMode === 'flamegraph' && diffFlamegraphError && <div style={{ ...S.error, marginTop: 8 }}>{diffFlamegraphError}</div>}
                            {diffViewMode === 'flamegraph' && diffFlamegraphResult?.degraded && (
                                <div style={{ ...S.warn, marginTop: 8 }}>
                                    {diffFlamegraphResult.message || '所选时间范围里有一段数据已降级为冷层摘要，无法生成调用树差分火焰图，请切换到表格视图查看。'}
                                </div>
                            )}
                            {diffViewMode === 'flamegraph' && diffFlamegraphResult && !diffFlamegraphResult.degraded && (
                                <div style={{ marginTop: 12 }}>
                                    <div style={{ display: 'flex', gap: 16, alignItems: 'center', marginBottom: 8, fontSize: 13, color: '#475467' }}>
                                        <span><span style={{ display: 'inline-block', width: 10, height: 10, borderRadius: 2, background: '#b42318', marginRight: 4 }} />变热/新增</span>
                                        <span><span style={{ display: 'inline-block', width: 10, height: 10, borderRadius: 2, background: '#175cd3', marginRight: 4 }} />变冷/消失</span>
                                        <span><span style={{ display: 'inline-block', width: 10, height: 10, borderRadius: 2, background: '#eaecf0', marginRight: 4 }} />基本不变</span>
                                    </div>
                                    <InteractiveFlamegraph
                                        data={diffFlamegraphData}
                                        loading={diffFlamegraphLoading}
                                        emptyMessage="暂无可对比数据"
                                        diffMode
                                    />
                                </div>
                            )}
                        </div>
                    )}
                </section>
            )}

            {profileType === 'memory' && (
                <section style={S.card}>
                    <div style={S.sectionHead}>
                        <h3 style={S.title}>Memray Allocation Profiles</h3>
                        <span style={S.subtle}>显式 Mini-Drop/Memray SDK 上报 · {memoryProfilesLoading ? '加载中...' : `${memoryProfiles.length} profiles`}</span>
                    </div>
                    <div style={{ ...S.info, marginTop: 8 }}>
                        以下为所选时间范围内 SDK 上报的 allocation profile（峰值存活字节）。状态 ready 表示已正常消费；duplicate 表示同一 profile 在多个窗口重复出现（已去重，不双计）；failed 表示 profile 存在但无可用样本。
                    </div>
                    {memoryProfilesLoading ? <div style={S.empty}>正在查询 Memray profiles...</div> : memoryProfiles.length === 0 ? (
                        <div style={S.empty}>所选范围没有 Memray allocation profile（Mini-Drop/Memray SDK 未启用，或最近没有完整 profile）</div>
                    ) : (
                        <div className="table-scroll" style={{ ...S.tableWrap, marginTop: 12 }}>
                            <table style={{ ...S.table, width: '100%' }}>
                                <thead>
                                    <tr>
                                        <th style={S.th}>Profile ID</th>
                                        <th style={S.th}>时间窗口</th>
                                        <th style={S.th}>进程</th>
                                        <th style={S.th}>样本</th>
                                        <th style={S.th}>状态</th>
                                    </tr>
                                </thead>
                                <tbody>
                                    {memoryProfiles.slice(0, 50).map((profile, index) => (
                                        <tr key={`${profile.profile_id}-${profile.window_start}-${index}`}>
                                            <td style={S.td}><code>{profile.profile_id}</code></td>
                                            <td style={S.td}>{formatTime(profile.window_start)} - {formatTime(profile.window_end)}</td>
                                            <td style={S.td}>{profile.comm || 'python'} · PID {profile.pid || '-'}{profile.exe ? ` · ${profile.exe}` : ''}</td>
                                            <td style={S.td}>{formatMetricValue(profile.sample_count || 0, 'bytes')}</td>
                                            <td style={S.td}>
                                                <span style={{ ...S.stateBadge, ...(profile.status === 'ready' ? { background: '#ecfdf3', color: '#067647' } : profile.status === 'duplicate' ? { background: '#FEF3C7', color: '#B54708' } : { background: '#FEE4E2', color: '#B42318' }) }}>
                                                    {profile.status}
                                                </span>
                                                {profile.reason && <div style={{ ...S.subtle, marginTop: 4 }}>{profile.reason}</div>}
                                            </td>
                                        </tr>
                                    ))}
                                </tbody>
                            </table>
                        </div>
                    )}
                </section>
            )}

            {profileType === 'memory' && (
                <section style={S.card}>
                    <div style={S.sectionHead}>
                        <h3 style={S.title}>Go pprof Heap 按需任务</h3>
                        <span style={S.subtle}>保留按需深度采集入口</span>
                    </div>
                    <div style={{ ...S.info, marginTop: 8 }}>
                        上方持续 Memory 视图显示 Python RSS 与显式 Mini-Drop/Memray SDK 的峰值存活字节。Go 堆仍通过 <code>Go pprof Heap</code> 按需任务采集。
                    </div>
                    <div style={{ marginTop: 12, display: 'flex', gap: 8, alignItems: 'center' }}>
                        <a style={{ ...S.btn, textDecoration: 'none', display: 'inline-block' }} href={`/hosts/${encodeURIComponent(target?.id || '')}?tab=tasks`}>
                            前往主机任务
                        </a>
                        {heapTasksLoading && <span style={S.subtle}>加载最近任务...</span>}
                    </div>
                    {Array.isArray(heapTasks) && heapTasks.length > 0 && (
                        <div style={{ marginTop: 12 }}>
                            <div style={S.subtle}>最近 Go Heap 任务</div>
                            <ul style={{ listStyle: 'none', padding: 0, marginTop: 6 }}>
                                {heapTasks.map((t, i) => (
                                    <li key={t.tid || t.id || i} style={{ padding: '4px 0', borderBottom: '1px solid #F2F4F7' }}>
                                        <a href={`/task/result?tid=${encodeURIComponent(t.tid || t.id || '')}`} style={{ color: '#315efb' }}>
                                            {t.name || `Heap Task ${t.tid || t.id || i + 1}`}
                                        </a>
                                        <span style={{ ...S.subtle, marginLeft: 8 }}>
                                            {t.status ?? '-'} · {t.create_time || t.created_at ? new Date(t.create_time || t.created_at).toLocaleString() : ''}
                                        </span>
                                    </li>
                                ))}
                            </ul>
                        </div>
                    )}
                    {Array.isArray(heapTasks) && heapTasks.length === 0 && !heapTasksLoading && (
                        <div style={{ ...S.subtle, marginTop: 8 }}>暂无 Go Heap 任务记录</div>
                    )}
                </section>
            )}

            <DiagnosticDetails target={target} flamegraph={flamegraph} topn={topn} timeWindow={timeWindow} profileType={profileType} stackScope={stackScope} filters={activeFilters} />
        </div>
    );
}

export function DiagnosticDetails(props) {
    const [copied, setCopied] = useState(false);
    const text = diagnosticText(props);
    const copy = async () => {
        try {
            await navigator.clipboard.writeText(text);
            setCopied(true);
            window.setTimeout(() => setCopied(false), 1200);
        } catch (err) {
            setCopied(false);
        }
    };
    return (
        <details className="diagnostic-details" style={S.details}>
            <summary style={S.detailsSummary}>诊断信息</summary>
            <div style={S.diagnosticToolbar}>
                <span style={S.subtle}>调试用</span>
                <button type="button" style={S.diagnosticCopy} onClick={copy}>{copied ? '已复制' : '复制诊断'}</button>
            </div>
            <pre className="diagnostic-output" style={S.mono}>{text}</pre>
        </details>
    );
}

function TimeRangeSlider({ fromInput, toInput, retentionHours, anchorNow, onChange }) {
    const slider = customInputsToSlider(fromInput, toInput, retentionHours, anchorNow);
    const updateWindow = (fromMinute, toMinute) => {
        onChange(sliderMinutesToInputs(fromMinute, toMinute, retentionHours, slider.now));
    };
    const drag = useRangeDrag({
        maxMinute: slider.maxMinute,
        fromMinute: slider.fromMinute,
        toMinute: slider.toMinute,
        minSpan: 1,
        onChange: updateWindow,
    });
    const spanSeconds = Math.max(0, (slider.toMinute - slider.fromMinute) * 60);
    return (
        <div className="continuous-time-slider" style={S.timeSlider}>
            <div style={S.sliderFrame}>
                <div style={S.sliderMeta}>
                    <span>保留期 {formatTime(slider.bounds.from.toISOString())}</span>
                    <strong style={{ color: '#344054' }}>跨度 {spanSeconds > 0 ? formatGapDuration(spanSeconds) : '-'}</strong>
                    <span>{formatTime(slider.bounds.to.toISOString())}</span>
                </div>
                <RangeTrack
                    fromMinute={slider.fromMinute}
                    toMinute={slider.toMinute}
                    maxMinute={slider.maxMinute}
                    onStartDrag={drag.startDrag}
                    selectionLabel="拖动自定义时间范围"
                    leftLabel="拖动开始时间"
                    rightLabel="拖动结束时间"
                />
                <div style={S.sliderInputs}>
                    <Field label="开始时间">
                        <input type="datetime-local" step="60" style={S.textInput} value={fromInput} onChange={event => onChange({ fromInput: event.target.value, toInput })} />
                    </Field>
                    <Field label="结束时间">
                        <input type="datetime-local" step="60" style={S.textInput} value={toInput} onChange={event => onChange({ fromInput, toInput: event.target.value })} />
                    </Field>
                </div>
            </div>
        </div>
    );
}

function DiffWindowSlider({ diffRange, diffRangeOptions, retentionHours, baseFromInput, compareToInput, onRangeChange, onChange, onManualInput }) {
    const duration = rangeMinutes(diffRange) || 15;
    const bounds = retentionBounds(retentionHours);
    const maxMinute = Math.max(2, Math.round((bounds.to.getTime() - bounds.from.getTime()) / 60000));
    const currentStart = localInputToMinuteOffset(baseFromInput, bounds);
    const currentEnd = localInputToMinuteOffset(compareToInput, bounds);
    const fallback = sequentialDiffWindowMinutes(maxMinute, duration);
    const fromMinute = clampNumber(Number.isFinite(currentStart) ? currentStart : fallback.fromMinute, 0, maxMinute - 2);
    const toMinute = clampNumber(Number.isFinite(currentEnd) ? currentEnd : fallback.toMinute, fromMinute + 2, maxMinute);
    const safeSpan = normalizeEvenSpan(fromMinute, toMinute, maxMinute);
    const windows = diffWindowsFromMinutes(safeSpan.fromMinute, safeSpan.toMinute, bounds);
    const drag = useRangeDrag({
        maxMinute,
        fromMinute: safeSpan.fromMinute,
        toMinute: safeSpan.toMinute,
        minSpan: 2,
        snapEven: true,
        onChange: (nextFrom, nextTo) => onChange(diffWindowsFromMinutes(nextFrom, nextTo, bounds)),
    });
    const updateRange = (value) => {
        onRangeChange(value);
        const nextDuration = rangeMinutes(value) || duration;
        const next = sequentialDiffWindowMinutes(maxMinute, nextDuration);
        onChange(diffWindowsFromMinutes(next.fromMinute, next.toMinute, bounds));
    };
    return (
        <div style={S.sliderFrame}>
            <div style={S.controls}>
                <Field label="窗口时长">
                    <select style={S.select} value={diffRange} onChange={event => updateRange(event.target.value)}>
                        {diffRangeOptions.map(([v, l]) => <option key={v} value={v}>{l.replace('最近 ', '')}</option>)}
                    </select>
                </Field>
            </div>
            <div style={{ ...S.sliderMeta, marginTop: 10 }}>
                <span>Baseline {formatTime(windows.baseWindow.from)} - {formatTime(windows.baseWindow.to)}</span>
                <strong style={{ color: '#344054' }}>相邻等长</strong>
                <span>Compare {formatTime(windows.compareWindow.from)} - {formatTime(windows.compareWindow.to)}</span>
            </div>
            <RangeTrack
                fromMinute={safeSpan.fromMinute}
                toMinute={safeSpan.toMinute}
                maxMinute={maxMinute}
                splitMinute={(safeSpan.fromMinute + safeSpan.toMinute) / 2}
                onStartDrag={drag.startDrag}
                selectionLabel="拖动 Diff 双窗"
                leftLabel="拖动 Diff 左边界"
                rightLabel="拖动 Diff 右边界"
            />
            <div style={S.diffWindows}>
                <Field label="Baseline 开始">
                    <input type="datetime-local" step="60" style={S.textInput} value={toLocalDateTimeInput(windows.baseWindow.from)} onChange={event => onManualInput('baseFrom', event.target.value)} />
                </Field>
                <Field label="Baseline 结束">
                    <input type="datetime-local" step="60" style={S.textInput} value={toLocalDateTimeInput(windows.baseWindow.to)} onChange={event => onManualInput('baseTo', event.target.value)} />
                </Field>
                <Field label="Compare 开始">
                    <input type="datetime-local" step="60" style={S.textInput} value={toLocalDateTimeInput(windows.compareWindow.from)} onChange={event => onManualInput('compareFrom', event.target.value)} />
                </Field>
                <Field label="Compare 结束">
                    <input type="datetime-local" step="60" style={S.textInput} value={toLocalDateTimeInput(windows.compareWindow.to)} onChange={event => onManualInput('compareTo', event.target.value)} />
                </Field>
            </div>
        </div>
    );
}

function RangeTrack({ fromMinute, toMinute, maxMinute, splitMinute = null, onStartDrag, selectionLabel, leftLabel, rightLabel }) {
    const trackRef = useRef(null);
    const leftPct = minutePercent(fromMinute, maxMinute);
    const rightPct = minutePercent(toMinute, maxMinute);
    const widthPct = Math.max(0, rightPct - leftPct);
    const splitPct = splitMinute === null ? null : minutePercent(splitMinute, maxMinute);
    return (
        <div ref={trackRef} style={S.sliderTrack}>
            <div style={S.sliderBase} />
            {splitPct === null ? (
                <div
                    role="slider"
                    aria-label={selectionLabel}
                    tabIndex={0}
                    style={{ ...S.sliderSelection, left: `${leftPct}%`, width: `${widthPct}%` }}
                    onPointerDown={event => onStartDrag(event, 'window', trackRef)}
                />
            ) : (
                <>
                    <div
                        role="slider"
                        aria-label={selectionLabel}
                        tabIndex={0}
                        style={{ ...S.sliderWindowA, left: `${leftPct}%`, width: `${Math.max(0, splitPct - leftPct)}%` }}
                        onPointerDown={event => onStartDrag(event, 'window', trackRef)}
                    />
                    <div
                        role="slider"
                        aria-label={selectionLabel}
                        tabIndex={0}
                        style={{ ...S.sliderWindowB, left: `${splitPct}%`, width: `${Math.max(0, rightPct - splitPct)}%` }}
                        onPointerDown={event => onStartDrag(event, 'window', trackRef)}
                    />
                    <span style={{ ...S.sliderSplit, left: `${splitPct}%` }} />
                </>
            )}
            <button
                type="button"
                aria-label={leftLabel}
                style={{ ...S.sliderThumb, ...S.sliderThumbMuted, left: `${leftPct}%` }}
                onPointerDown={event => onStartDrag(event, 'left', trackRef)}
            />
            <button
                type="button"
                aria-label={rightLabel}
                style={{ ...S.sliderThumb, left: `${rightPct}%` }}
                onPointerDown={event => onStartDrag(event, 'right', trackRef)}
            />
        </div>
    );
}

function useRangeDrag({ maxMinute, fromMinute, toMinute, minSpan = 1, snapEven = false, onChange }) {
    const startDrag = useCallback((event, mode, trackRef) => {
        const track = trackRef.current;
        if (!track) return;
        event.preventDefault();
        track.setPointerCapture?.(event.pointerId);
        const rect = track.getBoundingClientRect();
        const startX = event.clientX;
        const startFrom = fromMinute;
        const startTo = toMinute;
        const span = startTo - startFrom;
        const minuteFromEvent = (clientX) => {
            const ratio = rect.width <= 0 ? 0 : (clientX - rect.left) / rect.width;
            return clampNumber(Math.round(ratio * maxMinute), 0, maxMinute);
        };
        const apply = (clientX) => {
            let nextFrom = startFrom;
            let nextTo = startTo;
            if (mode === 'window') {
                const delta = minuteFromEvent(clientX) - minuteFromEvent(startX);
                nextFrom = clampNumber(startFrom + delta, 0, Math.max(0, maxMinute - span));
                nextTo = nextFrom + span;
            } else if (mode === 'left') {
                nextFrom = clampNumber(minuteFromEvent(clientX), 0, startTo - minSpan);
                nextTo = startTo;
            } else {
                nextFrom = startFrom;
                nextTo = clampNumber(minuteFromEvent(clientX), startFrom + minSpan, maxMinute);
            }
            if (snapEven) {
                ({ fromMinute: nextFrom, toMinute: nextTo } = normalizeEvenSpan(nextFrom, nextTo, maxMinute));
            }
            onChange(nextFrom, nextTo);
        };
        const move = (moveEvent) => apply(moveEvent.clientX);
        const stop = () => {
            window.removeEventListener('pointermove', move);
            window.removeEventListener('pointerup', stop);
            window.removeEventListener('pointercancel', stop);
        };
        window.addEventListener('pointermove', move);
        window.addEventListener('pointerup', stop);
        window.addEventListener('pointercancel', stop);
        apply(event.clientX);
    }, [fromMinute, maxMinute, minSpan, onChange, snapEven, toMinute]);
    return { startDrag };
}

export function CoverageBand({ reliability, signal }) {
    const [hover, setHover] = useState(null);
    const [showAllGaps, setShowAllGaps] = useState(false);
    if (!reliability?.coverage) return null;
    const view = coverageBandsFromReliability(reliability, signal);
    const ratio = Math.max(0, Math.min(1, Number(view.ratio) || 0));
    const clock = reliability.clock || {};
    const clockStatus = clock.status || 'unknown';
    const clockBad = clockStatus === 'warning' || clockStatus === 'critical';
    const summary = view.statusSummary || coverageStatusText(view.status);
    // Session 总体状态概览（所有信号合并口径，与当前信号状态独立）。
    const overall = reliability.status_summary || null;
    const displayGaps = showAllGaps ? view.gaps : view.gaps.slice(0, 6);
    return (
        <div style={S.coverage}>
            <div style={S.sectionHead}>
                <strong style={{ fontSize: 13 }}>采集覆盖</strong>
                <span style={S.subtle}>
                    {signal ? `${signalLabel(signal)} · ` : ''}{(ratio * 100).toFixed(1)}% · {view.gapCountTotal} 个缺口
                </span>
            </div>
            <div style={S.coverageWrap} onMouseLeave={() => setHover(null)}>
                <div style={S.coverageBar} role="img" aria-label={`采集覆盖率 ${(ratio * 100).toFixed(1)}%`}>
                    {view.bands.map((band, index) => (
                        <span
                            key={`${band.status}-${index}`}
                            data-testid={`coverage-${band.status}`}
                            style={{ background: coverageStatusColor(band.status), width: `${band.percent}%` }}
                            onMouseEnter={event => setCoverageHover(event, band, setHover)}
                            onMouseMove={event => setCoverageHover(event, band, setHover)}
                        />
                    ))}
                </div>
                {hover && <CoverageTooltip hover={hover} />}
            </div>
            <div style={S.coverageLegend}>
                <LegendItem color="#12b76a" label="有数据" />
                <LegendItem color="#d92d20" label="确认缺数" />
                <LegendItem color="#f79009" label="整理中" />
                <LegendItem color="#98a2b3" label="空闲/启动/收尾" />
            </div>
            {summary?.label && (
                <div style={S.coverageSummary}>
                    <strong style={S.coverageSummaryLabel}>{summary.label}</strong>
                    {summary.explanation && <span>{summary.explanation}</span>}
                    {summary.suggestion && <span style={{ color: '#667085' }}>（{summary.suggestion}）</span>}
                </div>
            )}
            {overall?.label && overall.label !== summary?.label && (
                <div style={{ ...S.coverageSummary, color: '#667085' }}>
                    <strong style={S.coverageSummaryLabel}>Session 总体</strong>
                    {overall.label}（{overall.explanation}）
                </div>
            )}
            <div style={S.metaLine}>
                <span style={clockBad ? { ...S.metaItem, ...S.metaItemWarn } : S.metaItem}>
                    <span style={S.metaKey}>Agent 时钟</span>
                    {clockStatus === 'unknown' ? '未观测' : `${clockStatus} · ${formatClockOffset(clock.offset_ms)}`}
                </span>
            </div>
            {view.gaps.length > 0 && (
                <div style={S.gapList}>
                    {displayGaps.map((gap, index) => (
                        <span key={`${gap.start}-${index}`}>
                            {formatTime(gap.start)} - {formatTime(gap.end)} · {formatGapDuration(gap.duration_seconds)}
                        </span>
                    ))}
                    {view.gapCountTotal > 6 && (
                        <button type="button" style={S.gapToggle} onClick={() => setShowAllGaps(v => !v)}>
                            {showAllGaps ? '收起缺口列表' : `另有 ${view.gapCountTotal - 6} 个缺口`}
                        </button>
                    )}
                </div>
            )}
        </div>
    );
}

function LegendItem({ color, label }) {
    return (
        <span style={S.coverageLegendItem}>
            <span style={{ ...S.coverageDot, background: color }} />
            {label}
        </span>
    );
}

// 阶段九：从 timeline 响应提取当前信号的覆盖视图。优先 signal_coverage[signal]
// 的 coverage_bands；其次顶层 coverage_bands（旧客户端/无 signal 参数）；
// 最后降级为旧 coverage/gaps 两色段（covered/gap）。
export function coverageBandsFromReliability(reliability, signal) {
    const data = reliability || {};
    const legacyCoverage = data.coverage || {};
    const legacyGaps = Array.isArray(data.gaps) ? data.gaps : [];

    const buildView = (source, useLegacyFields) => {
        const sourceCoverage = source?.coverage || (useLegacyFields ? legacyCoverage : {});
        const gaps = Array.isArray(source?.gaps) ? source.gaps : (useLegacyFields ? legacyGaps : []);
        const rawBands = Array.isArray(source?.coverage_bands) ? source.coverage_bands : [];
        const sourceRatio = validCoverageRatio(source?.coverage?.ratio);
        const legacyRatio = validCoverageRatio(legacyCoverage.ratio);
        const ratio = sourceRatio ?? (useLegacyFields ? legacyRatio : validCoverageRatio(data.coverage?.ratio)) ?? 0;
        const statusSummary = source?.status_summary || (useLegacyFields ? data.status_summary : null);

        let bands;
        if (rawBands.length > 0) {
            bands = rawBands.map(band => ({ ...band, percent: bandPercent(band, sourceCoverage) }));
        } else if (gaps.length > 0 || ratio === 1 || useLegacyFields) {
            bands = coverageSegments(sourceCoverage, gaps).map(segment => ({
                start: new Date(segment.start).toISOString(),
                end: new Date(segment.end).toISOString(),
                status: segment.type === 'gap' ? 'real_gap' : 'healthy',
                duration_seconds: (segment.end - segment.start) / 1000,
                percent: segment.percent,
                sample_count: 0,
            }));
        } else {
            // A signal response may legitimately have no detail bands yet.
            // Keep its own zero/unknown state instead of borrowing another signal.
            bands = [];
        }

        const rawGapCount = source?.gap_count_total ?? (useLegacyFields ? data.gap_count_total : undefined);
        const gapCountTotal = validGapCount(rawGapCount) ?? gaps.length;
        const explicitGapSeconds = validNonNegativeNumber(sourceCoverage?.gap_seconds);
        const gapSeconds = explicitGapSeconds ?? gaps.reduce((sum, gap) => sum + gapDurationSeconds(gap), 0);
        const status = source?.status || statusSummary?.status || (gaps.length > 0 ? 'real_gap' : (ratio > 0 ? 'healthy' : 'unknown'));
        return { bands, ratio, gaps, gapCountTotal, gapSeconds, statusSummary: statusSummary || null, status };
    };

    const sc = signal ? data.signal_coverage?.[signal] : null;
    if (sc) {
        // signal_coverage is authoritative even when coverage_bands is empty.
        // In particular, ratio=0 is valid and must never inherit CPU/legacy data.
        return buildView(sc, false);
    }
    return buildView(data, true);
}

function bandPercent(band, coverage) {
    const total = (new Date(coverage?.to).getTime() - new Date(coverage?.from).getTime()) / 1000;
    if (!Number.isFinite(total) || total <= 0) return 0;
    const duration = validNonNegativeNumber(band?.duration_seconds) || 0;
    return Math.max(0, Math.min(100, (duration / total) * 100));
}

function validNonNegativeNumber(value) {
    const number = Number(value);
    return Number.isFinite(number) && number >= 0 ? number : null;
}

function validCoverageRatio(value) {
    const number = Number(value);
    return Number.isFinite(number) && number >= 0 && number <= 1 ? number : null;
}

function validGapCount(value) {
    const number = Number(value);
    return Number.isInteger(number) && number >= 0 ? number : null;
}

function gapDurationSeconds(gap) {
    const explicit = validNonNegativeNumber(gap?.duration_seconds);
    if (explicit !== null) return explicit;
    const start = new Date(gap?.start).getTime();
    const end = new Date(gap?.end).getTime();
    if (!Number.isFinite(start) || !Number.isFinite(end) || end <= start) return 0;
    return (end - start) / 1000;
}

// 状态 → 色带颜色：绿=有数据，红=确认缺数/采集异常，黄=整理中，灰=空闲/启动/收尾/未知。
export const coverageStatusColor = (status) => {
    switch (status) {
        case 'healthy': return '#12b76a';
        case 'real_gap': return '#d92d20';
        case 'collector_failed': return '#b42318';
        case 'pending_upload': return '#f79009';
        case 'target_idle':
        case 'startup_grace':
        case 'shutdown_grace':
        case 'unknown':
        default: return '#98a2b3';
    }
};

// 状态 → 通俗中文文案（与后端 status_summary 同源；旧 API 降级时前端兜底）。
export const coverageStatusText = (status) => ({
    healthy: { label: '数据正常', explanation: '这段时间已经收到有效采集数据', suggestion: '' },
    real_gap: { label: '确认缺少数据', explanation: '这段时间已经超过等待时间，仍没有收到数据', suggestion: '请检查 Agent 状态与网络上传' },
    pending_upload: { label: '数据整理中', explanation: '采集器已经工作，数据还在上传或整理', suggestion: '稍后刷新；如果持续超过上传周期，请检查 Agent 状态' },
    target_idle: { label: '目标暂时空闲', explanation: '目标存在，但这段时间没有可采集活动', suggestion: '无需处理' },
    startup_grace: { label: '正在启动采集', explanation: '采集器刚启动，正在准备首批数据', suggestion: '稍等片刻后刷新' },
    shutdown_grace: { label: '停止收尾中', explanation: '采集已经停止，正在完成最后数据整理', suggestion: '无需处理' },
    collector_failed: { label: '采集异常', explanation: '采集过程出现错误，需要检查 Agent', suggestion: '请检查 Agent 日志与采集能力' },
    unknown: { label: '状态未知', explanation: '这段历史数据缺少足够状态信息', suggestion: '可尝试重新采集' },
})[status] || { label: status || '状态未知', explanation: '', suggestion: '' };

function setCoverageHover(event, segment, setHover) {
    const target = event.currentTarget;
    if (!target) return;
    const offsetX = Number(event.nativeEvent?.offsetX);
    const fallback = target.offsetWidth / 2;
    setHover({
        segment,
        x: target.offsetLeft + (Number.isFinite(offsetX) ? offsetX : fallback),
    });
}

function CoverageTooltip({ hover }) {
    const { segment, x } = hover;
    const text = coverageStatusText(segment.status);
    const left = `min(max(${Math.round(x)}px, 110px), calc(100% - 110px))`;
    return (
        <div role="tooltip" style={{ ...S.coverageTooltip, left, transform: 'translateX(-50%)' }}>
            <div style={S.coverageTooltipTitle}>{text.label}</div>
            <div>{formatTime(segment.start)} - {formatTime(segment.end)}</div>
            <div>持续 {formatGapDuration(segment.duration_seconds)} · 占比 {segment.percent.toFixed(1)}%</div>
            {text.explanation && <div style={{ marginTop: 4 }}>{text.explanation}</div>}
        </div>
    );
}

function CoverageAlert({ alert }) {
    return (
        <div style={{ ...S.coverageAlert, ...(alert.severity === 'warn' ? S.coverageAlertWarn : {}) }}>
            <div style={S.coverageAlertTitle}>采集缺口</div>
            <div>{alert.summary}</div>
            <div style={{ marginTop: 2 }}>{alert.detail}</div>
        </div>
    );
}

export function coverageAlertForReliability(reliability, signal) {
    if (!reliability?.coverage) return null;
    const view = coverageBandsFromReliability(reliability, signal);
    const gaps = view.gaps;
    if (gaps.length === 0) return null;
    const ratio = Math.max(0, Math.min(1, Number(view.ratio) || 0));
    const longest = gaps.reduce((max, gap) => Math.max(max, Number(gap?.duration_seconds) || 0), 0);
    // 累计缺口优先用后端精确统计（gap_seconds），避免详细缺口截断后低估。
    const total = Number(view.gapSeconds) || gaps.reduce((sum, gap) => sum + (Number(gap?.duration_seconds) || 0), 0);
    const severity = ratio < 0.97 || longest >= 15 ? 'warn' : 'info';
    return {
        severity,
        summary: `覆盖 ${(ratio * 100).toFixed(1)}% · ${view.gapCountTotal} 个缺口 · 最长 ${formatGapDuration(longest)}`,
        detail: `累计缺口 ${formatGapDuration(total)}，常见于 agent 短暂卡顿、上传滞后或当前窗口内进程空闲。`,
    };
}

function RuntimeDiagnostics({ diagnostics }) {
    const entries = Object.entries(diagnostics || {});
    if (entries.length === 0) return null;
    return (
        <section style={S.card}>
            <div style={S.sectionHead}><h3 style={S.title}>语言采集状态</h3><span style={S.subtle}>{entries.length} runtimes</span></div>
            <div className="table-scroll" style={S.tableWrap}><table style={S.table}>
                <thead><tr><th style={S.th}>语言</th><th style={S.th}>采集状态</th><th style={S.th}>模式</th><th style={S.th}>覆盖率 / 未解析</th><th style={S.th}>进程</th><th style={S.th}>诊断与修复建议</th></tr></thead>
                <tbody>{entries.map(([runtime, item]) => (
                    <tr key={runtime}>
                        <td style={S.td}>{runtimeLabel(runtime)}</td>
                        <td style={S.td}>{runtimeStatusLabel(item)}</td>
                        <td style={S.td}>{((item.collector_modes || []).length ? item.collector_modes : item.modes || []).join(', ') || '-'}</td>
                        <td style={S.td}>{runtimeCoverageLabel(runtime, item)}</td>
                        <td style={S.td}>{runtimeProcessLabel(item)}</td>
                        <td style={S.td}>{runtimeReasonWithAdvice(runtime, item)}</td>
                    </tr>
                ))}</tbody>
            </table></div>
        </section>
    );
}

// 阶段四：v2 collector_status（ready/partial/missing/pending/failed）文案。
// 旧数据（无 diagnostics_version）继续走 detected/missing 计数推导。
function runtimeStatusLabel(item = {}) {
    if (item.diagnostics_version >= 2 && item.collector_status) {
        return ({
            ready: '就绪',
            partial: '部分可用',
            missing: '缺少采集能力',
            pending: '处理中',
            failed: '采集失败',
            not_applicable: '未检测到',
        })[item.collector_status] || item.collector_status;
    }
    const detected = Number(item.detected_count) || 0;
    const ready = Number(item.ready_count) || 0;
    const missing = Number(item.missing_count) || 0;
    const limited = Number(item.limited_count) || 0;
    if (detected === 0) return '未检测到进程';
    if (ready === detected && limited === 0) return '已检测';
    if (ready > 0 && (missing > 0 || limited > 0)) return '部分可用';
    return '缺少采集能力';
}

function runtimeCoverageLabel(runtime, item = {}) {
    if (item.diagnostics_version >= 2 && item.sample_count > 0) {
        const semanticSample = Number(item.semantic_sample_percent) || 0;
        const semanticFrame = Number(item.semantic_frame_percent) || 0;
        const unresolved = Number(item.unresolved_frame_percent) || 0;
        const targetUnresolved = Number(item.target_module_unresolved_percent) || 0;
        const unresolvedLabel = runtime === 'native'
            ? `目标模块未解析 ${targetUnresolved.toFixed(1)}%`
            : `未解析 ${unresolved.toFixed(1)}%`;
        return `语义样本 ${semanticSample.toFixed(1)}% · 语义帧 ${semanticFrame.toFixed(1)}% · ${unresolvedLabel}`;
    }
    return '-';
}

// 阶段四：按状态/reason 给出可执行修复建议（不再只显示"缺少能力"）。
function runtimeFixAdvice(runtime, item = {}) {
    const reasonsText = (item.reasons || []).join(' ');
    if (item.diagnostics_version >= 2 && item.collector_status === 'failed')
        return reasonsText.includes('permission') ? '检查 Agent 容器权限（--privileged 或 SYS_PTRACE）后重试'
            : '查看 Agent 日志中该语言的确定性失败原因';
    if (item.collector_status === 'pending' || (item.reasons || []).some(r => r.includes('GoReSym background')))
        return 'GoReSym 正在后台提取符号，稍后刷新；结果会在后续窗口生效';
    switch (runtime) {
        case 'node':
            return '使用 --perf-basic-prof 启动 Node.js 以生成 JIT perf map';
        case 'python':
            if (reasonsText.includes('-X perf'))
                return 'Python 3.12+ 使用 -X perf 启动可直接生成 perf map，否则自动回退 py-spy';
            return '确认 py-spy 已安装且 Agent 有 ptrace 权限';
        case 'java':
            return '确认 asprof 可用且 Agent 对 JVM 有 attach 权限（同 UID 或 SYS_PTRACE）';
        case 'go':
            return 'stripped Go 程序依赖 GoReSym 提取符号；保持 DROP_CONTINUOUS_GORESYM 开启并等待缓存生成';
        case 'native':
            if ((item.reasons || []).some(r => r.includes('frame pointer')))
                return '目标程序需保留 frame pointer（-fno-omit-frame-pointer），或将 DROP_NATIVE_CP_CALL_GRAPH 设为 dwarf';
            return '上传对应 build-id 的二进制到符号库可降低未解析率';
        default:
            return '';
    }
}

function runtimeReasonWithAdvice(runtime, item = {}) {
    const reasons = (item.reasons || []).join('; ');
    const advice = runtimeFixAdvice(runtime, item);
    if (!reasons && !advice) return '-';
    return [reasons, advice].filter(Boolean).join('。建议：');
}

function runtimeProcessLabel(item = {}) {
    const detected = Number(item.detected_count) || 0;
    const ready = Number(item.ready_count) || 0;
    const missing = Number(item.missing_count) || 0;
    const limited = Number(item.limited_count) || 0;
    if (detected === 0) {
        if (item.diagnostics_version >= 2 && item.runtime_detection === 'detected' && Number(item.sample_count) > 0)
            return `已采样 ${Math.round(Number(item.sample_count))}`;
        return '未检测到进程';
    }
    return `已检测 ${detected} · 可采集 ${ready}${missing ? ` · 缺少 ${missing}` : ''}${limited ? ` · 受限 ${limited}` : ''}`;
}

function lowSampleGuidance(data, sessionMeta, querying) {
    if (querying || !data || data.empty) return '';
    const total = Number(data.total) || 0;
    if (total <= 0 || total > 100) return '';
    const rate = numberOrDefault(sessionMeta.sampleRateHz, 19);
    return `当前窗口共 ${formatCompactCount(total)} 个样本。${rate} Hz 低频采样遇到空闲进程时，样本少和大量 1 sample 函数属于正常现象；可扩大时间范围或切换到活跃时段再判断。`;
}

function formatCompactCount(value) {
    const num = Number(value) || 0;
    if (num >= 1000000) return `${(num / 1000000).toFixed(1)}M`;
    if (num >= 1000) return `${(num / 1000).toFixed(1)}K`;
    return Math.round(num).toString();
}

function formatEventCount(value) {
    return `${formatCompactCount(value)} 个`;
}

function symbolCheckSummary(check = {}) {
    const missing = Array.isArray(check.missing) ? check.missing : [];
    const buildIDs = Object.keys(check.build_ids || {});
    const presentCount = buildIDs.filter(key => check.build_ids[key]).length;
    const reasons = Array.isArray(check.reasons) ? check.reasons : [];
    const parts = [`符号存储状态：${symbolStatusLabel(check.symbol_status || 'unknown', {})}`];
    if (buildIDs.length > 0) parts.push(`build-id ${presentCount}/${buildIDs.length} 可用`);
    if (check.kallsyms !== undefined) parts.push(`kallsyms ${check.kallsyms ? '可用' : '缺失'}`);
    if (missing.length > 0) parts.push(`缺失：${missing.slice(0, 3).join(', ')}${missing.length > 3 ? '…' : ''}`);
    if (reasons.length > 0) parts.push(`原因：${reasons.slice(0, 3).join('；')}${reasons.length > 3 ? '…' : ''}`);
    return parts.join(' · ');
}

function RSSTrend({ series = [], meta = null, loading }) {
    const width = 900;
    const height = 240;
    const allPoints = series.flatMap(item => item.points || []);
    const times = allPoints.map(point => new Date(point.timestamp).getTime()).filter(Number.isFinite);
    const maxValue = Math.max(1, ...allPoints.map(point => Number(point.value) || 0));
    const minTime = times.length ? Math.min(...times) : 0;
    const maxTime = times.length ? Math.max(...times) : 1;
    const colors = ['#315efb', '#067647', '#b54708', '#b42318', '#6941c6', '#026aa2'];
    const diagnostics = Array.isArray(meta?.diagnostics) ? meta.diagnostics : [];
    return (
        <section style={S.card}>
            <div style={S.sectionHead}><h3 style={S.title}>Python RSS 趋势</h3><span style={S.subtle}>{series.length} processes · bytes{meta?.rss_truncated ? ` · 已截断 ${meta.rss_truncated}` : ''}</span></div>
            {loading ? <div style={S.empty}>正在查询 RSS...</div> : series.length === 0 ? (
                <div style={S.empty}>
                    所选范围没有 Python RSS 数据
                    {diagnostics.length > 0 && (
                        <ul style={{ marginTop: 8, paddingLeft: 18, color: '#475467', fontSize: 12, textAlign: 'left' }}>
                            {diagnostics.map((diag, index) => <li key={index}>{diag}</li>)}
                        </ul>
                    )}
                </div>
            ) : (
                <>
                    <svg role="img" aria-label="Python RSS 趋势图" viewBox={`0 0 ${width} ${height}`} style={{ width: '100%', height: 240, border: '1px solid #eaecf0', background: '#fff' }}>
                        {[0.25, 0.5, 0.75].map(value => <line key={value} x1="0" x2={width} y1={height * value} y2={height * value} stroke="#eaecf0" />)}
                        {series.map((item, index) => {
                            const points = (item.points || []).map(point => {
                                const time = new Date(point.timestamp).getTime();
                                const x = maxTime === minTime ? 0 : ((time - minTime) / (maxTime - minTime)) * width;
                                const y = height - ((Number(point.value) || 0) / maxValue) * (height - 8);
                                return `${x},${y}`;
                            }).join(' ');
                            return <polyline key={`${item.pid}-${item.process_start_ms || 0}-${item.exe}`} points={points} fill="none" stroke={colors[index % colors.length]} strokeWidth="2" />;
                        })}
                    </svg>
                    <div style={{ ...S.chipWrap, marginTop: 10 }}>{series.slice(0, 20).map((item, index) => <span style={S.chip} key={`${item.pid}-${item.process_start_ms || 0}-${item.exe}`}><span style={{ width: 8, height: 8, background: colors[index % colors.length] }} />{item.comm || 'python'} · PID {item.pid} · peak {formatMetricValue(item.peak || 0, 'bytes')}</span>)}</div>
                    {diagnostics.length > 0 && (
                        <div style={{ ...S.info, marginTop: 10 }}>
                            {diagnostics.map((diag, index) => <div key={index}>{diag}</div>)}
                        </div>
                    )}
                </>
            )}
        </section>
    );
}

export function runtimeLabel(runtime) {
    return ({ python: 'Python', java: 'Java/JVM', node: 'Node.js', go: 'Go', native: 'Native', kernel: 'Kernel', unknown: 'Unknown' })[runtime] || runtime;
}

// 阶段八：存储来源与分辨率展示（前端统一使用 API 的 storage_source /
// resolution_seconds / mixed_resolution 字段，不再自行猜测）。
export function storageSourceLabel(source) {
    switch (String(source || '').toLowerCase()) {
        case 'parquet_v2':
            return 'Parquet v2';
        case 'parquet_v1':
            return 'Parquet v1（热窗口）';
        case 'v1':
            return 'v1 热窗口';
        default:
            return source || 'Parquet v1（热窗口）';
    }
}

export function resolutionLabel(seconds) {
    const value = Number(seconds) || 0;
    if (value <= 0) return '-';
    if (value < 60) return `${value}s`;
    if (value % 3600 === 0) return `${value / 3600}h`;
    if (value % 60 === 0) return `${value / 60}m`;
    return `${value}s`;
}

function Field({ label, children, wide = false }) {
    return <label style={wide ? S.fieldWide : S.field}><span style={S.label}>{label}</span>{children}</label>;
}

function Metric({ label, value }) {
    return <div style={S.metric}><div style={S.metricLabel}>{label}</div><div style={S.metricValue}>{value || '-'}</div></div>;
}

function continuousSessionMeta(target, fixedSession = null) {
	const raw = fixedSession || target?.continuous_session || {};
	const caps = decodeJSONField(raw.capabilities, {});
    const signals = decodeJSONField(raw.signals, ['cpu_profile']).filter(Boolean);
    // labels 是 Go []byte jsonb 字段，序列化成 base64 字符串——decodeJSONField
    // 已经处理了这个 gotcha（先试直接 JSON.parse，失败再 atob 解一层）。
    const labels = decodeJSONField(raw.labels, {});
    if (Array.isArray(labels.db_targets) && labels.db_targets.length > 0) {
        signals.push('db_snapshot');
    }
    return {
        sid: raw.sid || '',
        name: raw.name || 'Native Continuous Profiling',
		status: raw.observed_state || raw.status || target?.profile_status || 'unknown',
		sampler: raw.sampler || caps.sampler || (raw.continuity_mode === 'degraded' ? 'perf/bpftrace fallback' : 'perf_event'),
        sampleRateHz: numberOrDefault(raw.sample_rate_hz, 19),
        aggregationWindowSec: numberOrDefault(raw.aggregation_window_sec, 10),
        uploadBatchSec: numberOrDefault(raw.upload_batch_sec, 60),
        retentionHours: numberOrDefault(raw.retention_hours, 24),
        lastUploadAt: raw.last_upload_at || target?.last_profile_at || '',
        startedAt: raw.started_at || '',
        stoppedAt: raw.stopped_at || '',
        capabilities: caps,
        signals: signals.length ? signals : ['cpu_profile'],
    };
}

export function signalTabsForSession(signalKey) {
    const signals = new Set(String(signalKey || 'cpu_profile').split('|').filter(Boolean));
    if (signals.size === 0) signals.add('cpu_profile');
    const tabs = SIGNAL_TAB_OPTIONS.filter(option => signals.has(option.signal));
    // 数据库快照是独立采集器（DBSnapshotSampler 不依赖 perf/eBPF signals），
    // 只要 Session 存在就始终展示数据库 tab，方便查看 digest/锁等待链。
    const dbTab = SIGNAL_TAB_OPTIONS.find(o => o.tab === 'db');
    if (dbTab && !tabs.some(t => t.tab === 'db')) tabs.push(dbTab);
    return tabs.length ? tabs : [SIGNAL_TAB_OPTIONS[0]];
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

export function TopNTable({ data, loading, profileURL, filterText = '' }) {
    if (loading && !data) return <div style={S.empty}>正在查询 TopN...</div>;
    const items = data?.items || [];
    if (data?.empty || items.length === 0) {
        return <ProfileEmpty message={data?.message || (filterText ? `该时间范围内 ${filterText} 无样本` : '暂无热点函数')} url={data?.profile_url || profileURL} />;
    }
    return (
        <div className="table-scroll" style={S.tableWrap}>
            <table style={S.table}>
                <thead>
                    <tr>
                        <th style={{ ...S.th, width: '48%' }}>函数</th>
                        <th style={S.th}>{metricColumnLabel(data.unit, '累计')}</th>
                        <th style={S.th}>累计占比</th>
                        <th style={S.th}>{metricColumnLabel(data.unit, '自身')}</th>
                        <th style={S.th}>自身占比</th>
                    </tr>
                </thead>
                <tbody>
                    {items.slice(0, 20).map((item, index) => (
                        <tr key={`${item.name}-${index}`}>
                            <td style={{ ...S.td, ...(item.unresolved ? S.tdMuted : {}) }} title={item.name}>
                                {truncate(item.display_name || item.name, 72)}
                            </td>
                            <td style={S.td}>{formatMetricValue(item.value, item.unit || data.unit)}</td>
                            <td style={S.td}>{formatPercent(item.percent)}</td>
                            <td style={S.td}>{formatMetricValue(item.self, item.unit || data.unit)}</td>
                            <td style={S.td}>{formatPercent(item.self_percent)}</td>
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

export function HistogramPanel({ data, loading, title, targetIP, signal, timeWindow }) {
    const [events, setEvents] = useState([]);
    const supportsSentinel = SENTINEL_SIGNALS.includes(signal);

    useEffect(() => {
        if (!supportsSentinel || !targetIP || !timeWindow?.from || !timeWindow?.to) { setEvents([]); return; }
        let cancelled = false;
        sentinelRules.events({ target_ip: targetIP, signal, from: timeWindow.from, to: timeWindow.to })
            .then(response => { if (!cancelled && response.code === 0) setEvents(response.data?.events || []); })
            .catch(() => { if (!cancelled) setEvents([]); });
        return () => { cancelled = true; };
    }, [supportsSentinel, targetIP, signal, timeWindow?.from, timeWindow?.to]);

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
                <span style={S.subtle}>总事件 {formatEventCount(data?.event_count || 0)}</span>
            </div>
            {data?.empty || buckets.length === 0 ? (
                <ProfileEmpty message={data?.message || `${title} 暂无 histogram 样本`} url={data?.profile_url} />
            ) : (
                <>
                    <div style={S.summaryGrid}>
                        <Metric label="P50" value={formatLatency(summary.p50, data?.unit)} />
                        <Metric label="P95" value={formatLatency(summary.p95, data?.unit)} />
                        <Metric label="P99" value={formatLatency(summary.p99, data?.unit)} />
                        <Metric label="事件数" value={formatEventCount(data?.event_count || 0)} />
                    </div>
                    <div className="table-scroll" style={{ ...S.tableWrap, marginTop: 14 }}>
                        <table style={S.table}>
                            <thead>
                                <tr>
                                    <th style={{ ...S.th, width: 170 }}>延迟桶</th>
                                    <th style={S.th}>分布</th>
                                    <th style={{ ...S.th, width: 140 }}>事件数</th>
                                </tr>
                            </thead>
                            <tbody>
                                {buckets.map((bucket, index) => (
                                    <tr key={`${bucket.range}-${index}`}>
                                        <td style={{ ...S.td, ...S.histogramBucketCell }}>{bucket.range}</td>
                                        <td style={{ ...S.td, ...S.histogramBarCell }}>
                                            <div style={S.barWithLabel}>
                                                <div style={S.barTrack} data-testid="histogram-bar-track">
                                                    <div
                                                        data-testid="histogram-bar"
                                                        style={{ ...S.bar, width: `${Math.max(3, (Number(bucket.count) || 0) / maxCount * 100)}%`, background: '#12b76a' }}
                                                    />
                                                </div>
                                                <span style={S.barPercent}>{formatPercent((Number(bucket.count) || 0) / maxCount * 100)}</span>
                                            </div>
                                        </td>
                                        <td style={{ ...S.td, ...S.histogramCountCell }}>{formatEventCount(bucket.count)}</td>
                                    </tr>
                                ))}
                            </tbody>
                        </table>
                    </div>
                    <div style={{ marginTop: 16 }}>
                        <div style={S.sectionHead}>
                            <h3 style={S.title}>P95/P99 趋势</h3>
                            <span style={S.subtle}>{trend.length} 个窗口{supportsSentinel ? '；红点为哨兵触发点，悬停查看详情，点击跳转诊断结果' : ''}</span>
                        </div>
                        {supportsSentinel && <HistogramTrendChart trend={trend} events={events} unit={data?.unit} metric="p99" />}
                        <div className="table-scroll" style={S.tableWrap}>
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
                                            <td style={{ ...S.td, ...S.tdNowrap }}>{formatEventCount(point.event_count || 0)}</td>
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

// db_questions_total 是累计计数器（自数据库启动以来的总请求数），直接画出
// 来看不出速率变化——QPS 骤降场景要看的是斜率，不是绝对值。这里在前端把相
// 邻两个窗口的差值除以时间差换算成瞬时 qps，和 Prometheus rate() 处理累计
// 计数器是同一个思路。
function computeQPS(points) {
    const out = [];
    for (let i = 1; i < points.length; i++) {
        const dtSec = (new Date(points[i].timestamp) - new Date(points[i - 1].timestamp)) / 1000;
        const dv = Number(points[i].value) - Number(points[i - 1].value);
        out.push({ timestamp: points[i].timestamp, value: dtSec > 0 && dv >= 0 ? dv / dtSec : 0 });
    }
    return out;
}

function Sparkline({ points, color, unit, digits = 0 }) {
    if (!points || points.length === 0) return <div style={S.empty}>暂无数据</div>;
    const width = 200, height = 60, pad = 6;
    const values = points.map(p => Number(p.value) || 0);
    const max = Math.max(...values), min = Math.min(...values);
    const range = (max - min) || 1;
    const step = values.length > 1 ? (width - pad * 2) / (values.length - 1) : 0;
    const coords = values.map((v, i) => {
        const x = pad + i * step;
        const y = height - pad - ((v - min) / range) * (height - pad * 2);
        return `${x.toFixed(1)},${y.toFixed(1)}`;
    }).join(' ');
    const last = values[values.length - 1];
    return (
        <div>
            <div style={{ fontSize: 18, fontWeight: 700, color: '#101828' }}>
                {last.toFixed(digits)}<span style={{ fontSize: 12, color: '#667085', fontWeight: 400 }}> {unit}</span>
            </div>
            <svg viewBox={`0 0 ${width} ${height}`} style={{ width: '100%', height, display: 'block', marginTop: 6 }}>
                <polyline points={coords} fill="none" stroke={color} strokeWidth="2" strokeLinejoin="round" strokeLinecap="round" />
            </svg>
        </div>
    );
}

// 只有 db_active_connections/db_questions_total/db_innodb_buffer_pool_hit_ratio_bps
// 三个信号一直没有界面能看——它们走 window.metrics 不是 window.dbSnapshots，
// 之前 queryNativeContinuousDBSnapshot 完全没有扫描/返回过这部分数据。同名指标
// 可能来自多个数据库实例（runtime 不同），这里简化处理只取第一个匹配到的
// runtime，多实例场景后续再拆分单独展示。
function DBMetricTrends({ metrics }) {
    const conn = metrics.find(m => m.metric === 'db_active_connections');
    const questions = metrics.find(m => m.metric === 'db_questions_total');
    const hitRatio = metrics.find(m => m.metric === 'db_innodb_buffer_pool_hit_ratio_bps');
    const qpsPoints = questions ? computeQPS(questions.points || []) : [];
    // 命中率存成整数（0~10000 代表 0%~100%，见 ContinuousSampler.cpp 的换算注释），这里换算回百分比。
    const hitRatioPoints = hitRatio ? (hitRatio.points || []).map(p => ({ timestamp: p.timestamp, value: Number(p.value) / 100 })) : [];
    return (
        <div style={{ marginTop: 16 }}>
            <div style={S.sectionHead}>
                <h3 style={S.title}>标量指标趋势</h3>
                <span style={S.subtle}>连接数 / QPS（相邻窗口增量换算）/ 缓冲池命中率</span>
            </div>
            <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(180px, 1fr))', gap: 12, marginTop: 8 }}>
                <div style={{ background: '#f8fafc', borderRadius: 8, padding: 12 }}>
                    <div style={S.subtle}>连接数</div>
                    <Sparkline points={conn?.points} color="#315efb" unit="" />
                </div>
                <div style={{ background: '#f8fafc', borderRadius: 8, padding: 12 }}>
                    <div style={S.subtle}>QPS</div>
                    <Sparkline points={qpsPoints} color="#d85a30" unit="qps" />
                </div>
                <div style={{ background: '#f8fafc', borderRadius: 8, padding: 12 }}>
                    <div style={S.subtle}>缓冲池命中率</div>
                    <Sparkline points={hitRatioPoints} color="#1d9e75" unit="%" digits={1} />
                </div>
            </div>
        </div>
    );
}

// "运行测试场景"：demo/验收场景下，与其让人手填 host/port/user/password_ref
// 一堆字段（容易在密码文件格式上翻车），不如把已经在 VM 上验证过的固定配置
// 包成一键应用的卡片——应用只是复用现有的 updateLabels 写 db_targets，
// 不新增写路径；真正的故障注入命令仍由人在终端里跑，不做"前端一键触发远程
// SQL"，那属于新增攻击面，不值得为演示引入。
function TestScenarioCard({ sid, sessionLabels }) {
    const [scenarios, setScenarios] = useState([]);
    const [applying, setApplying] = useState('');
    const [appliedId, setAppliedId] = useState('');
    const [error, setError] = useState('');

    useEffect(() => {
        let cancelled = false;
        continuous.dbTestScenarios()
            .then(response => { if (!cancelled && response.code === 0) setScenarios(response.data?.scenarios || []); })
            .catch(() => { if (!cancelled) setScenarios([]); });
        return () => { cancelled = true; };
    }, []);

    if (scenarios.length === 0) return null;

    const apply = async scenario => {
        if (!sid || applying) return;
        setApplying(scenario.id);
        setError('');
        try {
            const response = await continuous.updateLabels(sid, { ...sessionLabels, db_targets: [scenario.target] });
            if (response.code !== 0) throw new Error(response.message || '应用失败');
            setAppliedId(scenario.id);
        } catch (err) {
            setError(err?.response?.data?.message || err?.message || '应用失败');
        } finally {
            setApplying('');
        }
    };

    const applied = scenarios.find(item => item.id === appliedId);

    return (
        <div style={{ background: '#fff', border: '1px solid #e5e7eb', borderRadius: 8, padding: 16, marginBottom: 14 }}>
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 12, flexWrap: 'wrap', gap: 8 }}>
                <span style={{ fontSize: 13, fontWeight: 700, color: '#344054' }}>运行测试场景</span>
                <span style={{ fontSize: 11, color: '#98a2b3' }}>应用到当前会话，已在 VM 验证可用</span>
            </div>
            {error && <div style={{ marginBottom: 10, color: '#b42318', background: '#fff6f5', border: '1px solid #fda29b', borderRadius: 6, padding: 8, fontSize: 12 }}>{error}</div>}
            <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(220px, 1fr))', gap: 10 }}>
                {scenarios.map(scenario => (
                    <div key={scenario.id} style={{ border: '1px solid #e5e7eb', borderRadius: 8, padding: 12 }}>
                        <div style={{ fontSize: 13, fontWeight: 700, marginBottom: 4 }}>{scenario.title}</div>
                        <p style={{ margin: '0 0 10px', fontSize: 12, color: '#667085', lineHeight: 1.5 }}>{scenario.description}</p>
                        <button
                            type="button"
                            disabled={!sid || applying === scenario.id}
                            onClick={() => apply(scenario)}
                            style={{ background: '#315efb', color: '#fff', border: 0, borderRadius: 6, padding: '6px 12px', fontSize: 12, fontWeight: 700, cursor: !sid || applying === scenario.id ? 'not-allowed' : 'pointer', opacity: !sid ? 0.6 : 1 }}
                        >
                            {applying === scenario.id ? '应用中...' : '应用'}
                        </button>
                    </div>
                ))}
            </div>
            {applied && (
                <div style={{ marginTop: 10, background: '#f8fafc', border: '1px solid #e5e7eb', borderRadius: 6, padding: 10 }}>
                    <div style={{ fontSize: 11, color: '#067647', fontWeight: 700, marginBottom: 6 }}>
                        已应用「{applied.title}」，现场执行下方命令注入故障
                    </div>
                    <pre style={{ margin: 0, fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace', fontSize: 11, color: '#344054', background: '#fff', border: '1px solid #eaecf0', borderRadius: 4, padding: 8, overflowX: 'auto', whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>{applied.inject_hint}</pre>
                </div>
            )}
        </div>
    );
}

// 锁等待链单独成表而不是并进 digest 表：digest 是"这段时间哪些 SQL 累计
// 最慢"（聚合量），锁等待是"某一时刻谁在等谁"（时点事实），两者聚合口径不
// 同，合并展示会让人误以为锁等待也是累计值。
export function DBSnapshotPanel({ data, loading, targetIP, timeWindow, sid, sessionLabels }) {
    const [events, setEvents] = useState([]);

    useEffect(() => {
        if (!targetIP || !timeWindow?.from || !timeWindow?.to) { setEvents([]); return; }
        let cancelled = false;
        sentinelRules.events({ target_ip: targetIP, signal: 'db_snapshot', from: timeWindow.from, to: timeWindow.to })
            .then(response => { if (!cancelled && response.code === 0) setEvents(response.data?.events || []); })
            .catch(() => { if (!cancelled) setEvents([]); });
        return () => { cancelled = true; };
    }, [targetIP, timeWindow?.from, timeWindow?.to]);

    if (loading && !data) return (
        <>
            <TestScenarioCard sid={sid} sessionLabels={sessionLabels} />
            <section style={S.card}><div style={S.empty}>正在查询数据库快照...</div></section>
        </>
    );
    const digests = data?.digests || [];
    const lockWaits = data?.lock_waits || [];
    const deadlocks = data?.deadlocks || [];
    const firedEvents = events.filter(e => e.status === 'fired_no_action');
    return (
        <>
        <TestScenarioCard sid={sid} sessionLabels={sessionLabels} />
        <section style={S.card}>
            <div style={S.sectionHead}>
                <div>
                    <h3 style={S.title}>数据库快照</h3>
                    <div style={S.subtle}>{data?.source || 'mini-drop-native'} · 数据库系统视图轮询</div>
                </div>
                <span style={S.subtle}>{digests.length} 条 digest · {lockWaits.length} 条锁等待{deadlocks.length > 0 ? ` · ${deadlocks.length} 起死锁` : ''}</span>
            </div>
            {firedEvents.length > 0 && <DBSentinelEvents events={firedEvents} />}
            {data?.empty || (digests.length === 0 && lockWaits.length === 0 && deadlocks.length === 0 && !(data?.metrics?.length > 0)) ? (
                <ProfileEmpty message={data?.message || '该时间范围暂无数据库快照数据'} url={data?.profile_url} />
            ) : (
                <>
                    <div style={S.summaryGrid}>
                        <Metric label="慢查询 digest" value={String(digests.length)} />
                        <Metric label="锁等待事件" value={String(lockWaits.length)} />
                        <Metric label="最长锁等待" value={lockWaits.length ? `${Math.max(...lockWaits.map(w => Number(w.wait_seconds) || 0))} s` : '-'} />
                    </div>
                    {data?.metrics?.length > 0 && <DBMetricTrends metrics={data.metrics} />}
                    {deadlocks.length > 0 && (
                        <div style={{ marginTop: 16 }}>
                            <div style={S.sectionHead}>
                                <h3 style={S.title}>死锁记录</h3>
                                <span style={S.subtle}>按最近发生时间排序 · 原始 InnoDB 状态报告，未做结构化解析</span>
                            </div>
                            {deadlocks.map((item, index) => (
                                <details key={`${item.instance_label}-${index}`} style={{ marginTop: index === 0 ? 0 : 8, border: '1px solid #fda29b', borderRadius: 6, padding: '8px 12px', background: '#fff6f5' }}>
                                    <summary style={{ cursor: 'pointer', fontWeight: 700, color: '#b42318', fontSize: 13 }}>
                                        {formatTime(item.timestamp)} · 实例 {item.instance_label || '-'}
                                    </summary>
                                    <pre style={{ marginTop: 8, whiteSpace: 'pre-wrap', wordBreak: 'break-all', fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace', fontSize: 12, color: '#344054' }}>{item.report}</pre>
                                </details>
                            ))}
                        </div>
                    )}
                    <div style={{ marginTop: 16 }}>
                        <div style={S.sectionHead}>
                            <h3 style={S.title}>锁等待链</h3>
                            <span style={S.subtle}>按等待时长排序</span>
                        </div>
                        {lockWaits.length === 0 ? (
                            <div style={S.empty}>该时间范围内没有观测到锁等待</div>
                        ) : (
                            <div className="table-scroll" style={S.tableWrap}>
                                <table style={S.table}>
                                    <thead>
                                        <tr>
                                            <th style={S.th}>时间</th>
                                            <th style={S.th}>实例</th>
                                            <th style={S.th}>等待时长</th>
                                            <th style={S.th}>锁定表</th>
                                            <th style={{ ...S.th, width: '26%' }}>被阻塞 SQL</th>
                                            <th style={{ ...S.th, width: '26%' }}>阻塞方 SQL</th>
                                        </tr>
                                    </thead>
                                    <tbody>
                                        {lockWaits.slice(0, 50).map((item, index) => (
                                            <tr key={`${item.waiting_pid}-${item.blocking_pid}-${index}`}>
                                                <td style={S.td}>{formatTime(item.timestamp)}</td>
                                                <td style={S.td}>{item.instance_label || '-'}</td>
                                                <td style={S.td}>{item.wait_seconds} s</td>
                                                <td style={S.td}>{item.locked_table || '-'}</td>
                                                <td style={S.td} title={item.waiting_query}>
                                                    <div>pid {item.waiting_pid}</div>
                                                    {truncate(item.waiting_query || '-', 60)}
                                                </td>
                                                <td style={S.td} title={item.blocking_query}>
                                                    <div>pid {item.blocking_pid}</div>
                                                    {truncate(item.blocking_query || '-', 60)}
                                                </td>
                                            </tr>
                                        ))}
                                    </tbody>
                                </table>
                            </div>
                        )}
                    </div>
                    <div style={{ marginTop: 16 }}>
                        <div style={S.sectionHead}>
                            <h3 style={S.title}>慢查询 digest</h3>
                            <span style={S.subtle}>按窗口内累计耗时排序</span>
                        </div>
                        {digests.length === 0 ? (
                            <div style={S.empty}>该时间范围内没有采到 SQL digest</div>
                        ) : (
                            <div className="table-scroll" style={S.tableWrap}>
                                <table style={S.table}>
                                    <thead>
                                        <tr>
                                            <th style={{ ...S.th, width: '44%' }}>SQL digest</th>
                                            <th style={S.th}>实例 / schema</th>
                                            <th style={S.th}>调用次数</th>
                                            <th style={S.th}>累计耗时</th>
                                            <th style={S.th}>平均耗时</th>
                                            <th style={S.th}>扫描行数</th>
                                        </tr>
                                    </thead>
                                    <tbody>
                                        {digests.map((item, index) => (
                                            <tr key={`${item.digest_text}-${index}`}>
                                                <td style={S.td} title={item.digest_text}>{truncate(item.digest_text || '-', 90)}</td>
                                                <td style={S.td}>{item.instance_label || '-'} / {item.schema_name || '-'}</td>
                                                <td style={S.td}>{formatMetricValue(item.call_count || 0, 'samples')}</td>
                                                <td style={S.td}>{formatLatency(item.total_latency_us, 'us')}</td>
                                                <td style={S.td}>{formatLatency(item.avg_latency_us, 'us')}</td>
                                                <td style={S.td}>{formatMetricValue(item.rows_examined || 0, 'samples')}</td>
                                            </tr>
                                        ))}
                                    </tbody>
                                </table>
                            </div>
                        )}
                    </div>
                </>
            )}
        </section>
        </>
    );
}

// DBSentinelEvents 数据库Tab的"近期哨兵触发"列表。db_snapshot 命中后端写的是
// status=fired_no_action、child_tid 永远为空（script_diagnostic 的 Runner 还没接入，
// 见 apiserver/server/detection.go 里 evaluateDBSnapshotRule 的注释）——HistogramTrendChart
// 那套"红点可点击跳转诊断任务"的交互在这里不成立，做成可点击链接会点进一个不存在的
// 任务，是误导性 UI，所以这里只展示纯文案，不接 onClick/Link。
function DBSentinelEvents({ events }) {
    return (
        <div style={{ marginBottom: 16 }}>
            <div style={S.sectionHead}>
                <h3 style={S.title}>近期哨兵触发</h3>
                <span style={S.subtle}>{events.length} 条</span>
            </div>
            <div style={{ display: 'grid', gap: 6 }}>
                {events.slice(0, 20).map((event, index) => (
                    <div key={`${event.rule_sid}-${index}`} style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '10px 14px', border: '1px solid #e5e7eb', borderRadius: 6, background: '#f9fafb' }}>
                        <div>
                            <div style={{ fontSize: 13, color: '#101828' }}>
                                {event.metric === 'lock_wait' ? '锁等待' : event.metric === 'digest' ? '慢SQL环比' : event.metric} · {formatTime(event.evaluated_at)}
                            </div>
                            <div style={S.subtle}>
                                {event.metric === 'lock_wait'
                                    ? `等待 ${event.observed_value} s（阈值 ${event.floor_value} s）`
                                    : `当前耗时 ${formatLatency(event.observed_value, 'us')}（下限 ${formatLatency(event.floor_value, 'us')}）`}
                            </div>
                        </div>
                        <span style={{ fontSize: 12, color: '#98a2b3' }}>已记录异常，当前无自动诊断</span>
                    </div>
                ))}
            </div>
        </div>
    );
}

function sampleStateForTarget(target, flamegraph, topn, fixedSession = null) {
	const status = String(fixedSession?.observed_state || target?.profile_status || 'unknown');
	if (status === 'degraded') return '降级运行';
	if (status === 'waiting') return '等待进程';
	if (status === 'pending') return '待启动';
	if (status === 'stopping') return '停止中';
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
	if (signal === 'io_syscall') return caps.io_syscall_backend || caps.io_backend || '';
    if (signal === 'sched') return caps.sched_backend || '';
    return '';
}

export function instanceFilters(value) {
	const [pid, processStartMs] = String(value || '').split('|');
	return pid && processStartMs ? { pid, process_start_ms: processStartMs } : {};
}

export function processInstanceOptions(activeProcesses, historicalValues) {
	const instances = new Map();
	(activeProcesses || []).forEach(process => {
		const pid = Number(process?.pid);
		const processStartMs = Number(process?.process_start_ms);
		if (!Number.isInteger(pid) || pid <= 0 || !Number.isFinite(processStartMs) || processStartMs <= 0) return;
		const value = `${pid}|${processStartMs}`;
		instances.set(value, { value, pid, processStartMs, active: true });
	});
	(historicalValues || []).forEach(raw => {
		const value = String(raw || '');
		const [pidText, startText] = value.split('|');
		const pid = Number(pidText);
		const processStartMs = Number(startText);
		if (!Number.isInteger(pid) || pid <= 0 || !Number.isFinite(processStartMs) || processStartMs <= 0 || instances.has(value)) return;
		instances.set(value, { value, pid, processStartMs, active: false });
	});
	return Array.from(instances.values()).sort((left, right) => Number(right.active) - Number(left.active)
		|| right.processStartMs - left.processStartMs || left.pid - right.pid);
}

export function sampledProcessInstanceOptions(activeProcesses, sampledValues) {
    const activeKeys = new Set((activeProcesses || []).map(process => `${process?.pid}|${process?.process_start_ms}`));
    return (sampledValues || []).map(raw => {
        const value = String(raw || '');
        const [pidText, startText] = value.split('|');
        const pid = Number(pidText);
        const processStartMs = Number(startText);
        if (!Number.isInteger(pid) || pid <= 0 || !Number.isFinite(processStartMs) || processStartMs <= 0) return null;
        return { value, pid, processStartMs, active: activeKeys.has(value) };
    }).filter(Boolean).sort((left, right) => Number(right.active) - Number(left.active)
        || right.processStartMs - left.processStartMs || left.pid - right.pid);
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

export function formatDiagnosticJSON(value) {
    const seen = new WeakSet();
    try {
        const formatted = JSON.stringify(value === undefined ? null : value, (key, nestedValue) => {
            if (typeof nestedValue === 'bigint') return String(nestedValue);
            if (nestedValue && typeof nestedValue === 'object') {
                if (seen.has(nestedValue)) return '[Circular]';
                seen.add(nestedValue);
            }
            return nestedValue;
        }, 2);
        return formatted === undefined ? String(value) : formatted;
    } catch (error) {
        try {
            return String(value);
        } catch (stringError) {
            return '[Unserializable value]';
        }
    }
}

function diagnosticJSONField(label, value) {
    return `${label}:\n${formatDiagnosticJSON(value)}`;
}

export function diagnosticText({ target, flamegraph, topn, timeWindow, profileType, stackScope = 'all', filters = {} } = {}) {
    return [
        `target: ${target?.id || '-'}`,
        `profile_type: ${profileType || '-'}`,
        `stack_scope: ${stackScope}`,
        diagnosticJSONField('continuous_session', target?.continuous_session || {}),
        `time_range: ${timeWindow?.from || '-'} -> ${timeWindow?.to || '-'}`,
        `selector: ${labelSelectorForTarget(target, filters)}`,
        diagnosticJSONField('filters', filters || {}),
        `backend: ${flamegraph?.backend || topn?.backend || '-'}`,
        `query: ${flamegraph?.query || topn?.query || '-'}`,
        `unit: ${flamegraph?.unit || topn?.unit || '-'}`,
        `total_raw_value: ${formatRawMetric(flamegraph?.total || topn?.total || 0, flamegraph?.unit || topn?.unit || '')}`,
        `profile_url: ${flamegraph?.profile_url || topn?.profile_url || target?.profile_url || '-'}`,
        `raw_profile_url_debug_only: ${flamegraph?.raw_profile_url || topn?.raw_profile_url || '-'}`,
        `symbol_status: ${flamegraph?.symbol_status || topn?.symbol_status || 'not_applicable'}`,
        diagnosticJSONField('symbol_diagnostics', flamegraph?.symbol_diagnostics || topn?.symbol_diagnostics || {}),
        diagnosticJSONField('runtime_diagnostics', flamegraph?.runtime_diagnostics || topn?.runtime_diagnostics || {}),
        `truncated: ${flamegraph?.truncated || topn?.truncated || false}`,
    ].join('\n');
}

function symbolStatusLabel(status, diagnostics = {}) {
    const percent = Number(diagnostics?.unresolved_percent || 0);
    const unresolved = Number(diagnostics?.unresolved_frame_weight || 0);
    const totalWeight = Number(diagnostics?.total_frame_weight || 0);
    const moduleUnresolved = Number(diagnostics?.module_unresolved_frame_weight || 0);
    const noModule = Number(diagnostics?.no_module_frame_weight || 0);
    const goState = diagnostics?.go_symbol_state;
    const suffix = goState === 'pending'
        ? ' · Go 符号正在后台预热'
        : goState === 'failed'
            ? ` · Go 符号提取失败${diagnostics?.reasons?.length ? `：${diagnostics.reasons[0]}` : ''}`
            : '';
    // 未解析帧拆两类：模块未解析（符号库可补 build-id）与无模块（疑似 JIT/
    // 匿名内存，本质无解），成因和修法不同，混称"裸地址帧"会误导。
    // 仅在 total_weight 存在时展示占比，避免符号存储检查（空 diagnostics）
    // 场景下误报 0%。
    const hasBreakdown = totalWeight > 0;
    const modulePct = hasBreakdown ? moduleUnresolved * 100 / totalWeight : 0;
    const noModulePct = hasBreakdown ? noModule * 100 / totalWeight : 0;
    switch (status) {
        case 'complete': return `完整（当前范围未检测到未解析帧）${suffix}`;
        case 'partial':
            return hasBreakdown
                ? `部分解析（未解析 ${percent.toFixed(1)}%：模块未解析 ${modulePct.toFixed(1)}%、无模块 ${noModulePct.toFixed(1)}%，权重 ${formatNum(unresolved)}）${suffix}`
                : `部分解析${suffix}`;
        case 'missing':
            return hasBreakdown
                ? `缺失（未解析权重 ${formatNum(unresolved)}：模块未解析 ${modulePct.toFixed(1)}%、无模块 ${noModulePct.toFixed(1)}%）${suffix}`
                : `缺失${suffix}`;
        case 'not_applicable': return '不适用';
        default: return status || '未知';
    }
}

function formatNum(n) {
    if (n === null || n === undefined) return '-';
    if (typeof n !== 'number' || !isFinite(n)) return String(n);
    if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
    if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
    if (n >= 1e3) return (n / 1e3).toFixed(2) + 'K';
    return String(n);
}

export function makeTimeWindow(range, now = new Date()) {
    const to = new Date(now);
    const minutes = rangeMinutes(range) || 30;
    const from = new Date(to.getTime() - minutes * 60 * 1000);
    return { from: from.toISOString(), to: to.toISOString() };
}

export function makeSequentialDiffWindows(range, now = new Date()) {
    const minutes = rangeMinutes(range) || 15;
    const compareTo = new Date(now);
    const compareFrom = new Date(compareTo.getTime() - minutes * 60 * 1000);
    const baseTo = compareFrom;
    const baseFrom = new Date(baseTo.getTime() - minutes * 60 * 1000);
    return {
        baseWindow: { from: baseFrom.toISOString(), to: baseTo.toISOString() },
        compareWindow: { from: compareFrom.toISOString(), to: compareTo.toISOString() },
    };
}

export function retentionBounds(retentionHours = 24, nowValue = new Date()) {
    const now = new Date(nowValue);
    const retentionMs = Math.max(1, Number(retentionHours) || 24) * 60 * 60 * 1000;
    return { from: new Date(now.getTime() - retentionMs), to: now };
}

export function customInputsToSlider(fromInput, toInput, retentionHours = 24, nowValue = new Date()) {
    const bounds = retentionBounds(retentionHours, nowValue);
    const maxMinute = Math.max(1, Math.round((bounds.to.getTime() - bounds.from.getTime()) / 60000));
    const fallbackTo = maxMinute;
    const fallbackFrom = Math.max(0, fallbackTo - 30);
    let fromMinute = localInputToMinuteOffset(fromInput, bounds);
    let toMinute = localInputToMinuteOffset(toInput, bounds);
    if (!Number.isFinite(fromMinute)) fromMinute = fallbackFrom;
    if (!Number.isFinite(toMinute)) toMinute = fallbackTo;
    fromMinute = clampNumber(Math.round(fromMinute), 0, maxMinute - 1);
    toMinute = clampNumber(Math.round(toMinute), fromMinute + 1, maxMinute);
    return { bounds, maxMinute, fromMinute, toMinute, now: bounds.to };
}

export function sliderMinutesToInputs(fromMinute, toMinute, retentionHours = 24, nowValue = new Date()) {
    const bounds = retentionBounds(retentionHours, nowValue);
    const maxMinute = Math.max(1, Math.round((bounds.to.getTime() - bounds.from.getTime()) / 60000));
    const safeFrom = clampNumber(Math.round(Number(fromMinute) || 0), 0, maxMinute - 1);
    const safeTo = clampNumber(Math.round(Number(toMinute) || 0), safeFrom + 1, maxMinute);
    return {
        fromInput: toLocalDateTimeInput(new Date(bounds.from.getTime() + safeFrom * 60000).toISOString()),
        toInput: toLocalDateTimeInput(new Date(bounds.from.getTime() + safeTo * 60000).toISOString()),
    };
}

export function sequentialDiffWindowsFromStart(startMinute, durationMinutes, bounds) {
    const duration = Math.max(1, Math.round(Number(durationMinutes) || 15));
    const maxMinute = Math.max(duration * 2, Math.round((bounds.to.getTime() - bounds.from.getTime()) / 60000));
    const start = clampNumber(Math.round(Number(startMinute) || 0), 0, Math.max(0, maxMinute - duration * 2));
    const baseFrom = new Date(bounds.from.getTime() + start * 60000);
    const baseTo = new Date(baseFrom.getTime() + duration * 60000);
    const compareFrom = baseTo;
    const compareTo = new Date(compareFrom.getTime() + duration * 60000);
    return {
        baseWindow: { from: baseFrom.toISOString(), to: baseTo.toISOString() },
        compareWindow: { from: compareFrom.toISOString(), to: compareTo.toISOString() },
    };
}

export function sequentialDiffWindowMinutes(maxMinute, durationMinutes) {
    const duration = Math.max(1, Math.round(Number(durationMinutes) || 15));
    const span = Math.min(maxMinute, duration * 2);
    const toMinute = maxMinute;
    return { fromMinute: Math.max(0, toMinute - span), toMinute };
}

export function diffWindowsFromMinutes(fromMinute, toMinute, bounds) {
    const maxMinute = Math.max(2, Math.round((bounds.to.getTime() - bounds.from.getTime()) / 60000));
    const normalized = normalizeEvenSpan(fromMinute, toMinute, maxMinute);
    const middle = normalized.fromMinute + ((normalized.toMinute - normalized.fromMinute) / 2);
    const baseFrom = new Date(bounds.from.getTime() + normalized.fromMinute * 60000);
    const baseTo = new Date(bounds.from.getTime() + middle * 60000);
    const compareFrom = baseTo;
    const compareTo = new Date(bounds.from.getTime() + normalized.toMinute * 60000);
    return {
        baseWindow: { from: baseFrom.toISOString(), to: baseTo.toISOString() },
        compareWindow: { from: compareFrom.toISOString(), to: compareTo.toISOString() },
    };
}

export function normalizeEvenSpan(fromMinute, toMinute, maxMinute) {
    let start = clampNumber(Math.round(Number(fromMinute) || 0), 0, Math.max(0, maxMinute - 2));
    let end = clampNumber(Math.round(Number(toMinute) || 0), start + 2, maxMinute);
    if ((end - start) % 2 !== 0) {
        if (end < maxMinute) end += 1;
        else if (start > 0) start -= 1;
    }
    if (end - start < 2) end = Math.min(maxMinute, start + 2);
    return { fromMinute: start, toMinute: end };
}

export function validateCustomTimeWindow(fromInput, toInput, retentionHours = 24, label = '', nowValue = new Date()) {
    const prefix = label ? `${label} ` : '';
    if (!fromInput || !toInput) return { error: `请选择${prefix}开始时间和结束时间` };
    const from = localDateTimeToISO(fromInput);
    const to = localDateTimeToISO(toInput);
    if (!from || !to) return { error: `${prefix}时间格式无效` };
    const fromDate = new Date(from);
    const toDate = new Date(to);
    if (fromDate >= toDate) return { error: `${prefix}结束时间必须晚于开始时间` };
    const now = new Date(nowValue);
    if (toDate > now) return { error: `${prefix}结束时间不能晚于当前时间` };
    const retentionMs = Math.max(1, Number(retentionHours) || 24) * 60 * 60 * 1000;
    const earliest = new Date(now.getTime() - retentionMs);
    if (fromDate < earliest) {
        return { error: `${prefix}开始时间不能早于数据保留边界 ${formatTime(earliest.toISOString())}` };
    }
    if (toDate.getTime() - fromDate.getTime() > retentionMs) {
        return { error: `${prefix}时间跨度不能超过 ${formatHours(retentionHours)}` };
    }
    return { window: { from, to }, error: '' };
}

export function rangeOptionsForRetention(retentionHours = 24, forDiff = false) {
    const retentionMinutes = Math.max(1, Number(retentionHours) || 24) * 60;
    return RANGE_OPTIONS.filter(([, , minutes]) => minutes * (forDiff ? 2 : 1) <= retentionMinutes);
}

export function coverageSegments(coverage, gaps = []) {
    const from = new Date(coverage?.from).getTime();
    const to = new Date(coverage?.to).getTime();
    const total = to - from;
    if (!Number.isFinite(total) || total <= 0) return [];
    const normalized = (gaps || []).map(gap => ({
        start: Math.max(from, new Date(gap.start).getTime()),
        end: Math.min(to, new Date(gap.end).getTime()),
    })).filter(gap => Number.isFinite(gap.start) && Number.isFinite(gap.end) && gap.start < gap.end)
        .sort((a, b) => a.start - b.start);
    const segments = [];
    let cursor = from;
    normalized.forEach(gap => {
        if (gap.start > cursor) segments.push({ type: 'covered', start: cursor, end: gap.start });
        if (gap.end > cursor) segments.push({ type: 'gap', start: Math.max(cursor, gap.start), end: gap.end });
        cursor = Math.max(cursor, gap.end);
    });
    if (cursor < to) segments.push({ type: 'covered', start: cursor, end: to });
    return segments.map(segment => ({ ...segment, percent: ((segment.end - segment.start) / total) * 100 }));
}

function formatClockOffset(value) {
    const milliseconds = Number(value);
    if (!Number.isFinite(milliseconds)) return '-';
    if (Math.abs(milliseconds) < 1000) return `${milliseconds} ms`;
    return `${(milliseconds / 1000).toFixed(2)} s`;
}

function formatGapDuration(value) {
    const seconds = Math.max(0, Number(value) || 0);
    if (seconds < 60) return `${seconds.toFixed(1)} 秒`;
    if (seconds < 3600) return `${(seconds / 60).toFixed(1)} 分钟`;
    return `${(seconds / 3600).toFixed(1)} 小时`;
}

function rangeMinutes(range) {
    return RANGE_OPTIONS.find(([value]) => value === range)?.[2] || 0;
}

function toLocalDateTimeInput(value) {
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return '';
    const local = new Date(date.getTime() - date.getTimezoneOffset() * 60 * 1000);
    return local.toISOString().slice(0, 16);
}

function localInputToMinuteOffset(value, bounds) {
    const iso = localDateTimeToISO(value);
    if (!iso) return NaN;
    const time = new Date(iso).getTime();
    if (!Number.isFinite(time)) return NaN;
    return Math.round((time - bounds.from.getTime()) / 60000);
}

function clampNumber(value, min, max) {
    return Math.max(min, Math.min(max, value));
}

function minutePercent(value, maxMinute) {
    const max = Math.max(1, Number(maxMinute) || 1);
    return clampNumber((Number(value) || 0) / max * 100, 0, 100);
}

function formatSearchPercent(value) {
    const num = Number(value);
    if (!Number.isFinite(num)) return '0%';
    return `${num >= 10 ? num.toFixed(1) : num.toFixed(2)}%`;
}

function formatPercent(value) {
    const num = Number(value);
    if (!Number.isFinite(num)) return '-';
    return `${num >= 10 ? num.toFixed(1) : num.toFixed(2)}%`;
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
