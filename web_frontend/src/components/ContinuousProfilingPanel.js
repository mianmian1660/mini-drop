import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { continuous, profiles } from '../api';
import InteractiveFlamegraph, { countProfileNodes } from './InteractiveFlamegraph';
import { localDateTimeToISO } from '../utils/time';
import { decodeJSONField } from '../utils/continuous';
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
    customRange: { display: 'flex', gap: 10, alignItems: 'end', flexWrap: 'wrap', marginTop: 10, paddingTop: 10, borderTop: '1px solid #eef2f6' },
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
    coverage: { marginTop: 14, borderTop: '1px solid #eaecf0', paddingTop: 12 },
    coverageBar: { display: 'flex', width: '100%', height: 14, overflow: 'hidden', borderRadius: 4, background: '#f2f4f7', border: '1px solid #d0d5dd' },
    coverageOK: { height: '100%', background: '#12b76a' },
    coverageGap: { height: '100%', background: '#d92d20', minWidth: 2 },
    gapList: { display: 'grid', gap: 5, marginTop: 8, color: '#b42318', fontSize: 12 },
    tableWrap: { overflowX: 'auto' },
    table: { width: '100%', borderCollapse: 'collapse' },
    th: { textAlign: 'left', padding: '9px 10px', borderBottom: '1px solid #eaecf0', color: '#475467', fontSize: 12, background: '#fff' },
    td: { padding: '8px 10px', borderBottom: '1px solid #f2f4f7', fontSize: 13, verticalAlign: 'top', lineHeight: 1.45 },
    details: { borderTop: '1px solid #eaecf0', padding: '10px 0 0', background: '#fff' },
    detailsSummary: { cursor: 'pointer', color: '#475467', fontSize: 13, fontWeight: 700 },
    mono: { margin: '10px 0 0', padding: 10, background: '#111827', color: '#e5e7eb', borderRadius: 6, overflowX: 'auto', fontSize: 12, lineHeight: 1.5 },
};

const RANGE_OPTIONS = [
    ['15m', '最近 15 分钟', 15],
    ['30m', '最近 30 分钟', 30],
    ['1h', '最近 1 小时', 60],
    ['6h', '最近 6 小时', 360],
    ['12h', '最近 12 小时', 720],
    ['24h', '最近 24 小时', 1440],
];

