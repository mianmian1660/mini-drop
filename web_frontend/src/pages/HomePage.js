// ============================================================
// pages/HomePage.js — 主页（/）
// ============================================================
// 功能：Agent 列表 + 我的任务列表（含搜索/分页） + 新建任务按钮
// ============================================================

import React, { useState, useEffect, useCallback } from 'react';
import { Link } from 'react-router-dom';
import { agents, tasks } from '../api';
import CreateTaskModal from '../components/CreateTaskModal';
import Pagination from '../components/Pagination';

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
    btn: { background: '#315efb', color: '#fff', border: 'none', padding: '10px 18px', borderRadius: 6, cursor: 'pointer', fontSize: 14, fontWeight: 700 },
    table: { width: '100%', borderCollapse: 'collapse' },
    th: { textAlign: 'left', padding: '12px 16px', borderBottom: '1px solid #d0d7de', color: '#475467', fontSize: 12, background: '#f8fafc' },
    td: { padding: '12px 16px', borderBottom: '1px solid #edf0f3', fontSize: 14, verticalAlign: 'top' },
    badge: { display: 'inline-flex', alignItems: 'center', padding: '3px 9px', borderRadius: 999, fontSize: 12, fontWeight: 'bold' },
    empty: { textAlign: 'center', padding: 40, color: '#999' },
    loading: { textAlign: 'center', padding: 40, color: '#999' },
    searchRow: { display: 'flex', gap: 12, marginBottom: 16 },
    input: { flex: 1, padding: '8px 12px', border: '1px solid #ddd', borderRadius: 4, fontSize: 14 },
    select: { padding: '8px 12px', border: '1px solid #ddd', borderRadius: 4, fontSize: 14, background: '#fff' },
    toolbar: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 12 },
    totalInfo: { fontSize: 13, color: '#999' },
    sectionHead: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 12, margin: '24px 0 12px' },
    sectionTitle: { margin: 0, fontSize: 22 },
    subtle: { color: '#667085', fontSize: 12 },
    auditList: { display: 'grid', gap: 10 },
    auditItem: { display: 'grid', gridTemplateColumns: '130px minmax(120px, 1fr) 120px minmax(0, 2fr)', gap: 12, padding: '10px 12px', background: '#fbfcfe', border: '1px solid #edf0f3', borderRadius: 6, fontSize: 13 },
};

const statusColors = { 0: '#ffc107', 1: '#2196f3', 2: '#4caf50', 3: '#f44336' };
const statusNames = { 0: '待处理', 1: '执行中', 2: '已完成', 3: '失败' };

