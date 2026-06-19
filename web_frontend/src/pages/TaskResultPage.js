// ============================================================
// pages/TaskResultPage.js — 任务详情页（/task/result?tid=xxx）
// ============================================================
// 功能：基本信息（自动刷新） + 火焰图 + 热点 TopN
// W3：新增 3 秒轮询，任务执行中自动刷新状态
// ============================================================

import React, { useState, useEffect, useCallback } from 'react';
import { tasks, cosfiles } from '../api';

const styles = {
    container: { maxWidth: 1200, margin: '0 auto', padding: 20, fontFamily: 'Arial, sans-serif' },
    card: { background: '#fff', borderRadius: 8, padding: 24, marginBottom: 16, boxShadow: '0 2px 8px rgba(0,0,0,0.1)' },
    table: { width: '100%', borderCollapse: 'collapse' },
    th: { textAlign: 'left', padding: '12px 16px', borderBottom: '2px solid #e0e0e0', color: '#666', fontSize: 13 },
    td: { padding: '12px 16px', borderBottom: '1px solid #f0f0f0', fontSize: 14 },
    badge: { padding: '2px 8px', borderRadius: 10, fontSize: 12, fontWeight: 'bold' },
    loading: { textAlign: 'center', padding: 60, color: '#999' },
    error: { textAlign: 'center', padding: 60, color: '#f44336' },
    flameBox: { textAlign: 'center', padding: 40, background: '#f5f5fa', color: '#999', borderRadius: 8, minHeight: 300 },
    // W3: 轮询指示器
    pollingBar: {
        display: 'flex', alignItems: 'center', justifyContent: 'center', gap: 8,
        padding: '8px 16px', background: '#e3f2fd', borderRadius: 6, marginBottom: 16,
        fontSize: 13, color: '#1565c0',
    },
    // W3: 状态进度条
    progressBar: { display: 'flex', gap: 0, marginBottom: 16 },
    progressStep: (active, done) => ({
        flex: 1, textAlign: 'center', padding: '8px 4px', fontSize: 12,
        background: done ? '#4caf50' : active ? '#2196f3' : '#e0e0e0',
        color: done || active ? '#fff' : '#999',
        borderRadius: 4, margin: '0 2px',
    }),
};

const statusColors = { 0: '#ffc107', 1: '#2196f3', 2: '#4caf50', 3: '#f44336' };
const statusNames = { 0: '待处理', 1: '执行中', 2: '已完成', 3: '失败' };

// W3: 状态步骤定义
const statusSteps = [
    { key: 0, label: '📋 已创建' },
    { key: 1, label: '⚙️ 执行中' },
    { key: 2, label: '✅ 已完成' },
];