export default function ContinuousProfilingPanel({ target, targets = [], targetId = '', onTargetChange, showTargetSelect = false, fixedSession = null }) {
    const [range, setRange] = useState('30m');
    const [timeWindow, setTimeWindow] = useState(() => makeTimeWindow('30m'));
    const [customFrom, setCustomFrom] = useState('');
    const [customTo, setCustomTo] = useState('');
    const [appliedCustomWindow, setAppliedCustomWindow] = useState(null);
    const [profileType, setProfileType] = useState('cpu');
    const [signalTab, setSignalTab] = useState('cpu');
    const [stackScope, setStackScope] = useState('all');
    const [flamegraph, setFlamegraph] = useState(null);
    const [topn, setTopn] = useState(null);
    const [histogram, setHistogram] = useState(null);
    const [querying, setQuerying] = useState(false);
    const [error, setError] = useState('');
    const [reliability, setReliability] = useState(null);
    const [resetKey, setResetKey] = useState(0);
    const [scope, setScope] = useState('host');
    const [selectedComm, setSelectedComm] = useState('');
	const [selectedInstance, setSelectedInstance] = useState('');
	const [historicalInstanceValues, setHistoricalInstanceValues] = useState([]);
    const [commValues, setCommValues] = useState([]);
    const [commAvailable, setCommAvailable] = useState(false);
    const [commMessage, setCommMessage] = useState('');
    const [commLoading, setCommLoading] = useState(false);
    const [selectedRuntime, setSelectedRuntime] = useState('');
    const [runtimeValues, setRuntimeValues] = useState([]);
    const [rssSeries, setRssSeries] = useState([]);
    const [maxNodes, setMaxNodes] = useState(5000);
    const [flameSearchInput, setFlameSearchInput] = useState('');
    const [flameSearchText, setFlameSearchText] = useState('');
    const [renderStats, setRenderStats] = useState({ rendered: 0, total: 0, mode: 'full' });
    const [searchStats, setSearchStats] = useState({ matches: 0, samplePercent: 0 });
    // diff selector state (baseline vs compare)
    const [diffOpen, setDiffOpen] = useState(false);
    const [diffMode, setDiffMode] = useState('quick');
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
    const profileURL = flamegraph?.profile_url || topn?.profile_url || target?.profile_url;
    const hasFlamegraph = flamegraph && !flamegraph.empty && Array.isArray(flamegraph.nodes) && flamegraph.nodes.length > 0;
	const sampleState = sampleStateForTarget(target, flamegraph, topn, fixedSession);
	const sessionMeta = continuousSessionMeta(target, fixedSession);
    const sessionSID = sessionMeta.sid;
	const taskScope = fixedSession?.scope === 'process' ? 'process' : 'host';
	const activeProcesses = useMemo(() => decodeJSONField(fixedSession?.active_processes, []), [fixedSession?.active_processes]);
	const processInstances = useMemo(
		() => processInstanceOptions(activeProcesses, historicalInstanceValues),
		[activeProcesses, historicalInstanceValues],
	);
    const rangeOptions = useMemo(() => rangeOptionsForRetention(sessionMeta.retentionHours), [sessionMeta.retentionHours]);
    const diffRangeOptions = useMemo(() => rangeOptionsForRetention(sessionMeta.retentionHours, true), [sessionMeta.retentionHours]);
    const uploadState = uploadFreshness(sessionMeta);
    const unit = flamegraph?.unit || topn?.unit || '';
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

    useEffect(() => {
        const timer = setTimeout(() => setFlameSearchText(flameSearchInput.trim()), 250);
        return () => clearTimeout(timer);
    }, [flameSearchInput]);

    useEffect(() => {
        if (range !== 'custom' && !rangeOptions.some(([value]) => value === range)) {
            const fallback = rangeOptions.some(([value]) => value === '30m') ? '30m' : rangeOptions[0]?.[0] || '15m';
            setRange(fallback);
            setTimeWindow(makeTimeWindow(fallback));
        }
        if (!diffRangeOptions.some(([value]) => value === diffRange)) {
            setDiffRange(diffRangeOptions[0]?.[0] || '15m');
        }
    }, [diffRange, diffRangeOptions, range, rangeOptions]);

    const queryProfiles = useCallback(async (queryWindow) => {
        if (!target) return;
        setQuerying(true);
        setError('');
		const params = {
			session_sid: sessionSID,
            target_id: target.id,
            host: target.ip,
            service: target.service_name || 'hotmethod',
            from: queryWindow.from,
            to: queryWindow.to,
            profile_type: profileType,
            max_nodes: maxNodes,
        };
			if (signalTab === 'cpu' && Object.keys(activeFilters).length > 0) {
                params.filters = JSON.stringify(activeFilters);
            }
        try {
            const timelinePromise = sessionSID
                ? continuous.timeline(sessionSID, { from: queryWindow.from, to: queryWindow.to }).catch(() => null)
                : Promise.resolve(null);
            if (signalTab === 'cpu') {
                setHistogram(null);
                const cpuParams = { ...params };
                if (stackScope !== 'all') cpuParams.stack_scope = stackScope;
                const requests = [
                    profiles.flamegraph(cpuParams),
                    profiles.topn(cpuParams),
                ];
                if (profileType === 'memory') requests.push(profiles.timeseries({ ...cpuParams, metric: 'rss_bytes' }));
                const [profileResults, timelineRes] = await Promise.all([Promise.all(requests), timelinePromise]);
                const [fgRes, topRes, rssRes] = profileResults;
                setReliability(timelineRes?.code === 0 ? timelineRes.data : null);
                if (fgRes.code === 0) setFlamegraph(fgRes.data);
                if (topRes.code === 0) setTopn(topRes.data);
                if (profileType === 'memory') setRssSeries(rssRes?.code === 0 ? (rssRes.data?.series || []) : []);
                if (fgRes.code !== 0 || topRes.code !== 0) {
                    setError(fgRes.message || topRes.message || 'Native Continuous Profiling 查询失败');
                }
            } else {
                setFlamegraph(null);
                setTopn(null);
				const signalType = signalTab === 'io' ? 'io_latency' : signalTab === 'io_syscall' ? 'io_syscall_latency' : 'sched_latency';
                const [histRes, timelineRes] = await Promise.all([
                    continuous.histogram({ ...params, signal_type: signalType }),
                    timelinePromise,
                ]);
                setReliability(timelineRes?.code === 0 ? timelineRes.data : null);
                if (histRes.code === 0) setHistogram(histRes.data);
                if (histRes.code !== 0) {
                    setError(histRes.message || 'Native Continuous eBPF histogram 查询失败');
                }
            }
        } catch (err) {
            setFlamegraph(null);
            setTopn(null);
            setHistogram(null);
            setRssSeries([]);
            setReliability(null);
            setError(err?.message || 'Native Continuous Profiling 查询失败');
        } finally {
            setQuerying(false);
        }
    }, [target, profileType, activeFilters, signalTab, stackScope, maxNodes, sessionSID]);

    useEffect(() => {
        queryProfiles(timeWindow);
    }, [queryProfiles, timeWindow]);

    const refresh = useCallback(() => {
        if (range === 'custom') {
            queryProfiles(timeWindow);
            return;
        }
        setTimeWindow(makeTimeWindow(range));
    }, [queryProfiles, range, timeWindow]);

    const changeRange = useCallback((nextRange) => {
        setRange(nextRange);
        setError('');
        if (nextRange === 'custom') {
            const source = appliedCustomWindow || timeWindow;
            setCustomFrom(toLocalDateTimeInput(source.from));
            setCustomTo(toLocalDateTimeInput(source.to));
            return;
        }
        setTimeWindow(makeTimeWindow(nextRange));
    }, [appliedCustomWindow, timeWindow]);

    const applyCustomRange = useCallback(() => {
        const result = validateCustomTimeWindow(customFrom, customTo, sessionMeta.retentionHours);
        if (result.error) {
            setError(result.error);
            return;
        }
        setError('');
        setRange('custom');
        setAppliedCustomWindow(result.window);
        setTimeWindow(result.window);
    }, [customFrom, customTo, sessionMeta.retentionHours]);

    // Load recent Go pprof heap tasks for the Memory tab link.
    const loadHeapTasks = useCallback(async () => {
        if (!target) return;
        setHeapTasksLoading(true);
        try {
            const res = await profiles.heapTasks({ host: target.ip, limit: 5 });
            if (res.code === 0) {
                setHeapTasks(res.data?.tasks || res.data || []);
            }
        } catch (e) {
            // Silent: Memory tab is best-effort.
        } finally {
            setHeapTasksLoading(false);
        }
    }, [target]);

    useEffect(() => {
        if (profileType === 'memory') loadHeapTasks();
    }, [profileType, loadHeapTasks]);

    // buildDiffParams 是表格 diff 和火焰图 diff 共用的部分——算 baseline/compare
    // 时间窗、拼 target/filters 这些查询参数，两条路径只在要不要加
    // format=flamegraph 上分叉，不要各写一份容易漂移。
    const buildDiffParams = useCallback(() => {
        let baseWindow;
        let compareWindow;
        if (diffMode === 'custom') {
            const baseResult = validateCustomTimeWindow(diffBaseFrom, diffBaseTo, sessionMeta.retentionHours, 'Baseline');
            const compareResult = validateCustomTimeWindow(diffCompareFrom, diffCompareTo, sessionMeta.retentionHours, 'Compare');
            if (baseResult.error || compareResult.error) {
                return { error: baseResult.error || compareResult.error };
            }
            baseWindow = baseResult.window;
            compareWindow = compareResult.window;
            const durationDelta = Math.abs(
                (new Date(baseWindow.to).getTime() - new Date(baseWindow.from).getTime())
                - (new Date(compareWindow.to).getTime() - new Date(compareWindow.from).getTime()),
            );
            if (durationDelta >= 1000) {
                return { error: 'Baseline 与 Compare 必须使用等长时间窗' };
            }
            setAppliedDiffCustomWindows({ baseWindow, compareWindow });
        } else {
            ({ baseWindow, compareWindow } = makeSequentialDiffWindows(diffRange));
        }
        const params = {
            session_sid: sessionSID,
            target_id: target.id,
            host: target.ip,
            service: target.service_name || 'hotmethod',
            profile_type: 'cpu',
            base_from: baseWindow.from,
            base_to: baseWindow.to,
            compare_from: compareWindow.from,
            compare_to: compareWindow.to,
            max_nodes: maxNodes,
        };
        if (stackScope !== 'all') params.stack_scope = stackScope;
        if (Object.keys(activeFilters).length > 0) params.filters = JSON.stringify(activeFilters);
        return { params };
    }, [target, sessionSID, diffMode, diffRange, diffBaseFrom, diffBaseTo, diffCompareFrom, diffCompareTo, sessionMeta.retentionHours, maxNodes, stackScope, activeFilters]);

    const runDiff = useCallback(async () => {
        if (!target) return;
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
    }, [target, buildDiffParams]);

    const runDiffFlamegraph = useCallback(async () => {
        if (!target) return;
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
    }, [target, buildDiffParams]);

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

    const changeDiffMode = useCallback((nextMode) => {
        setDiffMode(nextMode);
        setDiffError('');
        if (nextMode === 'custom') {
            const { baseWindow, compareWindow } = appliedDiffCustomWindows || makeSequentialDiffWindows(diffRange);
            setDiffBaseFrom(toLocalDateTimeInput(baseWindow.from));
            setDiffBaseTo(toLocalDateTimeInput(baseWindow.to));
            setDiffCompareFrom(toLocalDateTimeInput(compareWindow.from));
            setDiffCompareTo(toLocalDateTimeInput(compareWindow.to));
        }
    }, [appliedDiffCustomWindows, diffRange]);

    useEffect(() => {
        setSelectedComm('');
		setSelectedRuntime('');
		setSelectedInstance('');
		setScope(taskScope === 'process' ? 'process' : 'host');
	}, [target?.id, sessionSID, taskScope]);

    useEffect(() => {
        if (!target) {
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
	}, [target, sessionSID, timeWindow, profileType, selectedComm]);

	useEffect(() => {
		if (!target) return undefined;
        let cancelled = false;
		profiles.labelValues({ session_sid: sessionSID, target_id: target.id, host: target.ip, service: target.service_name || 'hotmethod', from: timeWindow.from, to: timeWindow.to, profile_type: profileType, label: 'runtime' })
            .then(res => { if (!cancelled && res.code === 0) setRuntimeValues(res.data?.values || []); })
            .catch(() => { if (!cancelled) setRuntimeValues([]); });
        return () => { cancelled = true; };
	}, [target, sessionSID, timeWindow, profileType]);

	useEffect(() => {
		if (!target || taskScope !== 'process' || !sessionSID || profileType !== 'cpu') {
			setHistoricalInstanceValues([]);
			return undefined;
		}
		let cancelled = false;
		profiles.labelValues({
			session_sid: sessionSID,
			target_id: target.id,
			host: target.ip,
			service: target.service_name || 'hotmethod',
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
	}, [target, taskScope, sessionSID, profileType, timeWindow]);

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
        <div style={S.panel}>
            <section style={S.card}>
                <div style={S.head}>
                    <div>
                        <div style={S.titleLine}>
							<h3 style={S.title}>{sessionMeta.name}</h3>
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
                        <select style={S.select} value={range} onChange={e => changeRange(e.target.value)}>
                            {rangeOptions.map(([value, label]) => <option key={value} value={value}>{label}</option>)}
                            <option value="custom">自定义时间</option>
                        </select>
                    </Field>
                    <Field label="Profile 类型">
                        <select style={S.select} value={profileType} onChange={e => { setProfileType(e.target.value); if (e.target.value === 'memory') setSignalTab('cpu'); }}>
                            <option value="cpu">CPU</option>
                            <option value="memory">Memory</option>
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
                            <button type="button" style={S.segment(signalTab === 'cpu')} onClick={() => setSignalTab('cpu')}>CPU</button>
							<button type="button" style={S.segment(signalTab === 'io')} onClick={() => setSignalTab('io')}>块 IO</button>
							<button type="button" style={S.segment(signalTab === 'io_syscall')} onClick={() => setSignalTab('io_syscall')}>系统调用 IO</button>
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
							<option value="">全部实例</option>
							{processInstances.map(instance => <option key={instance.value} value={instance.value}>PID {instance.pid} · {formatTime(instance.processStartMs)}{instance.active ? ' · 活动' : ' · 历史'}</option>)}
						</select>
					</Field>}
                </div>
                {range === 'custom' && (
                    <div style={S.customRange}>
                        <Field label="开始时间">
                            <input type="datetime-local" step="60" style={S.textInput} value={customFrom} onChange={e => setCustomFrom(e.target.value)} />
                        </Field>
                        <Field label="结束时间">
                            <input type="datetime-local" step="60" style={S.textInput} value={customTo} onChange={e => setCustomTo(e.target.value)} />
                        </Field>
                        <button type="button" style={S.btn} onClick={applyCustomRange} disabled={querying}>查询</button>
                    </div>
                )}
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
					{signalTab === 'cpu' ? scopeLabel : `${taskScope === 'process' ? '进程范围' : '整机范围'} ${signalTab === 'io' ? '块 IO 延迟' : signalTab === 'io_syscall' ? '系统调用 IO 延迟' : '调度延迟'} / eBPF histogram`}；{sessionMeta.sampler} 以 {formatRateHz(sessionMeta.sampleRateHz)} 低频采样，当前查询窗口：{formatTime(timeWindow.from)} - {formatTime(timeWindow.to)}。
                    {signalTab === 'cpu' ? ' comm 是 Linux task comm，可能被截断到约 15 字符。' : ` 当前 backend：${histogram?.backend || signalBackend(sessionMeta, signalTab) || '-'}`}
                    {scope === 'process' && commMessage ? ` ${commMessage}` : ''}
                </div>
                <CoverageBand reliability={reliability} />
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
			</section> : <HistogramPanel data={histogram} loading={querying} title={signalTab === 'io' ? '块 IO 延迟' : signalTab === 'io_syscall' ? '系统调用 IO 延迟' : '调度延迟'} />}

            {signalTab === 'cpu' && profileType === 'cpu' && (flamegraph?.truncated || flamegraph?.symbol_status) && (
                <div style={{ ...S.warn, marginTop: 10 }}>
                    {flamegraph?.truncated && <span>火焰图节点数超过 {maxNodes} 上限，已截断展示。请缩小时间范围或提高最大节点数以查看完整栈。</span>}
                    {flamegraph?.truncated && flamegraph?.symbol_status && ' · '}
                    {flamegraph?.symbol_status && flamegraph?.symbol_status !== 'not_applicable' && (
                        <span>符号状态：{symbolStatusLabel(flamegraph.symbol_status, flamegraph.symbol_diagnostics)}</span>
                    )}
                </div>
            )}

            {signalTab === 'cpu' && <RuntimeDiagnostics diagnostics={flamegraph?.runtime_diagnostics || topn?.runtime_diagnostics} />}

            {profileType === 'memory' && signalTab === 'cpu' && <RSSTrend series={rssSeries} loading={querying} />}

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
                            <div style={{ marginBottom: 12 }}>
                                <span style={S.segmented}>
                                    <button type="button" style={S.segment(diffMode === 'quick')} onClick={() => changeDiffMode('quick')}>快捷</button>
                                    <button type="button" style={{ ...S.segment(diffMode === 'custom'), borderRight: 'none' }} onClick={() => changeDiffMode('custom')}>自定义</button>
                                </span>
                            </div>
                            {diffMode === 'quick' ? (
                                <div style={S.controls}>
                                    <Field label="相邻窗口时长">
                                        <select style={S.select} value={diffRange} onChange={e => setDiffRange(e.target.value)}>
                                            {diffRangeOptions.map(([v, l]) => <option key={v} value={v}>{l.replace('最近 ', '')}</option>)}
                                        </select>
                                    </Field>
                                </div>
                            ) : (
                                <div style={{ display: 'grid', gap: 10 }}>
                                    <div style={S.controls}>
                                        <Field label="Baseline 开始"><input type="datetime-local" step="60" style={S.textInput} value={diffBaseFrom} onChange={e => setDiffBaseFrom(e.target.value)} /></Field>
                                        <Field label="Baseline 结束"><input type="datetime-local" step="60" style={S.textInput} value={diffBaseTo} onChange={e => setDiffBaseTo(e.target.value)} /></Field>
                                    </div>
                                    <div style={S.controls}>
                                        <Field label="Compare 开始"><input type="datetime-local" step="60" style={S.textInput} value={diffCompareFrom} onChange={e => setDiffCompareFrom(e.target.value)} /></Field>
                                        <Field label="Compare 结束"><input type="datetime-local" step="60" style={S.textInput} value={diffCompareTo} onChange={e => setDiffCompareTo(e.target.value)} /></Field>
                                    </div>
                                </div>
                            )}
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
                                <div style={{ marginTop: 12, overflowX: 'auto' }}>
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

            <details style={S.details}>
                <summary style={S.detailsSummary}>诊断信息</summary>
                <pre style={S.mono}>{diagnosticText({ target, flamegraph, topn, timeWindow, profileType, stackScope, filters: activeFilters })}</pre>
            </details>
        </div>
    );
}

