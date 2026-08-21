// ============================================================
// pages/TaskResultPage.js — 任务详情页（/task/result?tid=xxx）
// ============================================================
// 任务状态 + 可视化结果 + TopN/直方图摘要 + 产物下载
// ============================================================

import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { tasks, cosfiles } from '../api';
import JavaFlamegraphPanel from '../components/JavaFlamegraphPanel';
import InteractiveFlamegraph, { foldedTextToFlamegraph } from '../components/InteractiveFlamegraph';
import AICard from '../components/AICard';
import { collectorLabelFromTask, collectorLabelByKind, parseRequestParams } from '../utils/collectors';

const styles = {
    container: { width: '100%', maxWidth: 1280, minWidth: 0, margin: '0 auto', padding: 20, fontFamily: 'Arial, sans-serif', color: '#202124' },
    header: { display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', gap: 16, marginBottom: 16 },
    titleBlock: { minWidth: 0 },
    pageTitle: { margin: '0 0 6px 0', fontSize: 24, lineHeight: 1.25 },
    subtitle: { margin: 0, color: '#667085', fontSize: 13, wordBreak: 'break-all' },
    button: { display: 'inline-flex', alignItems: 'center', justifyContent: 'center', border: '1px solid #d0d7de', background: '#fff', color: '#24292f', textDecoration: 'none', borderRadius: 6, padding: '8px 12px', fontSize: 13, fontWeight: 600, cursor: 'pointer' },
    primaryButton: { border: '1px solid #315efb', background: '#315efb', color: '#fff' },
    card: { minWidth: 0, maxWidth: '100%', background: '#fff', border: '1px solid #e5e7eb', borderRadius: 8, padding: 18, marginBottom: 16, boxShadow: '0 1px 2px rgba(16,24,40,0.04)' },
    sectionTitle: { margin: '0 0 12px 0', fontSize: 17 },
    grid: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(210px, 1fr))', gap: 12 },
    metric: { border: '1px solid #edf0f3', background: '#fbfcfe', borderRadius: 6, padding: 12, minHeight: 58 },
    metricLabel: { fontSize: 12, color: '#667085', marginBottom: 6 },
    metricValue: { fontSize: 14, color: '#202124', wordBreak: 'break-word' },
    badge: { display: 'inline-flex', alignItems: 'center', borderRadius: 999, padding: '4px 10px', fontSize: 12, fontWeight: 700, color: '#fff' },
    stageTimeline: { width: '100%', minWidth: 0, maxWidth: '100%', display: 'grid', gridTemplateColumns: 'repeat(10, minmax(112px, 1fr))', gap: 8, overflowX: 'auto', overflowY: 'hidden', overscrollBehaviorInline: 'contain', paddingBottom: 4, marginBottom: 16 },
    stage: (state) => ({ minHeight: 74, borderRadius: 6, border: `1px solid ${state === 'failed' ? '#fda29b' : state === 'done' ? '#86efac' : state === 'active' ? '#93c5fd' : '#e2e8f0'}`, background: state === 'failed' ? '#fee4e2' : state === 'done' ? '#f0fdf4' : state === 'active' ? '#eff6ff' : '#f8fafc', padding: '10px 11px' }),
    stageTitle: (state) => ({ margin: 0, fontSize: 13, fontWeight: 700, color: state === 'failed' ? '#b42318' : state === 'done' ? '#166534' : state === 'active' ? '#1d4ed8' : '#64748b', whiteSpace: 'nowrap' }),
    stageMeta: { margin: '6px 0 0 0', fontSize: 11, lineHeight: 1.35, color: '#667085', wordBreak: 'break-word', whiteSpace: 'pre-wrap' },
    failure: { border: '1px solid #fda29b', background: '#fff6f5', borderRadius: 6, padding: 14, marginBottom: 16, display: 'flex', gap: 14, alignItems: 'center', justifyContent: 'space-between', flexWrap: 'wrap' },
    notice: { display: 'flex', gap: 8, alignItems: 'center', background: '#eff6ff', border: '1px solid #bfdbfe', color: '#1d4ed8', borderRadius: 6, padding: '10px 12px', marginBottom: 16, fontSize: 13 },
    flameFrame: { width: '100%', height: 720, border: '1px solid #d0d7de', borderRadius: 6, background: '#fff' },
    interactiveFlameBox: { width: '100%', minWidth: 0, maxWidth: '100%', height: 720, overflowX: 'auto', overflowY: 'auto', border: '1px solid #d0d7de', borderRadius: 6, background: '#fff', padding: 6 },
    histogramStage: { display: 'flex', alignItems: 'center', justifyContent: 'center', minHeight: 420, border: '1px solid #d0d7de', borderRadius: 6, background: '#fbfcfe', padding: 16, overflow: 'hidden' },
    histogramImage: { width: '100%', height: 'clamp(340px, 42vw, 520px)', objectFit: 'contain', display: 'block' },
    visualEmpty: { display: 'flex', flexDirection: 'column', alignItems: 'center', justifyContent: 'center', minHeight: 260, color: '#667085', textAlign: 'center', background: '#f8fafc', border: '1px dashed #cbd5e1', borderRadius: 6, padding: 24 },
    split: { display: 'grid', gridTemplateColumns: 'minmax(0, 2fr) minmax(300px, 1fr)', gap: 16, alignItems: 'start', minWidth: 0, maxWidth: '100%' },
    table: { width: '100%', borderCollapse: 'collapse' },
    th: { textAlign: 'left', padding: '10px 12px', borderBottom: '1px solid #d0d7de', color: '#475467', fontSize: 12, background: '#f8fafc' },
    td: { padding: '10px 12px', borderBottom: '1px solid #edf0f3', fontSize: 13, verticalAlign: 'top' },
    paramList: { display: 'grid', gap: 10 },
    paramRow: { display: 'flex', justifyContent: 'space-between', gap: 14, alignItems: 'center', padding: '10px 0', borderBottom: '1px solid #edf0f3' },
    paramLabel: { color: '#667085', fontSize: 12, whiteSpace: 'nowrap' },
    paramValue: { color: '#202124', fontSize: 13, fontWeight: 700, textAlign: 'right', wordBreak: 'break-word' },
    paramHint: { margin: '12px 0 0 0', color: '#667085', fontSize: 12, lineHeight: 1.55 },
    suggestionList: { display: 'grid', gap: 12 },
    suggestionItem: { border: '1px solid #e5e7eb', borderRadius: 8, padding: 14, background: '#fbfcfe' },
    suggestionHead: { display: 'flex', justifyContent: 'space-between', gap: 12, alignItems: 'flex-start', marginBottom: 8 },
    suggestionTitle: { fontSize: 14, fontWeight: 700, color: '#202124', wordBreak: 'break-word' },
    suggestionMeta: { fontSize: 12, color: '#667085', whiteSpace: 'nowrap' },
    suggestionBody: { margin: 0, color: '#344054', fontSize: 13, lineHeight: 1.65, whiteSpace: 'pre-wrap' },
    fileList: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(290px, 1fr))', gap: 10 },
    fileItem: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 12, padding: '11px 12px', border: '1px solid #e5e7eb', borderRadius: 6, background: '#fbfcfe' },
    fileName: { fontSize: 13, color: '#202124', wordBreak: 'break-all', fontWeight: 600 },
    fileMeta: { fontSize: 11, color: '#667085', marginTop: 4 },
    streamState: { marginTop: 6, color: '#667085', fontSize: 12 },
    inlineError: { color: '#b42318', fontSize: 12, marginTop: 8, wordBreak: 'break-word' },
    error: { textAlign: 'center', padding: 60, color: '#b42318' },
    loading: { textAlign: 'center', padding: 60, color: '#667085' },
};