export default function TaskResultPage() {
    const params = new URLSearchParams(window.location.search);
    const tid = params.get('tid');

    const [task, setTask] = useState(null);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState('');
    const [polling, setPolling] = useState(false);  // W3: 是否正在轮询任务状态
    // W4: 火焰图文件状态
    const [flameFiles, setFlameFiles] = useState([]);      // 所有产物文件
    const [flameSvgUrl, setFlameSvgUrl] = useState('');    // 火焰图 SVG 的预签名 URL
    const [analysisPolling, setAnalysisPolling] = useState(false);  // 是否在等分析结果

    // W4: 加载任务详情 + 产物文件
    const loadTask = useCallback(async (isPoll = false) => {
        if (!isPoll) setLoading(true);
        else setPolling(true);

        try {
            const res = await tasks.detail(tid);
            if (res.code === 0) {
                // apiserver 返回 { data: { task: {...}, files: [...] } }
                const taskData = res.data?.task || res.data;
                setTask(taskData);
                setError('');

                // W4: 提取产物文件列表，查找火焰图 SVG
                const files = res.data?.files || [];
                setFlameFiles(files);
                const svgFile = files.find(f =>
                    f.name && (f.name.endsWith('.svg') || f.name.includes('flamegraph'))
                );
                if (svgFile?.download_url) {
                    setFlameSvgUrl(svgFile.download_url);
                }
            } else {
                if (!isPoll) setError(res.message || '任务不存在');
            }
        } catch (err) {
            if (!isPoll) setError('加载任务详情失败: ' + (err.message || '未知错误'));
        } finally {
            setLoading(false);
            setPolling(false);
        }
    }, [tid]);

    // W4: 单独加载产物文件列表（用于分析完成后轮询）
    const loadFiles = useCallback(async () => {
        setAnalysisPolling(true);
        try {
            const res = await cosfiles.list(tid);
            if (res.code === 0) {
                const files = res.data?.files || [];
                setFlameFiles(files);
                const svgFile = files.find(f =>
                    f.name && (f.name.endsWith('.svg') || f.name.includes('flamegraph'))
                );
                if (svgFile?.download_url) {
                    setFlameSvgUrl(svgFile.download_url);
                }
            }
        } catch (err) {
            console.error('加载文件列表失败:', err);
        } finally {
            setAnalysisPolling(false);
        }
    }, [tid]);

    // 初始加载
    useEffect(() => {
        if (!tid) {
            setError('缺少任务 ID 参数');
            setLoading(false);
            return;
        }
        loadTask();
    }, [tid, loadTask]);

    // W3: 任务未完成时 3 秒轮询
    useEffect(() => {
        if (!task || task.status >= 2) return;  // 已完成/失败 → 停止轮询

        const interval = setInterval(() => {
            loadTask(true);
        }, 3000);

        return () => clearInterval(interval);
    }, [task?.status, loadTask]);

    // W4: 任务已完成但无火焰图 → 每 5 秒轮询分析结果
    useEffect(() => {
        if (!task || task.status !== 2) return;       // 非完成状态不轮询
        if (flameSvgUrl) return;                       // 已有火焰图，停止

        const interval = setInterval(() => {
            loadFiles();
        }, 5000);

        return () => clearInterval(interval);
    }, [task?.status, flameSvgUrl, loadFiles]);

    if (loading) return <div style={styles.container}><p style={styles.loading}>⏳ 加载中...</p></div>;
    if (error) return <div style={styles.container}><p style={styles.error}>{error}</p></div>;
    if (!task) return <div style={styles.container}><p style={styles.error}>任务不存在</p></div>;

    const statusColor = statusColors[task.status] || '#999';
    const statusName = statusNames[task.status] || '未知';
    const isRunning = task.status < 2;

    return (
        <div style={styles.container}>
            <h2>任务详情: {tid}</h2>

            {/* W3: 轮询状态提示 */}
            {isRunning && (
                <div style={styles.pollingBar}>
                    <span>🔄</span>
                    <span>任务执行中，每 3 秒自动刷新状态...</span>
                    {polling && <span style={{ fontSize: 11, opacity: 0.7 }}>刷新中</span>}
                </div>
            )}

            {/* W3: 状态进度条 */}
            <div style={styles.progressBar}>
                {statusSteps.map((step, i) => (
                    <div key={step.key} style={styles.progressStep(
                        task.status === step.key,
                        task.status > step.key
                    )}>
                        {step.label}
                    </div>
                ))}
                {task.status === 3 && (
                    <div style={styles.progressStep(true, false)}>❌ 失败</div>
                )}
            </div>

            {/* ===== 基本信息 ===== */}
            <div style={styles.card}>
                <h3>基本信息</h3>
                <table style={styles.table}>
                    <tbody>
                        <tr>
                            <td style={{ ...styles.td, fontWeight: 'bold', width: 120 }}>任务名称</td>
                            <td style={styles.td}>{task.name || '-'}</td>
                        </tr>
                        <tr>
                            <td style={{ ...styles.td, fontWeight: 'bold' }}>状态</td>
                            <td style={styles.td}>
                                <span style={{ ...styles.badge, background: statusColor, color: '#fff' }}>{statusName}</span>
                                {task.status_info && <span style={{ marginLeft: 8, color: '#999', fontSize: 13 }}>({task.status_info})</span>}
                            </td>
                        </tr>
                        <tr>
                            <td style={{ ...styles.td, fontWeight: 'bold' }}>目标 IP</td>
                            <td style={styles.td}>{task.target_ip || '-'}</td>
                        </tr>
                        <tr>
                            <td style={{ ...styles.td, fontWeight: 'bold' }}>创建时间</td>
                            <td style={styles.td}>{task.create_time || '-'}</td>
                        </tr>
                        <tr>
                            <td style={{ ...styles.td, fontWeight: 'bold' }}>开始时间</td>
                            <td style={styles.td}>{task.begin_time || (task.status >= 1 ? '已开始' : '等待中')}</td>
                        </tr>
                        <tr>
                            <td style={{ ...styles.td, fontWeight: 'bold' }}>结束时间</td>
                            <td style={styles.td}>{task.end_time || (task.status >= 2 ? '已完成' : '进行中')}</td>
                        </tr>
                        <tr>
                            <td style={{ ...styles.td, fontWeight: 'bold' }}>采集参数</td>
                            <td style={styles.td}>
                                {task.request_params ? JSON.stringify(task.request_params) : '-'}
                            </td>
                        </tr>
                    </tbody>
                </table>
            </div>

            {/* ===== 火焰图 ===== */}
            <h3>🔥 火焰图</h3>
            <div style={styles.flameBox}>
                {flameSvgUrl ? (
                    // W4: 有火焰图 → iframe 加载 SVG（支持鼠标交互）
                    <div>
                        <iframe
                            src={flameSvgUrl}
                            title="火焰图"
                            style={{
                                width: '100%', height: 500, border: '1px solid #e0e0e0',
                                borderRadius: 4, background: '#fff',
                            }}
                        />
                        <p style={{ fontSize: 12, color: '#888', marginTop: 8 }}>
                            💡 点击函数框可放大，右键可缩小
                        </p>
                    </div>
                ) : task.status === 2 ? (
                    // 任务完成但分析结果未出
                    <div>
                        <p style={{ fontSize: 48, margin: '0 0 16px 0' }}>🔬</p>
                        <p>任务采集已完成，正在等待分析引擎生成火焰图...</p>
                        {analysisPolling && (
                            <p style={{ fontSize: 13, color: '#1565c0', marginTop: 8 }}>
                                🔄 每 5 秒检查分析结果...
                            </p>
                        )}
                        {flameFiles.length > 0 && (
                            <div style={{ marginTop: 12, textAlign: 'left', maxWidth: 500, margin: '12px auto' }}>
                                <p style={{ fontSize: 13, fontWeight: 'bold' }}>已有产物文件：</p>
                                {flameFiles.map(f => (
                                    <div key={f.name} style={{ fontSize: 12, padding: '4px 0', color: '#666' }}>
                                        📄 {f.name} ({formatSize(f.size)})
                                        {f.download_url && (
                                            <a href={f.download_url} target="_blank" rel="noreferrer"
                                                style={{ marginLeft: 8, color: '#4a6cf7', fontSize: 11 }}>
                                                下载
                                            </a>
                                        )}
                                    </div>
                                ))}
                            </div>
                        )}
                        {flameFiles.length === 0 && !analysisPolling && (
                            <p style={{ fontSize: 12, color: '#999', marginTop: 8 }}>
                                暂无产物文件（analysis 服务未运行或尚未产出）
                            </p>
                        )}
                    </div>
                ) : task.status === 3 ? (
                    <div>
                        <p style={{ fontSize: 48, margin: '0 0 16px 0' }}>❌</p>
                        <p>任务执行失败，无法生成火焰图</p>
                        {task.status_info && <p style={{ fontSize: 13, color: '#f44336' }}>原因: {task.status_info}</p>}
                    </div>
                ) : (
                    <div>
                        <p style={{ fontSize: 48, margin: '0 0 16px 0' }}>⏳</p>
                        <p>任务执行中，火焰图将在采集完成后自动生成</p>
                    </div>
                )}
            </div>

            {/* ===== 热点 TopN ===== */}
            <h3>🔥 热点 TopN</h3>
            <div style={styles.card}>
                {task.top_functions && task.top_functions.length > 0 ? (
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
                            {task.top_functions.map((f, i) => (
                                <tr key={i}>
                                    <td style={styles.td}>{i + 1}</td>
                                    <td style={styles.td}>{f.name}</td>
                                    <td style={styles.td}>{f.samples}</td>
                                    <td style={styles.td}>{f.percent}%</td>
                                </tr>
                            ))}
                        </tbody>
                    </table>
                ) : (
                    <p style={{ textAlign: 'center', padding: 40, color: '#999' }}>
                        {task.status >= 2 ? '暂无热点数据，分析完成后将显示' : '任务完成后将自动分析热点函数'}
                    </p>
                )}
            </div>
        </div>
    );
}

// W4: 文件大小格式化工具
function formatSize(bytes) {
    if (!bytes || bytes === 0) return '0 B';
    const units = ['B', 'KB', 'MB', 'GB'];
    let i = 0;
    let size = bytes;
    while (size >= 1024 && i < units.length - 1) {
        size /= 1024;
        i++;
    }
    return size.toFixed(1) + ' ' + units[i];
}