function CoverageBand({ reliability }) {
    if (!reliability?.coverage) return null;
    const segments = coverageSegments(reliability.coverage, reliability.gaps || []);
    const ratio = Math.max(0, Math.min(1, Number(reliability.coverage.ratio) || 0));
    const clock = reliability.clock || {};
    const clockStatus = clock.status || 'unknown';
    const clockBad = clockStatus === 'warning' || clockStatus === 'critical';
    return (
        <div style={S.coverage}>
            <div style={S.sectionHead}>
                <strong style={{ fontSize: 13 }}>采集覆盖</strong>
                <span style={S.subtle}>
                    {(ratio * 100).toFixed(1)}% · {reliability.gaps?.length || 0} 个缺口
                </span>
            </div>
            <div style={S.coverageBar} role="img" aria-label={`采集覆盖率 ${(ratio * 100).toFixed(1)}%`}>
                {segments.map((segment, index) => (
                    <span
                        key={`${segment.type}-${index}`}
                        style={{ ...(segment.type === 'gap' ? S.coverageGap : S.coverageOK), width: `${segment.percent}%` }}
                        title={`${segment.type === 'gap' ? '缺口' : '已覆盖'} ${formatTime(segment.start)} - ${formatTime(segment.end)}`}
                    />
                ))}
            </div>
            <div style={S.metaLine}>
                <span style={clockBad ? { ...S.metaItem, ...S.metaItemWarn } : S.metaItem}>
                    <span style={S.metaKey}>Agent 时钟</span>
                    {clockStatus === 'unknown' ? '未观测' : `${clockStatus} · ${formatClockOffset(clock.offset_ms)}`}
                </span>
            </div>
            {Array.isArray(reliability.gaps) && reliability.gaps.length > 0 && (
                <div style={S.gapList}>
                    {reliability.gaps.slice(0, 6).map((gap, index) => (
                        <span key={`${gap.start}-${index}`}>
                            {formatTime(gap.start)} - {formatTime(gap.end)} · {formatGapDuration(gap.duration_seconds)}
                        </span>
                    ))}
                    {reliability.gaps.length > 6 && <span>另有 {reliability.gaps.length - 6} 个缺口</span>}
                </div>
            )}
        </div>
    );
}