const statusColors = { 0: '#d97706', 1: '#2563eb', 2: '#16a34a', 3: '#dc2626', 4: '#7c3aed', 5: '#64748b' };
const statusNames = { 0: 'PENDING', 1: 'RUNNING', 2: 'DONE', 3: 'FAILED', 4: 'UPLOADING', 5: 'CANCELED' };
const analysisNames = { 0: '待分析', 1: '分析中', 2: '分析完成', 3: '分析失败' };
const stageDefinitions = [
    { id: 'created', label: 'Created' },
    { id: 'queued', label: 'Queued' },
    { id: 'delivered', label: 'Delivered' },
    { id: 'running', label: 'Running' },
    { id: 'uploading', label: 'Uploading' },
    { id: 'collected', label: 'Collected' },
    { id: 'analyzing', label: 'Analyzing' },
    { id: 'success', label: 'Success' },
    { id: 'failed', label: 'Failed' },
    { id: 'canceled', label: 'Canceled' },
];

export default function TaskResultPage() {
    const params = new URLSearchParams(window.location.search);
    const tid = params.get('tid');

    const [task, setTask] = useState(null);
    const [files, setFiles] = useState([]);
    const [artifacts, setArtifacts] = useState([]);
    const [artifactLinks, setArtifactLinks] = useState({});
    const [artifactError, setArtifactError] = useState('');
    const [topFunctions, setTopFunctions] = useState([]);
    const [bpfHistogram, setBpfHistogram] = useState(null);
    const [suggestions, setSuggestions] = useState([]);
    const [attribution, setAttribution] = useState(null);
    const [statusEvents, setStatusEvents] = useState([]);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState('');
    const [polling, setPolling] = useState(false);
    const [retrying, setRetrying] = useState(false);
    const [retryNotice, setRetryNotice] = useState('');
    const [streamState, setStreamState] = useState('connecting');
    const [flameMeta, setFlameMeta] = useState(null);
    const [foldedFlamegraph, setFoldedFlamegraph] = useState(null);
    const [foldedLoading, setFoldedLoading] = useState(false);
    const [foldedError, setFoldedError] = useState('');
    const [manualRefreshSeq, setManualRefreshSeq] = useState(0);
    const taskRef = useRef(null);
    const filesRef = useRef([]);

    useEffect(() => {
        taskRef.current = task;
    }, [task]);

    useEffect(() => {
        filesRef.current = files;
    }, [files]);

    const applyFiles = useCallback((inputFiles = []) => {
        const safeFiles = Array.isArray(inputFiles)
            ? inputFiles.map(f => ({
                ...f,
                download_url: resolveUrl(f?.download_url),
                view_url: resolveUrl(f?.view_url),
            }))
            : [];
        setFiles(safeFiles);
    }, []);

    const applyTaskData = useCallback((data = {}, options = {}) => {
        setTask(data.task || {});
        setTopFunctions(Array.isArray(data.top_functions) ? data.top_functions : []);
        setBpfHistogram(data.bpf_histogram || null);
        setSuggestions(Array.isArray(data.suggestions) ? data.suggestions : []);
        setAttribution(readEmbeddedAttribution(data.suggestions));
        setStatusEvents(Array.isArray(data.status_events) ? data.status_events : []);
        setArtifacts(Array.isArray(data.artifacts) ? data.artifacts : []);
        if (!options.preserveVisualFiles) {
            applyFiles(data.files || []);
        }
        setError('');
    }, [applyFiles]);

    const loadTask = useCallback(async (isPoll = false, options = {}) => {
        if (!tid) return;
        if (!isPoll) setLoading(true);
        setPolling(isPoll);

        try {
            const res = await tasks.detail(tid);
            if (res.code !== 0) {
                if (!isPoll) setError(res.message || '任务不存在');
                return;
            }
            applyTaskData(res.data || {}, options);
        } catch (err) {
            if (!isPoll) setError('加载任务详情失败: ' + (err.message || '未知错误'));
        } finally {
            setLoading(false);
            setPolling(false);
        }
    }, [tid, applyTaskData]);

    useEffect(() => {
        if (attribution) return;
        const file = files.find(item => String(item?.name || '').endsWith('attribution.json'));
        const url = file?.view_url || file?.download_url;
        if (!url) return;
        let cancelled = false;
        fetch(url)
            .then(response => response.ok ? response.json() : null)
            .then(data => { if (!cancelled && data && typeof data === 'object') setAttribution(data); })
            .catch(() => {});
        return () => { cancelled = true; };
    }, [files, attribution]);

    const loadFiles = useCallback(async () => {
        if (!tid) return;
        try {
            const res = await cosfiles.list(tid);
            if (res.code === 0) applyFiles(res.data?.files || []);
        } catch (err) {
            console.error('加载文件列表失败:', err);
        }
    }, [tid, applyFiles]);

    const manualRefresh = useCallback(async () => {
        setManualRefreshSeq(seq => seq + 1);
        await loadTask(true);
        await loadFiles();
    }, [loadTask, loadFiles]);

    useEffect(() => {
        if (!tid) {
            setError('缺少任务 ID 参数');
            setLoading(false);
            return;
        }
        loadTask();
    }, [tid, loadTask]);

    useEffect(() => {
        if (!tid || typeof EventSource === 'undefined') {
            setStreamState('polling');
            return undefined;
        }
        let closed = false;
        let reconnectTimer = null;
        let retryDelay = 1000;
        let eventsSource = null;
        let suggestionsSource = null;

        const openStreams = () => {
            if (closed) return;
            setStreamState('connecting');
            eventsSource = new EventSource(tasks.eventsStreamURL(tid), { withCredentials: true });
            suggestionsSource = new EventSource(tasks.suggestionsStreamURL(tid), { withCredentials: true });

            const handleTaskPayload = (evt) => {
                retryDelay = 1000;
                setStreamState('live');
                try {
                    const payload = JSON.parse(evt.data || '{}');
                    const data = payload.task_snapshot || payload;
                    applyTaskData(data, { preserveVisualFiles: isCompletedWithVisual(taskRef.current, filesRef.current) });
                } catch (_) {
                    setStreamState('polling');
                }
            };
            eventsSource.addEventListener('snapshot', handleTaskPayload);
            eventsSource.addEventListener('task-events', handleTaskPayload);
            eventsSource.addEventListener('complete', handleTaskPayload);
            eventsSource.onerror = () => {
                eventsSource?.close();
                suggestionsSource?.close();
                setStreamState('polling');
                if (!closed) {
                    reconnectTimer = setTimeout(openStreams, retryDelay);
                    retryDelay = Math.min(retryDelay * 2, 15000);
                }
            };

            const handleSuggestions = (evt) => {
                try {
                    const payload = JSON.parse(evt.data || '{}');
                    if (Array.isArray(payload.suggestions)) {
                        setSuggestions(payload.suggestions);
                        const embedded = readEmbeddedAttribution(payload.suggestions);
                        if (embedded) setAttribution(embedded);
                    }
                } catch (_) {}
            };
            suggestionsSource.addEventListener('suggestions', handleSuggestions);
            suggestionsSource.addEventListener('complete', handleSuggestions);
            suggestionsSource.onerror = () => {
                suggestionsSource?.close();
            };
        };

        openStreams();
        return () => {
            closed = true;
            if (reconnectTimer) clearTimeout(reconnectTimer);
            eventsSource?.close();
            suggestionsSource?.close();
        };
    }, [tid, applyTaskData]);

    useEffect(() => {
        if (!task || Number(task.status) >= 2 || streamState === 'live' || streamState === 'connecting') return;
        const timer = setInterval(() => loadTask(true), 3000);
        return () => clearInterval(timer);
    }, [task, loadTask, streamState]);

    useEffect(() => {
        if (!task || Number(task.status) !== 2) return;
        if (streamState === 'live' && Number(task.analysis_status) < 2) return;
        if (hasVisual(files)) return;
        const timer = setInterval(() => {
            loadTask(true);
            loadFiles();
        }, 5000);
        return () => clearInterval(timer);
    }, [task, files, loadTask, loadFiles, streamState]);

    const artifact = useMemo(() => pickVisualArtifact(files), [files]);
    const foldedArtifact = useMemo(() => pickFoldedArtifact(files), [files]);
    const foldedArtifactUrl = foldedArtifact?.url || '';
    const foldedArtifactName = foldedArtifact?.name || '';
    useEffect(() => {
        if (!artifact?.url || artifact.type !== 'flamegraph') {
            setFlameMeta(null);
            return undefined;
        }
        let cancelled = false;
        const worker = new Worker(new URL('../workers/flamegraphWorker.js', import.meta.url));
        worker.onmessage = (event) => {
            if (!cancelled) setFlameMeta(event.data || null);
        };
        worker.onerror = () => {
            if (!cancelled) setFlameMeta({ ok: false, reason: 'worker_error' });
        };
        worker.postMessage({ url: artifact.url, maxNodes: 6000, maxDepth: 160 });
        return () => {
            cancelled = true;
            worker.terminate();
        };
    }, [artifact]);

    useEffect(() => {
        if (!foldedArtifactUrl || artifact?.type === 'bpf') {
            setFoldedFlamegraph(null);
            setFoldedError('');
            setFoldedLoading(false);
            return undefined;
        }
        let cancelled = false;
        setFoldedLoading(true);
        setFoldedError('');
        fetch(foldedArtifactUrl, { credentials: 'include' })
            .then(response => {
                if (!response.ok) throw new Error(`HTTP ${response.status}`);
                return response.text();
            })
            .then(text => {
                if (cancelled) return;
                setFoldedFlamegraph(foldedTextToFlamegraph(text, displayFileName(foldedArtifactName)));
            })
            .catch(err => {
                if (cancelled) return;
                setFoldedFlamegraph(null);
                setFoldedError(err?.message || 'folded stacks 加载失败');
            })
            .finally(() => {
                if (!cancelled) setFoldedLoading(false);
            });
        return () => {
            cancelled = true;
        };
    }, [foldedArtifactUrl, foldedArtifactName, artifact?.type, manualRefreshSeq]);
    const stageStatus = Number(task?.status);
    const stageAnalysisStatus = Number(task?.analysis_status);
    const stages = useMemo(
        () => buildStages(task || {}, statusEvents, stageStatus, stageAnalysisStatus, artifact, files),
        [task, statusEvents, stageStatus, stageAnalysisStatus, artifact, files],
    );
    const failure = stageStatus === 3 ? describeFailure(task || {}, statusEvents) : null;

    if (loading) return <div style={styles.container}><p style={styles.loading}>加载中...</p></div>;
    if (error) return <div style={styles.container}><p style={styles.error}>{error}</p></div>;
    if (!task) return <div style={styles.container}><p style={styles.error}>任务不存在</p></div>;

    const taskParams = parseRequestParams(task.request_params);
    const collectorLabel = collectorLabelFromTask(task);
    const bpfEvent = String(taskParams.event || '').toLowerCase();
    const isBpfTask = Number(task.type) === 5 || Boolean(bpfHistogram);
    const isBpfHistogramTask = isBpfTask && (bpfEvent === 'io' || bpfEvent === 'sched' || Boolean(bpfHistogram?.buckets));
    const isBpfCpuTask = isBpfTask && !isBpfHistogramTask;
    const isJavaTask = Number(task.type) === 1 || Number(task.profiler_type) === 1;
    const isPprofTask = Number(task.type) === 2 || Number(task.profiler_type) === 2;
    const isPprofHeapTask = isPprofTask && (task.task_kind === 'go_pprof_heap' ||
        (typeof taskParams.pprof_url === 'string' && taskParams.pprof_url.indexOf('/heap') !== -1));
    const status = Number(task.status);
    const analysisStatus = Number(task.analysis_status);
    const statusName = statusNames[status] || 'UNKNOWN';
    const statusColor = statusColors[status] || '#667085';
    const shouldPoll = isActiveTaskStatus(status) || (status === 2 && analysisStatus < 2 && !hasVisual(files));

    const refreshArtifactLink = async (artifactItem) => {
        if (!artifactItem?.id) return;
        setArtifactError('');
        try {
            const response = await tasks.artifactDownload(tid, artifactItem.id);
            if (response.code !== 0) throw response;
            setArtifactLinks(prev => ({
                ...prev,
                [artifactItem.id]: {
                    url: resolveUrl(response.data?.url),
                    expiresAt: response.data?.expires_at,
                },
            }));
        } catch (err) {
            const payload = err?.response?.data || err;
            const requestID = payload?.request_id || '-';
            const apiError = payload?.error || {};
            const code = apiError.code || payload?.code || 'ARTIFACT_DOWNLOAD_FAILED';
            const retryable = apiError.retryable;
            setArtifactError(`下载链接刷新失败：${code} · retryable=${String(Boolean(retryable))} · request_id=${requestID}`);
        }
    };

    const retryTask = async () => {
        setRetrying(true);
        setRetryNotice('');
        try {
            const response = await tasks.retry(tid);
            if (response.code !== 0) throw new Error(response.message || '重试请求失败');
            const nextTid = response.data?.task?.tid || response.data?.tid || response.data?.task_id;
            if (nextTid) {
                window.location.assign(`/task/result?tid=${encodeURIComponent(nextTid)}`);
                return;
            }
            setRetryNotice('已创建重试任务，任务列表会显示新的采集记录。');
        } catch (err) {
            setRetryNotice(`无法重试：${err.message || '未知错误'}`);
        } finally {
            setRetrying(false);
        }
    };

    return (
        <div style={styles.container}>
            <div style={styles.header}>
                <div style={styles.titleBlock}>
                    <h2 style={styles.pageTitle}>{task.name || '任务详情'}</h2>
                    <p style={styles.subtitle}>{tid}</p>
                    <p style={styles.streamState}>{streamState === 'live' ? '实时事件流已连接' : streamState === 'connecting' ? '正在连接实时事件流' : '实时事件流不可用，已回退轮询'}</p>
                </div>
                <button style={styles.button} onClick={manualRefresh} disabled={polling}>
                    {polling ? '刷新中...' : '刷新'}
                </button>
            </div>

            {shouldPoll && (
                <div style={styles.notice}>
                    <span>自动刷新中：采集、上传和分析完成后，页面会显示可视化结果与下载入口。</span>
                </div>
            )}

            <StageTimeline stages={stages} />

            {failure && <FailurePanel failure={failure} retrying={retrying} onRetry={retryTask} notice={retryNotice} />}

            <div style={styles.card}>
                <h3 style={styles.sectionTitle}>任务概览</h3>
                <div style={styles.grid}>
                    <Metric label="任务状态" value={<span style={{ ...styles.badge, background: statusColor }}>{statusName}</span>} />
                    <Metric label="分析状态" value={analysisNames[analysisStatus] || '未知'} />
                    <Metric label="采集器" value={collectorLabel} />
                    <Metric label="TaskKind" value={collectorLabelByKind(task.task_kind) || task.task_kind || '旧任务兼容'} />
                    <Metric label="目标 Agent" value={task.target_ip || '-'} />
                    {isPprofTask && <Metric label="pprof 来源" value={redactProfileUrl(taskParams.pprof_url) || '-'} />}
                    <Metric label="创建时间" value={formatTime(task.create_time)} />
                    <Metric label="开始时间" value={formatTime(task.begin_time) || '-'} />
                    <Metric label="结束时间" value={formatTime(task.end_time) || '-'} />
                    <Metric label="状态 reason" value={safeText(task.status_info) || '-'} />
                    <Metric label="request_id" value={task.request_id || '-'} />
                </div>
            </div>

            <StatusEventsPanel events={statusEvents} />

            <div style={styles.card}>
                <h3 style={styles.sectionTitle}>{isBpfHistogramTask ? 'eBPF 直方图' : isBpfCpuTask ? 'eBPF CPU 火焰图' : isJavaTask ? 'Java 火焰图' : isPprofHeapTask ? 'Go pprof Heap 火焰图' : isPprofTask ? 'Go pprof CPU 调用图' : '火焰图'}</h3>
                <VisualResult
                    artifact={artifact}
                    foldedArtifact={foldedArtifact}
                    foldedFlamegraph={foldedFlamegraph}
                    foldedLoading={foldedLoading}
                    foldedError={foldedError}
                    task={task}
                    isBpfHistogramTask={isBpfHistogramTask}
                    flameMeta={flameMeta}
                />
            </div>

            <div className="layout-boundary" style={styles.split}>
                <div style={styles.card}>
                    <h3 style={styles.sectionTitle}>采集参数</h3>
                    <ParameterPanel task={task} />
                </div>

                <div style={styles.card}>
                    <h3 style={styles.sectionTitle}>结果状态</h3>
                    <ResultStatePanel task={task} files={files} artifact={artifact} foldedArtifact={foldedArtifact} />
                </div>
            </div>

            {isJavaTask && (
                <JavaFlamegraphPanel
                    task={task}
                    topFunctions={topFunctions}
                    files={files}
                    artifact={artifact}
                />
            )}

            {isBpfHistogramTask ? (
                <BPFHistogramPanel histogram={bpfHistogram} />
            ) : isJavaTask ? null : (
                <TopFunctionsPanel topFunctions={topFunctions} status={status} isPprof={isPprofTask} isHeap={isPprofHeapTask} />
            )}

            {isBpfHistogramTask && topFunctions.length > 0 && (
                <TopFunctionsPanel topFunctions={topFunctions} status={status} title="热点 TopN" />
            )}

            <AICard attribution={attribution} />

            <SuggestionsPanel
                suggestions={suggestions}
                bpfHistogram={bpfHistogram}
                isBpfHistogramTask={isBpfHistogramTask}
                status={status}
            />

            <ArtifactsPanel
                files={files}
                artifacts={artifacts}
                artifactLinks={artifactLinks}
                artifactError={artifactError}
                onRefreshDownload={refreshArtifactLink}
            />
        </div>
    );
}