export default function HomePage() {
    const [agentList, setAgentList] = useState([]);
    const [taskList, setTaskList] = useState([]);
    const [auditList, setAuditList] = useState([]);
    const [showCreate, setShowCreate] = useState(false);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState('');

    // 任务列表的分页 & 搜索状态
    const [keyword, setKeyword] = useState('');
    const [searchText, setSearchText] = useState('');
    const [statusFilter, setStatusFilter] = useState('');
    const [page, setPage] = useState(1);
    const [pageSize] = useState(5);   // 主页任务列表每页 5 条
    const [total, setTotal] = useState(0);
    const totalPages = Math.max(1, Math.ceil(total / pageSize));
    const onlineAgents = agentList.filter(a => a.online).length;
    const runningTasks = taskList.filter(t => t.status === 1).length;
    const failedTasks = taskList.filter(t => t.status === 3).length;

    // 加载 Agent 列表（不分页）
    const loadAgents = async () => {
        try {
            const res = await agents.list();
            if (res.code === 0) {
                setAgentList(res.data?.agents || []);
            }
        } catch (err) {
            console.error('加载 Agent 列表失败:', err);
        }
    };

    const loadAudits = async () => {
        try {
            const res = await agents.audits({ limit: 8 });
            if (res.code === 0) {
                setAuditList(res.data?.audits || []);
            }
        } catch (err) {
            console.error('加载 Agent 审计日志失败:', err);
        }
    };

    // 加载任务列表（分页+搜索）
    const loadTasks = useCallback(async () => {
        try {
            const res = await tasks.list({
                page,
                pageSize,
                keyword: searchText,
                status: statusFilter || undefined,
            });
            if (res.code === 0) {
                setTaskList(res.data?.tasks || []);
                setTotal(res.data?.total || 0);
            }
        } catch (err) {
            console.error('加载任务列表失败:', err);
        }
    }, [page, pageSize, searchText, statusFilter]);

    // 组件加载 + 搜索/翻页变化时拉数据
    useEffect(() => {
        setLoading(true);
        setError('');
        Promise.all([loadAgents(), loadTasks(), loadAudits()])
            .catch(err => {
                console.error('加载数据失败:', err);
                setError('无法连接到后端服务，请确认 apiserver 已启动');
            })
            .finally(() => setLoading(false));
    }, [loadTasks]);

    // W3: 任务列表自动刷新（10 秒轮询，检测状态变化）
    useEffect(() => {
        const interval = setInterval(() => {
            loadTasks();
        }, 10000);
        return () => clearInterval(interval);
    }, [loadTasks]);

    // 按回车触发搜索
    const handleSearchKeyDown = (e) => {
        if (e.key === 'Enter') {
            setSearchText(keyword.trim());
            setPage(1);
        }
    };

    // 状态筛选
    const handleStatusChange = (e) => {
        setStatusFilter(e.target.value);
        setPage(1);
    };

    // 任务创建成功
    const handleTaskCreated = () => {
        setShowCreate(false);
        setPage(1);
        setSearchText('');
        setKeyword('');
        // 重新加载
        Promise.all([loadAgents(), loadTasks(), loadAudits()]).finally(() => setLoading(false));
    };

    if (loading) {
        return <div style={styles.container}><p style={styles.loading}>⏳ 加载中...</p></div>;
    }

    return (
        <div style={styles.container}>
            <div style={styles.pageHead}>
                <div>
                    <p style={styles.eyebrow}>Mini-Drop 控制台</p>
                    <h2 style={styles.title}>性能采集与分析</h2>
                </div>
                <button style={styles.btn} onClick={() => setShowCreate(!showCreate)}>
                    {showCreate ? '取消' : '+ 新建采样'}
                </button>
            </div>

            <div style={styles.metricGrid}>
                <Metric label="在线 Agent" value={`${onlineAgents}/${agentList.length}`} />
                <Metric label="当前页运行任务" value={runningTasks} />
                <Metric label="当前页失败任务" value={failedTasks} />
                <Metric label="任务总数" value={total} />
            </div>

            {error && (
                <div style={{ ...styles.card, background: '#fff3f3', border: '1px solid #ffcdd2' }}>
                    <p style={{ color: '#d32f2f', margin: 0 }}>⚠️ {error}</p>
                </div>
            )}

            {/* ===== Agent 列表 ===== */}
            <div style={styles.sectionHead}>
                <h2 style={styles.sectionTitle}>Agent 列表</h2>
                <span style={styles.subtle}>每 5s 心跳；超过 30s 未探测到会判定离线并写审计</span>
            </div>
            <div style={styles.card}>
                {agentList.length === 0 ? (
                    <p style={styles.empty}>暂无 Agent 在线，请先启动 drop_agent</p>
                ) : (
                    <table style={styles.table}>
                        <thead>
                            <tr>
                                <th style={styles.th}>主机名</th>
                                <th style={styles.th}>IP 地址</th>
                                <th style={styles.th}>状态</th>
                                <th style={styles.th}>版本</th>
                                <th style={styles.th}>最后心跳</th>
                            </tr>
                        </thead>
                        <tbody>
                            {agentList.map(a => (
                                <tr key={a.id}>
                                    <td style={styles.td}>{a.hostname}</td>
                                    <td style={styles.td}>{a.ip_addr}</td>
                                    <td style={styles.td}>
                                        <span style={{ ...styles.badge, background: a.online ? '#4caf50' : '#f44336', color: '#fff' }}>
                                            {a.online ? '在线' : '离线'}
                                        </span>
                                    </td>
                                    <td style={styles.td}>{a.version}</td>
                                    <td style={styles.td}>{formatTime(a.last_seen) || '-'}</td>
                                </tr>
                            ))}
                        </tbody>
                    </table>
                )}
            </div>

            <div style={styles.sectionHead}>
                <h2 style={styles.sectionTitle}>Agent 审计日志</h2>
                <span style={styles.subtle}>记录注册、离线、恢复原因</span>
            </div>
            <div style={styles.card}>
                {auditList.length === 0 ? (
                    <p style={styles.empty}>暂无 Agent 审计日志</p>
                ) : (
                    <div style={styles.auditList}>
                        {auditList.map(a => (
                            <div key={a.id} style={styles.auditItem}>
                                <span>{formatTime(a.created_at)}</span>
                                <strong>{a.ip_addr}</strong>
                                <span style={{ ...styles.badge, background: auditColor(a.event), color: '#fff', justifyContent: 'center' }}>{auditName(a.event)}</span>
                                <span>{a.reason || '-'}</span>
                            </div>
                        ))}
                    </div>
                )}
            </div>

            {/* ===== 任务列表标题 + 新建按钮 ===== */}
            <div style={styles.sectionHead}>
                <h2 style={styles.sectionTitle}>任务列表</h2>
                <span style={styles.subtle}>状态 reason 来自最近一次状态迁移</span>
            </div>

            {showCreate && <CreateTaskModal onClose={() => setShowCreate(false)} onSuccess={handleTaskCreated} />}

            {/* ===== 任务搜索 + 筛选 ===== */}
            <div style={styles.card}>
                <div style={styles.searchRow}>
                    <input
                        style={styles.input}
                        placeholder="搜索任务名称 / ID / IP（回车搜索）"
                        value={keyword}
                        onChange={e => setKeyword(e.target.value)}
                        onKeyDown={handleSearchKeyDown}
                    />
                    <select style={styles.select} value={statusFilter} onChange={handleStatusChange}>
                        <option value="">全部状态</option>
                        <option value="0">待处理</option>
                        <option value="1">执行中</option>
                        <option value="2">已完成</option>
                        <option value="3">失败</option>
                    </select>
                </div>

                <div style={styles.toolbar}>
                    <span style={styles.totalInfo}>共 {total} 条任务</span>
                    {searchText && (
                        <span style={{ ...styles.totalInfo, color: '#4a6cf7' }}>
                            搜索: "{searchText}"
                            <button
                                style={{ color: '#f44336', cursor: 'pointer', background: 'none', border: 'none', fontSize: 12, marginLeft: 8 }}
                                onClick={() => { setKeyword(''); setSearchText(''); setPage(1); }}
                            >
                                清除
                            </button>
                        </span>
                    )}
                </div>

                {/* ===== 任务表格 ===== */}
                {taskList.length === 0 ? (
                    <p style={styles.empty}>{searchText ? '没有匹配的任务' : '暂无任务，点击"新建采样"开始'}</p>
                ) : (
                    <table style={styles.table}>
                        <thead>
                            <tr>
                                <th style={styles.th}>任务ID</th>
                                <th style={styles.th}>名称</th>
                                <th style={styles.th}>目标IP</th>
                                <th style={styles.th}>状态</th>
                                <th style={styles.th}>Reason</th>
                                <th style={styles.th}>创建时间</th>
                                <th style={styles.th}>操作</th>
                            </tr>
                        </thead>
                        <tbody>
                            {taskList.map(t => (
                                <tr key={t.tid}>
                                    <td style={{ ...styles.td, fontSize: 12, color: '#888' }}>{t.tid}</td>
                                    <td style={styles.td}>{t.name}</td>
                                    <td style={styles.td}>{t.target_ip}</td>
                                    <td style={styles.td}>
                                        <span style={{ ...styles.badge, background: statusColors[t.status] || '#999', color: '#fff' }}>
                                            {statusNames[t.status] || '未知'}
                                        </span>
                                    </td>
                                    <td style={{ ...styles.td, color: '#667085', maxWidth: 260 }}>{t.status_info || '-'}</td>
                                    <td style={styles.td}>{formatTime(t.create_time)}</td>
                                    <td style={styles.td}>
                                        <Link to={`/task/result?tid=${t.tid}`} style={{ color: '#4a6cf7' }}>查看</Link>
                                    </td>
                                </tr>
                            ))}
                        </tbody>
                    </table>
                )}

                {/* 分页 */}
                <Pagination page={page} totalPages={totalPages} onPageChange={setPage} />
            </div>
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

function formatTime(value) {
    if (!value) return '';
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return String(value);
    return date.toLocaleString();
}

function auditName(event) {
    if (event === 'registered') return '注册';
    if (event === 'offline') return '离线';
    if (event === 'recovered') return '恢复';
    return event || '事件';
}

function auditColor(event) {
    if (event === 'offline') return '#dc2626';
    if (event === 'recovered') return '#16a34a';
    if (event === 'registered') return '#2563eb';
    return '#64748b';
}