function RuntimeDiagnostics({ diagnostics }) {
    const entries = Object.entries(diagnostics || {});
    if (entries.length === 0) return null;
    return (
        <section style={S.card}>
            <div style={S.sectionHead}><h3 style={S.title}>语言采集状态</h3><span style={S.subtle}>{entries.length} runtimes</span></div>
            <div style={S.tableWrap}><table style={S.table}>
                <thead><tr><th style={S.th}>语言</th><th style={S.th}>状态</th><th style={S.th}>模式</th><th style={S.th}>进程</th><th style={S.th}>诊断</th></tr></thead>
                <tbody>{entries.map(([runtime, item]) => (
                    <tr key={runtime}>
                        <td style={S.td}>{runtimeLabel(runtime)}</td><td style={S.td}>{item.status}</td>
                        <td style={S.td}>{(item.modes || []).join(', ') || '-'}</td>
                        <td style={S.td}>{item.ready_count || 0}/{item.detected_count || 0}{item.limited_count ? ` · 受限 ${item.limited_count}` : ''}</td>
                        <td style={S.td}>{(item.reasons || []).join('; ') || '-'}</td>
                    </tr>
                ))}</tbody>
            </table></div>
        </section>
    );
}

function RSSTrend({ series = [], loading }) {
    const width = 900;
    const height = 240;
    const allPoints = series.flatMap(item => item.points || []);
    const times = allPoints.map(point => new Date(point.timestamp).getTime()).filter(Number.isFinite);
    const maxValue = Math.max(1, ...allPoints.map(point => Number(point.value) || 0));
    const minTime = times.length ? Math.min(...times) : 0;
    const maxTime = times.length ? Math.max(...times) : 1;
    const colors = ['#315efb', '#067647', '#b54708', '#b42318', '#6941c6', '#026aa2'];
    return (
        <section style={S.card}>
            <div style={S.sectionHead}><h3 style={S.title}>Python RSS 趋势</h3><span style={S.subtle}>{series.length} processes · bytes</span></div>
            {loading ? <div style={S.empty}>正在查询 RSS...</div> : series.length === 0 ? <div style={S.empty}>所选范围没有 Python RSS 数据</div> : (
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
                </>
            )}
        </section>
    );
}

