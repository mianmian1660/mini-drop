// ============================================================
// pages/TaskResultPage.js — 任务详情页（/task/result?tid=xxx）
// ============================================================
// 任务状态 + 可视化结果 + TopN/直方图摘要 + 产物下载
// ============================================================

import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { tasks, cosfiles } from '../api';

const styles = {
    container: { maxWidth: 1280, margin: '0 auto', padding: 20, fontFamily: 'Arial, sans-serif', color: '#202124' },
    header: { display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', gap: 16, marginBottom: 16 },
    titleBlock: { minWidth: 0 },
    pageTitle: { margin: '0 0 6px 0', fontSize: 24, lineHeight: 1.25 },
    subtitle: { margin: 0, color: '#667085', fontSize: 13, wordBreak: 'break-all' },
    button: { display: 'inline-flex', alignItems: 'center', justifyContent: 'center', border: '1px solid #d0d7de', background: '#fff', color: '#24292f', textDecoration: 'none', borderRadius: 6, padding: '8px 12px', fontSize: 13, fontWeight: 600, cursor: 'pointer' },
    primaryButton: { border: '1px solid #315efb', background: '#315efb', color: '#fff' },
    card: { background: '#fff', border: '1px solid #e5e7eb', borderRadius: 8, padding: 18, marginBottom: 16, boxShadow: '0 1px 2px rgba(16,24,40,0.04)' },
    sectionTitle: { margin: '0 0 12px 0', fontSize: 17 },
    grid: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(210px, 1fr))', gap: 12 },
    metric: { border: '1px solid #edf0f3', background: '#fbfcfe', borderRadius: 6, padding: 12, minHeight: 58 },
    metricLabel: { fontSize: 12, color: '#667085', marginBottom: 6 },
    metricValue: { fontSize: 14, color: '#202124', wordBreak: 'break-word' },
    badge: { display: 'inline-flex', alignItems: 'center', borderRadius: 999, padding: '4px 10px', fontSize: 12, fontWeight: 700, color: '#fff' },
    progressBar: { display: 'grid', gridTemplateColumns: 'repeat(5, minmax(0, 1fr))', gap: 6, marginBottom: 16 },
    progressStep: (active, done, failed) => ({
        textAlign: 'center',
        padding: '8px 6px',
        fontSize: 12,
        borderRadius: 6,
        background: failed ? '#fee4e2' : done ? '#dcfce7' : active ? '#dbeafe' : '#f1f5f9',
        color: failed ? '#b42318' : done ? '#166534' : active ? '#1d4ed8' : '#64748b',
        border: failed ? '1px solid #fda29b' : active ? '1px solid #93c5fd' : '1px solid transparent',
        fontWeight: active || done || failed ? 700 : 500,
    }),
    notice: { display: 'flex', gap: 8, alignItems: 'center', background: '#eff6ff', border: '1px solid #bfdbfe', color: '#1d4ed8', borderRadius: 6, padding: '10px 12px', marginBottom: 16, fontSize: 13 },
    flameFrame: { width: '100%', height: 560, border: '1px solid #d0d7de', borderRadius: 6, background: '#fff' },
    histogramStage: { display: 'flex', alignItems: 'center', justifyContent: 'center', minHeight: 420, border: '1px solid #d0d7de', borderRadius: 6, background: '#fbfcfe', padding: 16, overflow: 'hidden' },
    histogramImage: { width: '100%', height: 'clamp(340px, 42vw, 520px)', objectFit: 'contain', display: 'block' },
    visualEmpty: { display: 'flex', flexDirection: 'column', alignItems: 'center', justifyContent: 'center', minHeight: 260, color: '#667085', textAlign: 'center', background: '#f8fafc', border: '1px dashed #cbd5e1', borderRadius: 6, padding: 24 },
    split: { display: 'grid', gridTemplateColumns: 'minmax(0, 2fr) minmax(300px, 1fr)', gap: 16, alignItems: 'start' },
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
    error: { textAlign: 'center', padding: 60, color: '#b42318' },
    loading: { textAlign: 'center', padding: 60, color: '#667085' },
};

const statusColors = { 0: '#d97706', 1: '#2563eb', 2: '#16a34a', 3: '#dc2626' };
const statusNames = { 0: 'PENDING', 1: 'RUNNING / UPLOADING', 2: 'DONE', 3: 'FAILED' };
const analysisNames = { 0: '待分析', 1: '分析中', 2: '分析完成', 3: '分析失败' };
const progressSteps = ['PENDING', 'RUNNING', 'UPLOADING', 'ANALYZING', 'DONE'];