function readEmbeddedAttribution(items) {
    if (!Array.isArray(items)) return null;
    for (const item of items) {
        const value = item?.ai_suggestion;
        if (!value) continue;
        if (typeof value === 'object') return value;
        try {
            const parsed = JSON.parse(value);
            if (parsed && typeof parsed === 'object') return parsed;
        } catch (_) {
            // Historical rows can contain prose rather than structured B3 output.
        }
    }
    return null;
}

function Metric({ label, value }) {
    return (
        <div style={styles.metric}>
            <div style={styles.metricLabel}>{label}</div>
            <div style={styles.metricValue}>{value}</div>
        </div>
    );
}

function VisualResult({ artifact, foldedArtifact, foldedFlamegraph, foldedLoading, foldedError, task, isBpfHistogramTask, flameMeta }) {
    if (artifact?.url) {
        if (artifact.type === 'bpf' || isBpfHistogramTask) {
            return (
                <div>
                    <div style={styles.histogramStage}>
                        <img
                            src={artifact.url}
                            alt={isBpfHistogramTask ? 'eBPF 直方图' : '可视化结果'}
                            style={styles.histogramImage}
                        />
                    </div>
                    <div style={{ marginTop: 10, display: 'flex', gap: 10, flexWrap: 'wrap' }}>
                        <a href={artifact.downloadUrl || artifact.url} target="_blank" rel="noreferrer" style={{ ...styles.button, ...styles.primaryButton }} download={displayFileName(artifact.name)}>
                            下载可视化文件
                        </a>
                        <a href={artifact.url} target="_blank" rel="noreferrer" style={styles.button}>
                            新窗口查看
                        </a>
                    </div>
                </div>
            );
        }

        if (foldedArtifact?.url) {
            return (
                <div>
                    {foldedError && (
                        <div style={styles.notice}>
                            folded stacks 加载失败：{safeText(foldedError)}。已回退到原始 SVG 视图。
                        </div>
                    )}
                    {!foldedError && (
                        <InteractiveFlamegraph
                            data={foldedFlamegraph}
                            loading={foldedLoading}
                            loadingMessage="正在从 folded stacks 构建交互式火焰图..."
                            emptyMessage="folded stacks 中没有可渲染的样本"
                            externalUrl={artifact.url}
                            externalLabel="原始 SVG 视图"
                            boxStyle={styles.interactiveFlameBox}
                        />
                    )}
                    {foldedError && (
                        <iframe
                            src={artifact.url}
                            title="Flame Graph"
                            style={styles.flameFrame}
                        />
                    )}
                    <div style={{ marginTop: 10, display: 'flex', gap: 10, flexWrap: 'wrap' }}>
                        <a href={artifact.downloadUrl || artifact.url} target="_blank" rel="noreferrer" style={{ ...styles.button, ...styles.primaryButton }} download={displayFileName(artifact.name)}>
                            下载原始 SVG
                        </a>
                        <a href={artifact.url} target="_blank" rel="noreferrer" style={styles.button}>
                            原始 SVG 视图
                        </a>
                        <a href={foldedArtifact.downloadUrl || foldedArtifact.url} target="_blank" rel="noreferrer" style={styles.button} download={displayFileName(foldedArtifact.name)}>
                            下载 folded stacks
                        </a>
                    </div>
                </div>
            );
        }

        return (
            <div>
                {flameMeta?.truncated && (
                    <div style={styles.notice}>
                        火焰图节点 {flameMeta.nodeCount}、深度 {flameMeta.depth}，已建议使用新窗口或下载查看，避免主线程长时间阻塞。
                    </div>
                )}
                {flameMeta && flameMeta.ok === false && (
                    <div style={styles.notice}>
                        火焰图后台检测失败：{safeText(flameMeta.reason)}。仍可使用 iframe 或新窗口查看。
                    </div>
                )}
                <iframe
                    src={artifact.url}
                    title={isBpfHistogramTask ? 'eBPF Histogram' : 'Flame Graph'}
                    style={styles.flameFrame}
                />
                <div style={{ marginTop: 10, display: 'flex', gap: 10, flexWrap: 'wrap' }}>
                    <a href={artifact.downloadUrl || artifact.url} target="_blank" rel="noreferrer" style={{ ...styles.button, ...styles.primaryButton }} download={displayFileName(artifact.name)}>
                        下载可视化文件
                    </a>
                    <a href={artifact.url} target="_blank" rel="noreferrer" style={styles.button}>
                        原始 SVG 视图
                    </a>
                </div>
            </div>
        );
    }

    if (foldedArtifact?.url) {
        return (
            <div>
                {foldedError && <div style={styles.notice}>folded stacks 加载失败：{safeText(foldedError)}</div>}
                <InteractiveFlamegraph
                    data={foldedFlamegraph}
                    loading={foldedLoading}
                    loadingMessage="正在从 folded stacks 构建交互式火焰图..."
                    emptyMessage="folded stacks 中没有可渲染的样本"
                    externalUrl={foldedArtifact.url}
                    externalLabel="打开 folded stacks"
                    boxStyle={styles.interactiveFlameBox}
                />
                <div style={{ marginTop: 10, display: 'flex', gap: 10, flexWrap: 'wrap' }}>
                    <a href={foldedArtifact.downloadUrl || foldedArtifact.url} target="_blank" rel="noreferrer" style={{ ...styles.button, ...styles.primaryButton }} download={displayFileName(foldedArtifact.name)}>
                        下载 folded stacks
                    </a>
                </div>
            </div>
        );
    }

    const status = Number(task.status);
    const text = status === 3
        ? '任务失败，未生成可视化产物。'
        : status === 2
            ? (isBpfHistogramTask ? '采集已完成，正在等待分析引擎生成直方图。' : '采集已完成，正在等待分析引擎生成火焰图。')
            : (isBpfHistogramTask ? 'eBPF 采集中，直方图会在分析完成后显示。' : 'CPU 采集中，火焰图会在分析完成后显示。');

    return (
        <div style={styles.visualEmpty}>
            <strong>{text}</strong>
            {task.status_info && <span style={{ marginTop: 8, fontSize: 13 }}>{task.status_info}</span>}
        </div>
    );
}