export function runtimeLabel(runtime) {
    return ({ python: 'Python', java: 'Java/JVM', node: 'Node.js', go: 'Go', native: 'Native', kernel: 'Kernel', unknown: 'Unknown' })[runtime] || runtime;
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
        `symbol_status: ${flamegraph?.symbol_status || topn?.symbol_status || 'not_applicable'}`,
        `symbol_diagnostics: ${JSON.stringify(flamegraph?.symbol_diagnostics || topn?.symbol_diagnostics || {})}`,
        `runtime_diagnostics: ${JSON.stringify(flamegraph?.runtime_diagnostics || topn?.runtime_diagnostics || {})}`,
        `truncated: ${flamegraph?.truncated || topn?.truncated || false}`,
    ].join('\n');
}

function symbolStatusLabel(status, diagnostics = {}) {
    const percent = Number(diagnostics?.unresolved_percent || 0);
    const unresolved = Number(diagnostics?.unresolved_frame_weight || 0);
    const goState = diagnostics?.go_symbol_state;
    const suffix = goState === 'pending'
        ? ' · Go 符号正在后台预热'
        : goState === 'failed'
            ? ` · Go 符号提取失败${diagnostics?.reasons?.length ? `：${diagnostics.reasons[0]}` : ''}`
            : '';
    switch (status) {
        case 'complete': return `完整（当前范围未检测到裸地址帧）${suffix}`;
        case 'partial': return `部分解析（裸地址帧 ${percent.toFixed(1)}%，权重 ${formatNum(unresolved)}）${suffix}`;
        case 'missing': return `缺失（当前范围主要为裸地址帧，权重 ${formatNum(unresolved)}）${suffix}`;
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

function formatSearchPercent(value) {
    const num = Number(value);
    if (!Number.isFinite(num)) return '0%';
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