export default function TaskResultPage() {
    const params = new URLSearchParams(window.location.search);
    const tid = params.get('tid');

    const [task, setTask] = useState(null);
    const [files, setFiles] = useState([]);
    const [topFunctions, setTopFunctions] = useState([]);
    const [bpfHistogram, setBpfHistogram] = useState(null);
    const [suggestions, setSuggestions] = useState([]);
    const [statusEvents, setStatusEvents] = useState([]);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState('');
    const [polling, setPolling] = useState(false);

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

    const loadTask = useCallback(async (isPoll = false) => {
        if (!tid) return;
        if (!isPoll) setLoading(true);
        setPolling(isPoll);

        try {
            const res = await tasks.detail(tid);
            if (res.code !== 0) {
                if (!isPoll) setError(res.message || '任务不存在');
                return;
            }
            const data = res.data || {};
            setTask(data.task || {});
            setTopFunctions(Array.isArray(data.top_functions) ? data.top_functions : []);
            setBpfHistogram(data.bpf_histogram || null);
            setSuggestions(Array.isArray(data.suggestions) ? data.suggestions : []);
            setStatusEvents(Array.isArray(data.status_events) ? data.status_events : []);
            applyFiles(data.files || []);
            setError('');
        } catch (err) {
            if (!isPoll) setError('加载任务详情失败: ' + (err.message || '未知错误'));
        } finally {
            setLoading(false);
            setPolling(false);
        }
    }, [tid, applyFiles]);

    const loadFiles = useCallback(async () => {
        if (!tid) return;
        try {
            const res = await cosfiles.list(tid);
            if (res.code === 0) applyFiles(res.data?.files || []);
        } catch (err) {
            console.error('加载文件列表失败:', err);
        }
    }, [tid, applyFiles]);

    useEffect(() => {
        if (!tid) {
            setError('缺少任务 ID 参数');
            setLoading(false);
            return;
        }
        loadTask();
    }, [tid, loadTask]);

    useEffect(() => {
        if (!task || Number(task.status) >= 2) return;
        const timer = setInterval(() => loadTask(true), 3000);
        return () => clearInterval(timer);
    }, [task, loadTask]);

    useEffect(() => {
        if (!task || Number(task.status) !== 2) return;
        if (hasVisual(files)) return;
        const timer = setInterval(() => {
            loadTask(true);
            loadFiles();
        }, 5000);
        return () => clearInterval(timer);
    }, [task, files, loadTask, loadFiles]);

    const artifact = useMemo(() => pickVisualArtifact(files), [files]);

    if (loading) return <div style={styles.container}><p style={styles.loading}>加载中...</p></div>;
    if (error) return <div style={styles.container}><p style={styles.error}>{error}</p></div>;
    if (!task) return <div style={styles.container}><p style={styles.error}>任务不存在</p></div>;

    const isBpfTask = Number(task.type) === 5 || Boolean(bpfHistogram);
    const status = Number(task.status);
    const analysisStatus = Number(task.analysis_status);
    const statusName = statusNames[status] || 'UNKNOWN';
    const statusColor = statusColors[status] || '#667085';
    const shouldPoll = status < 2 || (status === 2 && analysisStatus < 2 && !artifact);

    return (
        <div style={styles.container}>
            <div style={styles.header}>
                <div style={styles.titleBlock}>
                    <h2 style={styles.pageTitle}>{task.name || '任务详情'}</h2>
                    <p style={styles.subtitle}>{tid}</p>
                </div>
                <button style={styles.button} onClick={() => loadTask(true)} disabled={polling}>
                    {polling ? '刷新中...' : '刷新'}
                </button>
            </div>

            {shouldPoll && (
                <div style={styles.notice}>
                    <span>自动刷新中：采集、上传和分析完成后，页面会显示可视化结果与下载入口。</span>
                </div>
            )}

            <div style={styles.progressBar}>
                {progressSteps.map((label, index) => {
                    const done = isStepDone(index, status, analysisStatus, artifact);
                    const active = isStepActive(index, status, analysisStatus, artifact);
                    return (
                        <div key={label} style={styles.progressStep(active, done, status === 3)}>
                            {status === 3 && index === 4 ? 'FAILED' : label}
                        </div>
                    );
                })}
            </div>

            <div style={styles.card}>
                <h3 style={styles.sectionTitle}>任务概览</h3>
                <div style={styles.grid}>
                    <Metric label="任务状态" value={<span style={{ ...styles.badge, background: statusColor }}>{statusName}</span>} />
                    <Metric label="分析状态" value={analysisNames[analysisStatus] || '未知'} />
                    <Metric label="采集器" value={profilerLabel(task.profiler_type, task.type, task.request_params?.event)} />
                    <Metric label="目标 Agent" value={task.target_ip || '-'} />
                    <Metric label="创建时间" value={formatTime(task.create_time)} />
                    <Metric label="开始时间" value={formatTime(task.begin_time) || '-'} />
                    <Metric label="结束时间" value={formatTime(task.end_time) || '-'} />
                    <Metric label="状态 reason" value={task.status_info || '-'} />
                </div>
            </div>

            <StatusEventsPanel events={statusEvents} />

            <div style={styles.split}>
                <div style={styles.card}>
                    <h3 style={styles.sectionTitle}>{isBpfTask ? 'eBPF 直方图' : '火焰图'}</h3>
                    <VisualResult artifact={artifact} task={task} isBpfTask={isBpfTask} />
                </div>

                <div style={styles.card}>
                    <h3 style={styles.sectionTitle}>采集参数</h3>
                    <ParameterPanel task={task} />
                </div>
            </div>

            {isBpfTask ? (
                <BPFHistogramPanel histogram={bpfHistogram} />
            ) : (
                <TopFunctionsPanel topFunctions={topFunctions} status={status} />
            )}

            {isBpfTask && topFunctions.length > 0 && (
                <TopFunctionsPanel topFunctions={topFunctions} status={status} title="热点 TopN" />
            )}

            <SuggestionsPanel
                suggestions={suggestions}
                bpfHistogram={bpfHistogram}
                isBpfTask={isBpfTask}
                status={status}
            />

            <ArtifactsPanel files={files} />
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

function VisualResult({ artifact, task, isBpfTask }) {
    if (artifact?.url) {
        if (artifact.type === 'bpf' || isBpfTask) {
            return (
                <div>
                    <div style={styles.histogramStage}>
                        <img
                            src={artifact.url}
                            alt={isBpfTask ? 'eBPF 直方图' : '可视化结果'}
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

        return (
            <div>
                <iframe
                    src={artifact.url}
                    title={isBpfTask ? 'eBPF Histogram' : 'Flame Graph'}
                    style={styles.flameFrame}
                />
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

    const status = Number(task.status);
    const text = status === 3
        ? '任务失败，未生成可视化产物。'
        : status === 2
            ? (isBpfTask ? '采集已完成，正在等待分析引擎生成直方图。' : '采集已完成，正在等待分析引擎生成火焰图。')
            : (isBpfTask ? 'eBPF 采集中，直方图会在分析完成后显示。' : 'CPU 采集中，火焰图会在分析完成后显示。');

    return (
        <div style={styles.visualEmpty}>
            <strong>{text}</strong>
            {task.status_info && <span style={{ marginTop: 8, fontSize: 13 }}>{task.status_info}</span>}
        </div>
    );
}

function ParameterPanel({ task }) {
    const params = task.request_params || {};
    const rows = [
        ['采集器', profilerLabel(task.profiler_type, task.type, params.event)],
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

function StatusEventsPanel({ events }) {
    return (
        <div style={styles.card}>
            <h3 style={styles.sectionTitle}>状态迁移 Reason</h3>
            {events.length > 0 ? (
                <div style={{ overflowX: 'auto' }}>
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
                                    <td style={styles.td}>{event.source || '-'}</td>
                                    <td style={styles.td}>{event.reason || '-'}</td>
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
                        <div style={{ overflowX: 'auto', marginTop: 14 }}>
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

function TopFunctionsPanel({ topFunctions, status, title = '热点 TopN' }) {
    return (
        <div style={styles.card}>
            <h3 style={styles.sectionTitle}>{title}</h3>
            {topFunctions.length > 0 ? (
                <div style={{ overflowX: 'auto' }}>
                    <table style={styles.table}>
                        <thead>
                            <tr>
                                <th style={styles.th}>#</th>
                                <th style={styles.th}>函数名</th>
                                <th style={styles.th}>采样次数</th>
                                <th style={styles.th}>占比</th>
                            </tr>
                        </thead>
                        <tbody>
                            {topFunctions.map((item, index) => (
                                <tr key={`${item.function || item.name || 'fn'}-${index}`}>
                                    <td style={styles.td}>{item.rank || index + 1}</td>
                                    <td style={styles.td}>{item.function || item.name || '-'}</td>
                                    <td style={styles.td}>{item.samples || item.count || 0}</td>
                                    <td style={styles.td}>{formatPercent(item.percentage ?? item.percent)}</td>
                                </tr>
                            ))}
                        </tbody>
                    </table>
                </div>
            ) : (
                <p style={{ textAlign: 'center', padding: 32, color: '#667085', margin: 0 }}>
                    {status >= 2 ? '暂无热点数据。请确认 top.json 已生成，或下载 folded.txt / perf.data 排查。' : '任务完成后将自动分析热点函数。'}
                </p>
            )}
        </div>
    );
}

function SuggestionsPanel({ suggestions, bpfHistogram, isBpfTask, status }) {
    const items = suggestions.length > 0
        ? suggestions
        : (isBpfTask ? deriveBpfSuggestions(bpfHistogram) : []);

    return (
        <div style={styles.card}>
            <h3 style={styles.sectionTitle}>优化建议</h3>
            {items.length > 0 ? (
                <div style={styles.suggestionList}>
                    {items.slice(0, 8).map((item, index) => (
                        <div key={`${item.function || item.title || 'suggestion'}-${index}`} style={styles.suggestionItem}>
                            <div style={styles.suggestionHead}>
                                <div style={styles.suggestionTitle}>
                                    {item.function || item.title || `建议 ${index + 1}`}
                                </div>
                                <div style={styles.suggestionMeta}>
                                    {formatSuggestionMeta(item)}
                                </div>
                            </div>
                            <p style={styles.suggestionBody}>{item.advice || item.suggestion || '-'}</p>
                        </div>
                    ))}
                </div>
            ) : (
                <p style={{ textAlign: 'center', padding: 28, color: '#667085', margin: 0 }}>
                    {status >= 2 ? '暂无建议。CPU 任务需要 suggestions.json，eBPF 任务需要有效直方图摘要。' : '任务完成后将自动生成分析建议。'}
                </p>
            )}
        </div>
    );
}

function ArtifactsPanel({ files }) {
    return (
        <div style={styles.card}>
            <h3 style={styles.sectionTitle}>产物文件下载</h3>
            {files.length > 0 ? (
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
                                    <a href={file.download_url} target="_blank" rel="noreferrer" download={displayFileName(file.name)} style={{ ...styles.button, ...styles.primaryButton }}>
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

function pickVisualArtifact(files) {
    const bpf = files.find(isBpfHistogramFile);
    const flame = files.find(isFlamegraphFile);
    const picked = bpf || flame;
    if (!picked) return null;
    return {
        name: picked.name,
        url: picked.view_url || picked.download_url || '',
        downloadUrl: picked.download_url || picked.view_url || '',
        type: bpf ? 'bpf' : 'flamegraph',
    };
}

function hasVisual(files) {
    return Boolean(pickVisualArtifact(files));
}

function isStepDone(index, status, analysisStatus, artifact) {
    if (status === 3) return false;
    if (index === 0) return status > 0;
    if (index === 1) return status >= 2;
    if (index === 2) return status >= 2;
    if (index === 3) return analysisStatus >= 2 || Boolean(artifact);
    if (index === 4) return status === 2 && (analysisStatus >= 2 || Boolean(artifact));
    return false;
}

function isStepActive(index, status, analysisStatus, artifact) {
    if (status === 3) return index === 4;
    if (index === 0) return status === 0;
    if (index === 1) return status === 1;
    if (index === 2) return status === 2 && analysisStatus === 0 && !artifact;
    if (index === 3) return status === 2 && analysisStatus === 1 && !artifact;
    if (index === 4) return status === 2 && (analysisStatus >= 2 || Boolean(artifact));
    return false;
}

function statusLabel(status) {
    const n = Number(status);
    if (n < 0) return 'INIT';
    if (n === 0) return 'PENDING';
    if (n === 1) return 'RUNNING';
    if (n === 2) return 'DONE';
    if (n === 3) return 'FAILED';
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

function profilerLabel(profilerType, taskType, event) {
    const pt = Number(profilerType);
    if (Number(taskType) === 5) {
        if (event === 'sched') return 'eBPF 调度延迟';
        if (event === 'io' || event === 'blk') return 'eBPF IO 延迟';
        return 'eBPF 内核探针';
    }
    if (pt === 1) return 'async-profiler';
    if (pt === 2) return 'pprof';
    if (pt === 3) return 'eBPF CPU';
    return 'perf CPU';
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
        return `${formatPercent(item.percentage)} · ${item.samples} samples`;
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