function ParameterPanel({ task }) {
    const params = parseRequestParams(task.request_params);
    const rows = [
        ['采集器', collectorLabelFromTask(task)],
        ['TaskKind', collectorLabelByKind(task.task_kind) || task.task_kind || '旧任务兼容'],
        ['目标 PID', Number(params.target_pid || 0) > 0 ? params.target_pid : '整机'],
        ['采样时长', params.duration ? `${params.duration} 秒` : '-'],
        ['采样频率', params.frequency ? `${params.frequency} Hz` : '-'],
        ['事件/模式', eventLabel(params.event)],
        ['调用图', params.callgraph || '-'],
        ['子进程', params.subprocess ? '包含' : '不包含'],
    ];

    return (
        <div>
            <div style={styles.paramList}>
                {rows.map(([label, value]) => (
                    <div key={label} style={styles.paramRow}>
                        <span style={styles.paramLabel}>{label}</span>
                        <span style={styles.paramValue}>{value}</span>
                    </div>
                ))}
            </div>
            <p style={styles.paramHint}>
                这些参数来自任务创建请求，分析产物会在采集完成后自动关联到当前任务。
            </p>
        </div>
    );
}

function ResultStatePanel({ task, files, artifact, foldedArtifact }) {
    const status = Number(task?.status);
    const analysisStatus = Number(task?.analysis_status);
    const rows = [
        ['可视化产物', artifact?.name ? displayFileName(artifact.name) : '暂无'],
        ['folded stacks', foldedArtifact?.name ? displayFileName(foldedArtifact.name) : '暂无'],
        ['产物数量', files.length],
        ['自动刷新', isCompletedWithVisual(task, files) ? '已停止，使用手动刷新更新' : '等待任务或分析完成'],
        ['分析状态', analysisNames[analysisStatus] || '未知'],
        ['任务状态', statusNames[status] || 'UNKNOWN'],
    ];
    return (
        <div style={styles.paramList}>
            {rows.map(([label, value]) => (
                <div key={label} style={styles.paramRow}>
                    <span style={styles.paramLabel}>{label}</span>
                    <span style={styles.paramValue}>{value}</span>
                </div>
            ))}
        </div>
    );
}

