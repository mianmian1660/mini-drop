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
    container: { maxWidth: 1200, margin: '0 auto', padding: 20, fontFamily: 'Arial, sans-serif' },
    card: { background: '#fff', borderRadius: 8, padding: 24, marginBottom: 16, boxShadow: '0 2px 8px rgba(0,0,0,0.1)' },
    btn: { background: '#4a6cf7', color: '#fff', border: 'none', padding: '10px 20px', borderRadius: 6, cursor: 'pointer', fontSize: 14 },
    table: { width: '100%', borderCollapse: 'collapse' },
    th: { textAlign: 'left', padding: '12px 16px', borderBottom: '2px solid #e0e0e0', color: '#666', fontSize: 13 },
    td: { padding: '12px 16px', borderBottom: '1px solid #f0f0f0', fontSize: 14 },
    badge: { padding: '2px 8px', borderRadius: 10, fontSize: 12, fontWeight: 'bold' },
    empty: { textAlign: 'center', padding: 40, color: '#999' },
    loading: { textAlign: 'center', padding: 40, color: '#999' },
    searchRow: { display: 'flex', gap: 12, marginBottom: 16 },
    input: { flex: 1, padding: '8px 12px', border: '1px solid #ddd', borderRadius: 4, fontSize: 14 },
    select: { padding: '8px 12px', border: '1px solid #ddd', borderRadius: 4, fontSize: 14, background: '#fff' },
    toolbar: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 12 },
    totalInfo: { fontSize: 13, color: '#999' },
};

const statusColors = { 0: '#ffc107', 1: '#2196f3', 2: '#4caf50', 3: '#f44336' };
const statusNames = { 0: '待处理', 1: '执行中', 2: '已完成', 3: '失败' };

export default function HomePage() {
    const [agentList, setAgentList] = useState([]);
    const [taskList, setTaskList] = useState([]);
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
        Promise.all([loadAgents(), loadTasks()])
            .catch(err => {
                console.error('加载数据失败:', err);
                setError('无法连接到后端服务，请确认 apiserver 已启动');
            })
            .finally(() => setLoading(false));
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
        Promise.all([loadAgents(), loadTasks()]).finally(() => setLoading(false));
    };

    if (loading) {
        return <div style={styles.container}><p style={styles.loading}>⏳ 加载中...</p></div>;
    }

    return (
        <div style={styles.container}>
            {error && (
                <div style={{ ...styles.card, background: '#fff3f3', border: '1px solid #ffcdd2' }}>
                    <p style={{ color: '#d32f2f', margin: 0 }}>⚠️ {error}</p>
                </div>
            )}

            {/* ===== Agent 列表 ===== */}
            <h2>Agent 列表</h2>
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
                                    <td style={styles.td}>{a.last_seen || '-'}</td>
                                </tr>
                            ))}
                        </tbody>
                    </table>
                )}
            </div>

            {/* ===== 任务列表标题 + 新建按钮 ===== */}
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                <h2>任务列表</h2>
                <button style={styles.btn} onClick={() => setShowCreate(!showCreate)}>
                    {showCreate ? '取消' : '+ 新建采样'}
                </button>
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
                                    <td style={styles.td}>{t.create_time}</td>
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
