import React, { useMemo } from 'react';

const S = {
    card: { background: '#fff', border: '1px solid #e5e7eb', borderRadius: 8, padding: 18, marginBottom: 16, boxShadow: '0 1px 2px rgba(16,24,40,0.04)' },
    title: { margin: '0 0 12px 0', fontSize: 17 },
    grid: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(220px, 1fr))', gap: 12, marginBottom: 16 },
    metric: { border: '1px solid #edf0f3', background: '#fbfcfe', borderRadius: 6, padding: 12, minHeight: 58 },
    metricLabel: { fontSize: 12, color: '#667085', marginBottom: 6 },
    metricValue: { fontSize: 14, color: '#202124', wordBreak: 'break-word', fontWeight: 700 },
    split: { display: 'grid', gridTemplateColumns: 'minmax(0, 1.25fr) minmax(280px, 0.75fr)', gap: 16, alignItems: 'start' },
    table: { width: '100%', borderCollapse: 'collapse' },
    th: { textAlign: 'left', padding: '10px 12px', borderBottom: '1px solid #d0d7de', color: '#475467', fontSize: 12, background: '#f8fafc' },
    td: { padding: '10px 12px', borderBottom: '1px solid #edf0f3', fontSize: 13, verticalAlign: 'top' },
    pathBox: { border: '1px solid #edf0f3', background: '#fbfcfe', borderRadius: 6, padding: 12 },
    pathTitle: { margin: '0 0 8px 0', fontSize: 13, color: '#475467', fontWeight: 700 },
    pathLine: { margin: 0, color: '#202124', fontSize: 13, lineHeight: 1.65, wordBreak: 'break-word' },
    empty: { textAlign: 'center', padding: 28, color: '#667085', margin: 0 },
    fileList: { display: 'grid', gap: 8 },
    fileLink: { color: '#315efb', fontSize: 13, textDecoration: 'none', fontWeight: 700, wordBreak: 'break-all' },
};

export default function JavaFlamegraphPanel({ task, topFunctions = [], files = [], artifact }) {
    const params = task?.request_params || {};
    const javaFiles = useMemo(() => files.filter(isJavaArtifact), [files]);
    const hottest = topFunctions[0];

    return (
        <div style={S.card}>
            <h3 style={S.title}>Java async-profiler 分析</h3>
            <div style={S.grid}>
                <Metric label="运行时采集器" value="async-profiler" />
                <Metric label="目标 PID" value={Number(params.target_pid || 0) > 0 ? params.target_pid : '整机'} />
                <Metric label="采样频率" value={params.frequency ? `${params.frequency} Hz` : '-'} />
                <Metric label="最热方法" value={hottest?.function || '-'} />
            </div>

            <div style={S.split}>
                <div>
                    <h4 style={S.pathTitle}>Java 方法 TopN</h4>
                    {topFunctions.length > 0 ? (
                        <div style={{ overflowX: 'auto' }}>
                            <table style={S.table}>
                                <thead>
                                    <tr>
                                        <th style={S.th}>#</th>
                                        <th style={S.th}>方法</th>
                                        <th style={S.th}>Samples</th>
                                        <th style={S.th}>占比</th>
                                    </tr>
                                </thead>
                                <tbody>
                                    {topFunctions.slice(0, 12).map((item, index) => (
                                        <tr key={`${item.function || 'method'}-${index}`}>
                                            <td style={S.td}>{item.rank || index + 1}</td>
                                            <td style={S.td}>{item.function || '-'}</td>
                                            <td style={S.td}>{item.samples || 0}</td>
                                            <td style={S.td}>{formatPercent(item.percentage)}</td>
                                        </tr>
                                    ))}
                                </tbody>
                            </table>
                        </div>
                    ) : (
                        <p style={S.empty}>等待 java_top.json 生成后显示方法热点。</p>
                    )}
                </div>

                <div style={S.pathBox}>
                    <h4 style={S.pathTitle}>Java 产物</h4>
                    {javaFiles.length > 0 || artifact?.url ? (
                        <div style={S.fileList}>
                            {artifact?.url && (
                                <a href={artifact.url} target="_blank" rel="noreferrer" style={S.fileLink}>
                                    java_flamegraph.svg
                                </a>
                            )}
                            {javaFiles.map((file, index) => (
                                <a key={`${file.name}-${index}`} href={file.view_url || file.download_url} target="_blank" rel="noreferrer" style={S.fileLink}>
                                    {displayFileName(file.name)}
                                </a>
                            ))}
                        </div>
                    ) : (
                        <p style={S.pathLine}>分析完成后会出现 Java 火焰图、folded stacks 和 TopN JSON。</p>
                    )}
                </div>
            </div>
        </div>
    );
}

function Metric({ label, value }) {
    return (
        <div style={S.metric}>
            <div style={S.metricLabel}>{label}</div>
            <div style={S.metricValue}>{value}</div>
        </div>
    );
}

function isJavaArtifact(file) {
    const name = String(file?.name || '').toLowerCase();
    return name.includes('java_') || name.includes('java-') || name.endsWith('/top.json');
}

function displayFileName(name) {
    if (!name) return '未知文件';
    const parts = name.split('/');
    return parts[parts.length - 1] || name;
}

function formatPercent(value) {
    const n = Number(value);
    if (!Number.isFinite(n)) return '-';
    return `${n.toFixed(2)}%`;
}