function StatusEventsPanel({ events }) {
    return (
        <div style={styles.card}>
            <h3 style={styles.sectionTitle}>状态迁移 Reason</h3>
            {events.length > 0 ? (
                <div className="table-scroll">
                    <table style={styles.table}>
                        <thead>
                            <tr>
                                <th style={styles.th}>时间</th>
                                <th style={styles.th}>迁移</th>
                                <th style={styles.th}>来源</th>
                                <th style={styles.th}>Reason</th>
                            </tr>
                        </thead>
                        <tbody>
                            {events.map(event => (
                                <tr key={event.id}>
                                    <td style={styles.td}>{formatTime(event.created_at)}</td>
                                    <td style={styles.td}>{statusLabel(event.from_status)} -> {statusLabel(event.to_status)}</td>
                                    <td style={styles.td}>{safeText(event.source || event.source_module) || '-'}</td>
                                    <td style={styles.td}>{safeText(event.reason) || '-'}</td>
                                </tr>
                            ))}
                        </tbody>
                    </table>
                </div>
            ) : (
                <p style={{ textAlign: 'center', padding: 24, color: '#667085', margin: 0 }}>
                    旧任务暂无迁移审计；新建任务会记录每次状态变化及 reason。
                </p>
            )}
        </div>
    );
}

function BPFHistogramPanel({ histogram }) {
    const buckets = Array.isArray(histogram?.buckets) ? histogram.buckets : [];
    const summary = histogram?.summary || {};

    return (
        <div style={styles.card}>
            <h3 style={styles.sectionTitle}>直方图摘要</h3>
            {histogram ? (
                <>
                    <div style={styles.grid}>
                        <Metric label="类型" value={histogram.type || 'unknown'} />
                        <Metric label="总事件数" value={histogram.total_events ?? 0} />
                        <Metric label="单位" value={histogram.unit || 'us'} />
                        <Metric label="P50 / P95 / P99" value={`${formatNumber(summary.p50)} / ${formatNumber(summary.p95)} / ${formatNumber(summary.p99)}`} />
                    </div>
                    {buckets.length > 0 ? (
                        <div className="table-scroll" style={{ marginTop: 14 }}>
                            <table style={styles.table}>
                                <thead>
                                    <tr>
                                        <th style={styles.th}>区间</th>
                                        <th style={styles.th}>次数</th>
                                        <th style={styles.th}>低值</th>
                                        <th style={styles.th}>高值</th>
                                    </tr>
                                </thead>
                                <tbody>
                                    {buckets.slice(0, 30).map((bucket, index) => (
                                        <tr key={`${bucket.range}-${index}`}>
                                            <td style={styles.td}>{bucket.range}</td>
                                            <td style={styles.td}>{bucket.count}</td>
                                            <td style={styles.td}>{formatNumber(bucket.low)}</td>
                                            <td style={styles.td}>{formatNumber(bucket.high)}</td>
                                        </tr>
                                    ))}
                                </tbody>
                            </table>
                        </div>
                    ) : (
                        <p style={{ color: '#667085', margin: '12px 0 0 0' }}>暂无桶数据。</p>
                    )}
                </>
            ) : (
                <p style={{ color: '#667085', margin: 0 }}>等待 bpf_data.json 生成后显示摘要。</p>
            )}
        </div>
    );
}

function TopFunctionsPanel({ topFunctions, status, title = '热点 TopN', isPprof = false, isHeap = false }) {
    const unit = topFunctions[0]?.sample_unit || (isHeap ? 'bytes' : (isPprof ? 'seconds' : 'samples'));
    const isSeconds = unit === 'seconds';
    return (
        <div style={styles.card}>
            <h3 style={styles.sectionTitle}>{title}</h3>
            {topFunctions.length > 0 ? (
                <div className="table-scroll">
                    <table style={styles.table}>
                        <thead>
                                <tr>
                                    <th style={styles.th}>#</th>
                                    <th style={styles.th}>函数名</th>
                                <th style={styles.th}>{isSeconds ? '自身 CPU 时间' : '采样次数'}</th>
                                <th style={styles.th} title="该函数自身（栈顶）采样次数 ÷ 全部函数自身采样总数，不含它调用的子函数">占比（自身 / 总采样）</th>
                            </tr>
                        </thead>
                        <tbody>
                            {topFunctions.map((item, index) => (
                                <tr key={`${item.function || item.name || 'fn'}-${index}`}>
                                    <td style={styles.td}>{item.rank || index + 1}</td>
                                    <td style={styles.td}>{item.function || item.name || '-'}</td>
                                    <td style={styles.td}>{formatSampleValue(item.samples ?? item.count ?? 0, item.sample_unit || unit)}</td>
                                    <td style={styles.td} title="自身采样占比，不含子函数">{formatPercent(item.percentage ?? item.percent)}</td>
                                </tr>
                            ))}
                        </tbody>
                    </table>
                </div>
            ) : (
                <p style={{ textAlign: 'center', padding: 32, color: '#667085', margin: 0 }}>
                    {isFinalTaskStatus(status) ? '暂无热点数据。请确认 top.json 已生成，或下载 folded.txt / perf.data 排查。' : '任务完成后将自动分析热点函数。'}
                </p>
            )}
        </div>
    );
}

function SuggestionsPanel({ suggestions, bpfHistogram, isBpfHistogramTask, status }) {
    const items = suggestions.length > 0
        ? suggestions
        : (isBpfHistogramTask ? deriveBpfSuggestions(bpfHistogram) : []);

    return (
        <div style={styles.card}>
            <h3 style={styles.sectionTitle}>优化建议</h3>
            {items.length > 0 ? (
                <div style={styles.suggestionList}>
                    {items.slice(0, 8).map((item, index) => (
                        <div key={`${item.function || item.title || 'suggestion'}-${index}`} style={styles.suggestionItem}>
                            <div style={styles.suggestionHead}>
                                <div style={styles.suggestionTitle}>
                                    {safeText(item.function || item.title || `建议 ${index + 1}`)}
                                </div>
                                <div style={styles.suggestionMeta}>
                                    {formatSuggestionMeta(item)}
                                </div>
                            </div>
                            <p style={styles.suggestionBody}>{safeText(item.advice || item.suggestion) || '-'}</p>
                        </div>
                    ))}
                </div>
            ) : (
                <p style={{ textAlign: 'center', padding: 28, color: '#667085', margin: 0 }}>
                    {isFinalTaskStatus(status)
                        ? (isBpfHistogramTask ? '暂无建议。eBPF IO/调度任务需要有效直方图摘要。' : '暂无建议。CPU 任务需要 suggestions.json 或热点 TopN。')
                        : '任务完成后将自动生成分析建议。'}
                </p>
            )}
        </div>
    );
}

function ArtifactsPanel({ files, artifacts, artifactLinks, artifactError, onRefreshDownload }) {
    const hasRegisteredArtifacts = Array.isArray(artifacts) && artifacts.length > 0;
    return (
        <div style={styles.card}>
            <h3 style={styles.sectionTitle}>产物文件下载</h3>
            {artifactError && <div style={styles.inlineError}>{safeText(artifactError)}</div>}
            {hasRegisteredArtifacts ? (
                <div style={styles.fileList}>
                    {artifacts.map((artifact) => {
                        const link = artifactLinks[artifact.id];
                        return (
                            <div key={artifact.id} style={styles.fileItem}>
                                <div style={{ minWidth: 0 }}>
                                    <div style={styles.fileName}>{displayFileName(artifact.name)}</div>
                                    <div style={styles.fileMeta}>
                                        {artifact.content_type || 'application/octet-stream'} · {formatSize(artifact.size)} · {artifact.kind || 'artifact'}
                                        {link?.expiresAt ? ` · 过期 ${formatTime(link.expiresAt)}` : ''}
                                    </div>
                                </div>
                                <div style={{ display: 'flex', gap: 8, flexShrink: 0 }}>
                                    {link?.url && (
                                        <a href={link.url} download={displayFileName(artifact.name)} style={{ ...styles.button, ...styles.primaryButton }}>
                                            下载
                                        </a>
                                    )}
                                    <button style={styles.button} onClick={() => onRefreshDownload(artifact)}>
                                        {link?.url ? '刷新链接' : '生成链接'}
                                    </button>
                                </div>
                            </div>
                        );
                    })}
                </div>
            ) : files.length > 0 ? (
                <div style={styles.fileList}>
                    {files.map((file, index) => (
                        <div key={file.name || index} style={styles.fileItem}>
                            <div style={{ minWidth: 0 }}>
                                <div style={styles.fileName}>{displayFileName(file.name)}</div>
                                <div style={styles.fileMeta}>
                                    {file.content_type || 'application/octet-stream'} · {formatSize(file.size)}
                                    {file.source ? ` · ${file.source}` : ''}
                                </div>
                            </div>
                            <div style={{ display: 'flex', gap: 8, flexShrink: 0 }}>
                                {file.view_url && (
                                    <a href={file.view_url} target="_blank" rel="noreferrer" style={styles.button}>
                                        查看
                                    </a>
                                )}
                                {file.download_url ? (
                                    <a href={file.download_url} download={displayFileName(file.name)} style={{ ...styles.button, ...styles.primaryButton }}>
                                        下载
                                    </a>
                                ) : (
                                    <span style={{ color: '#98a2b3', fontSize: 12 }}>无链接</span>
                                )}
                            </div>
                        </div>
                    ))}
                </div>
            ) : (
                <p style={{ textAlign: 'center', padding: 24, color: '#667085', margin: 0 }}>暂无产物文件</p>
            )}
        </div>
    );
}

function StageTimeline({ stages }) {
    return (
        <div style={styles.stageTimeline} aria-label="任务阶段时间线">
            {stages.map(stage => (
                <div key={stage.id} style={styles.stage(stage.state)}>
                    <p style={styles.stageTitle(stage.state)}>{stage.label}</p>
                    <p style={styles.stageMeta}>{stage.detail}</p>
                </div>
            ))}
        </div>
    );
}

function FailurePanel({ failure, retrying, onRetry, notice }) {
    return (
        <div style={styles.failure} role="alert">
            <div>
                <div style={{ fontSize: 14, fontWeight: 700, color: '#b42318' }}>任务失败</div>
                <div style={{ marginTop: 5, fontSize: 13, color: '#475467', wordBreak: 'break-word' }}>
                    错误码：{failure.code} · {safeText(failure.message)}
                </div>
                <div style={{ marginTop: 4, fontSize: 12, color: '#667085' }}>
                    stage: {failure.stage} · retryable: {String(failure.retryable)} · request_id: {failure.requestID || '-'}
                </div>
                <div style={{ marginTop: 4, fontSize: 12, color: '#667085' }}>
                    {failure.action}
                </div>
                {notice && <div style={{ marginTop: 6, fontSize: 12, color: '#b42318' }}>{safeText(notice)}</div>}
            </div>
            {failure.retryable && (
                <button style={{ ...styles.button, ...styles.primaryButton }} onClick={onRetry} disabled={retrying}>
                    {retrying ? '正在创建重试任务...' : '重试任务'}
                </button>
            )}
        </div>
    );
}

function buildStages(task, events, status, analysisStatus, artifact, files) {
    const hasEvent = (stageId) => events.some(event => eventMatchesStage(event, stageId));
    const rawArtifact = files.some(file => /(?:perf\.data|folded\.txt|bpf_raw|java_folded)/i.test(String(file?.name || '')));
    const stateByID = {
        created: true,
        queued: hasEvent('queued') || status !== 0 || events.length > 1,
        delivered: hasEvent('delivered') || status === 1 || status === 4 || status === 2 || analysisStatus > 0,
        running: hasEvent('running') || status === 1 || status === 4 || status === 2 || analysisStatus > 0,
        uploading: hasEvent('uploading') || status === 4 || status === 2 || rawArtifact,
        collected: hasEvent('collected') || status === 2 || analysisStatus > 0 || rawArtifact,
        analyzing: hasEvent('analyzing') || analysisStatus > 0 || Boolean(artifact),
        success: status === 2 && (analysisStatus >= 2 || Boolean(artifact)),
        failed: status === 3,
        canceled: status === 5,
    };
    const activeOrder = ['created', 'queued', 'delivered', 'running', 'uploading', 'collected', 'analyzing', 'success'];
    const currentID = status === 5 ? 'canceled' : status === 3 ? 'failed' : [...activeOrder].reverse().find(id => stateByID[id]) || 'created';

    return stageDefinitions.map((definition) => {
        let state = 'pending';
        if (definition.id === currentID && (definition.id === 'failed' || definition.id === 'canceled')) state = 'failed';
        else if (definition.id === currentID) state = 'active';
        else if (stateByID[definition.id]) state = 'done';
        const source = events.find(event => eventMatchesStage(event, definition.id));
        return {
            ...definition,
            state,
            detail: stageLine(source, stageDetail(definition.id, task, status, analysisStatus, Boolean(artifact))),
        };
    });
}

function eventMatchesStage(event, stageId) {
    const text = `${event?.reason || ''} ${event?.source || ''} ${event?.source_module || ''}`.toLowerCase();
    const to = Number(event?.to_status);
    const patterns = {
        created: /创建|created/,
        queued: /等待|queued|outbox/,
        delivered: /下发|dispatch|deliver/,
        running: /接收|accepted|agent|runner.started|采集|collect/,
        uploading: /上传|uploading|raw.*artifact/,
        collected: /完成|collected|done|采集产物/,
        analyzing: /分析|analysis/,
        success: /succeeded|result|分析完成/,
        failed: /failed|失败|error/,
        canceled: /cancel|取消/,
    };
    if (stageId === 'uploading' && to === 4) return true;
    if (stageId === 'collected' && to === 2) return true;
    if (stageId === 'failed' && to === 3) return true;
    if (stageId === 'canceled' && to === 5) return true;
    return patterns[stageId]?.test(text);
}

function stageDetail(stageId, task, status, analysisStatus, available) {
    if (stageId === 'created') return formatTime(task.create_time) || '任务已记录';
    if (stageId === 'queued') return status === 0 ? '等待 Server 下发' : '队列阶段已推进';
    if (stageId === 'delivered') return status >= 1 ? '已下发到执行链路' : '等待下发';
    if (stageId === 'running') return status === 1 ? '采集器正在运行' : '等待 Agent 执行';
    if (stageId === 'uploading') return status === 4 ? '正在上传原始数据' : '等待上传';
    if (stageId === 'collected') return status >= 2 ? '采集结果已登记' : '等待采集结果';
    if (stageId === 'analyzing') return analysisStatus === 1 ? '分析器正在处理' : analysisStatus >= 2 ? '分析已完成' : '等待分析器领取';
    if (stageId === 'success') return available ? '结果与产物已可查看' : '等待成功终态';
    if (stageId === 'failed') return status === 3 ? safeText(task.status_info) || '任务失败' : '未失败';
    if (stageId === 'canceled') return status === 5 ? '任务已取消' : '未取消';
    return '';
}

function stageLine(event, fallback) {
    if (!event) return safeText(fallback);
    const parts = [safeText(event.reason || fallback)];
    const meta = [formatTime(event.created_at), event.source || event.source_module].filter(Boolean).join(' · ');
    if (meta) parts.push(meta);
    return parts.filter(Boolean).join('\n');
}

function describeFailure(task, events) {
    const last = [...events].reverse().find(event => event?.reason) || {};
    const message = task.status_info || last.reason || '任务执行失败，服务端未提供详细原因。';
    const match = String(message).match(/\b[A-Z][A-Z0-9_]{2,}\b/);
    const code = match ? match[0] : 'TASK_FAILED';
    const nonRetryable = new Set(['AUTH_FORBIDDEN', 'TASK_INVALID_ARGUMENT', 'TARGET_NOT_FOUND', 'AGENT_INCOMPATIBLE']);
    const retryable = !nonRetryable.has(code);
    return {
        code,
        message,
        retryable,
        requestID: task.request_id || '',
        stage: last.source_module || last.source || 'task',
        action: retryable ? '该错误可重试，将使用原参数创建新的任务。' : '该错误通常不可通过直接重试恢复，请先修正任务参数或运行环境。',
    };
}

function pickVisualArtifact(files) {
    const bpf = files.find(isBpfHistogramFile);
    const java = files.find(isJavaFlamegraphFile);
    const flame = files.find(isFlamegraphFile);
    const picked = bpf || java || flame;
    if (!picked) return null;
    return {
        name: picked.name,
        url: picked.view_url || picked.download_url || '',
        downloadUrl: picked.download_url || picked.view_url || '',
        type: bpf ? 'bpf' : java ? 'java' : 'flamegraph',
    };
}

function pickFoldedArtifact(files) {
    const picked = files.find(isFoldedStackFile);
    if (!picked) return null;
    return {
        name: picked.name,
        url: picked.view_url || picked.download_url || '',
        downloadUrl: picked.download_url || picked.view_url || '',
    };
}

function hasVisual(files) {
    return Boolean(pickVisualArtifact(files) || pickFoldedArtifact(files));
}

function isCompletedWithVisual(task, files) {
    return Number(task?.status) === 2 && hasVisual(files || []);
}

function isActiveTaskStatus(status) {
    return status === 0 || status === 1 || status === 4;
}

function isFinalTaskStatus(status) {
    return status === 2 || status === 3 || status === 5;
}

function statusLabel(status) {
    const n = Number(status);
    if (n < 0) return 'INIT';
    if (n === 0) return 'PENDING';
    if (n === 1) return 'RUNNING';
    if (n === 2) return 'DONE';
    if (n === 3) return 'FAILED';
    if (n === 4) return 'UPLOADING';
    if (n === 5) return 'CANCELED';
    return String(status);
}

function resolveUrl(url) {
    if (!url) return '';
    if (/^https?:\/\//i.test(url)) return url;
    if (!url.startsWith('/')) return url;
    const hostUrl = window.config?.HOST_URL || '';
    return hostUrl ? hostUrl.replace(/\/$/, '') + url : url;
}

function displayFileName(name) {
    if (!name) return '未知文件';
    const parts = name.split('/');
    return parts[parts.length - 1] || name;
}

function redactProfileUrl(value) {
    try {
        const url = new URL(value);
        if (url.username || url.password) { url.username = ''; url.password = ''; }
        return url.toString();
    } catch (_) { return ''; }
}

function safeText(value, maxLength = 2400) {
    return String(value || '')
        .replace(/<[^>]*>/g, '')
        .replace(/!?\[([^\]]*)\]\((?:javascript|data):[^)]*\)/gi, '$1')
        .replace(/[\u0000-\u0008\u000b\u000c\u000e-\u001f]/g, '')
        .slice(0, maxLength);
}

function isBpfHistogramFile(file) {
    const name = String(file?.name || '').toLowerCase();
    return name.endsWith('.svg') && (
        name.includes('bpf_histogram') ||
        name.includes('bpf-latency') ||
        name.includes('bpf_latency')
    );
}

function isFlamegraphFile(file) {
    const name = String(file?.name || '').toLowerCase();
    return name.endsWith('.svg') && name.includes('flamegraph') && !name.includes('bpf_histogram');
}

function isJavaFlamegraphFile(file) {
    const name = String(file?.name || '').toLowerCase();
    return name.endsWith('.svg') && (
        name.includes('java_flamegraph') ||
        name.includes('java-flamegraph')
    );
}

function isFoldedStackFile(file) {
    const name = String(file?.name || '').toLowerCase();
    if (!name) return false;
    return (
        name.endsWith('folded.txt') ||
        name.endsWith('.collapsed') ||
        name.includes('java_folded') ||
        name.includes('java-folded') ||
        name.includes('profile.collapsed')
    );
}

function deriveBpfSuggestions(histogram) {
    if (!histogram || !Array.isArray(histogram.buckets) || histogram.buckets.length === 0) {
        return [];
    }

    const summary = histogram.summary || {};
    const p95 = Number(summary.p95 || 0);
    const p99 = Number(summary.p99 || 0);
    const type = histogram.type || '';
    const unit = histogram.unit || 'us';
    const suggestions = [];

    if (type === 'io_latency') {
        suggestions.push({
            title: 'IO 延迟分布检查',
            percentage: p95,
            advice: `当前 IO 延迟 P95=${formatNumber(p95)}${unit}，P99=${formatNumber(p99)}${unit}。如果高延迟桶占比上升，建议检查磁盘队列深度、同步写入、页缓存命中率和是否存在大量小 I/O；演示时可用 dd/fio 对比采集前后的分布变化。`,
        });
    } else if (type === 'sched_latency') {
        suggestions.push({
            title: '调度延迟分布检查',
            percentage: p95,
            advice: `当前调度延迟 P95=${formatNumber(p95)}${unit}，P99=${formatNumber(p99)}${unit}。如果尾部延迟明显，建议检查 CPU 抢占、run queue 长度、线程数是否过多、容器 CPU quota 和高优先级任务竞争。`,
        });
    }

    const hottestBucket = histogram.buckets.reduce((best, bucket) => {
        if (!best || Number(bucket.count || 0) > Number(best.count || 0)) return bucket;
        return best;
    }, null);
    if (hottestBucket) {
        suggestions.push({
            title: '最高频延迟桶',
            percentage: hottestBucket.count,
            advice: `样本最多的区间是 ${hottestBucket.range}，共 ${hottestBucket.count} 次。建议把该区间作为 baseline，后续制造 IO 或 CPU 压力时观察高延迟桶是否右移。`,
        });
    }

    return suggestions;
}

function formatSuggestionMeta(item) {
    if (item.percentage !== undefined && item.percentage !== null && item.samples !== undefined) {
        return `${formatPercent(item.percentage)} · ${formatSampleValue(item.samples, item.sample_unit || 'samples')}`;
    }
    if (item.percentage !== undefined && item.percentage !== null) {
        return `指标 ${formatNumber(item.percentage)}`;
    }
    return '';
}

function eventLabel(event) {
    if (!event) return '-';
    if (event === 'sched') return '调度延迟';
    if (event === 'io' || event === 'blk') return 'IO 延迟';
    if (event === 'cpu-clock') return 'CPU clock';
    if (event === 'cpu-cycles') return 'CPU cycles';
    if (event === 'cpu') return 'CPU profile';
    return event;
}

function formatTime(value) {
    if (!value) return '';
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return String(value);
    return date.toLocaleString();
}

function formatSize(bytes) {
    if (!bytes) return '0 B';
    const units = ['B', 'KB', 'MB', 'GB'];
    let size = Number(bytes);
    let i = 0;
    while (size >= 1024 && i < units.length - 1) {
        size /= 1024;
        i += 1;
    }
    return `${size.toFixed(1)} ${units[i]}`;
}

function formatPercent(value) {
    if (value === undefined || value === null || value === '') return '0%';
    const n = Number(value);
    if (Number.isNaN(n)) return `${value}%`;
    return `${n.toFixed(n >= 10 ? 1 : 2)}%`;
}

function formatNumber(value) {
    if (value === undefined || value === null || value === '') return '-';
    const n = Number(value);
    if (Number.isNaN(n)) return String(value);
    return n >= 100 ? n.toFixed(0) : n.toFixed(2);
}

function formatSampleValue(value, unit = 'samples') {
    if (value === undefined || value === null || value === '') return '-';
    const n = Number(value);
    if (Number.isNaN(n)) return String(value);
    if (unit === 'seconds') return `${formatNumber(n)}s`;
    return new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 0 }).format(n);
}
