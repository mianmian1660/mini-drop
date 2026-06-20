// ============================================================
// pages/TaskListPage.js — 全部任务列表页（/tasks）
// ============================================================
// 功能：全部任务表格 + 后端搜索 + 分页 + 删除
// ============================================================

import React, { useState, useEffect, useCallback } from 'react';
import { Link } from 'react-router-dom';
import { tasks } from '../api';
import Pagination from '../components/Pagination';

const styles = {
    container: { maxWidth: 1200, margin: '0 auto', padding: 20, fontFamily: 'Arial, sans-serif' },
    card: { background: '#fff', borderRadius: 8, padding: 24, marginBottom: 16, boxShadow: '0 2px 8px rgba(0,0,0,0.1)' },
    table: { width: '100%', borderCollapse: 'collapse' },
    th: { textAlign: 'left', padding: '12px 16px', borderBottom: '2px solid #e0e0e0', color: '#666', fontSize: 13 },
    td: { padding: '12px 16px', borderBottom: '1px solid #f0f0f0', fontSize: 14 },
    badge: { padding: '2px 8px', borderRadius: 10, fontSize: 12, fontWeight: 'bold' },
    searchRow: { display: 'flex', gap: 12, marginBottom: 16 },
    input: { flex: 1, padding: '8px 12px', border: '1px solid #ddd', borderRadius: 4, fontSize: 14 },
    select: { padding: '8px 12px', border: '1px solid #ddd', borderRadius: 4, fontSize: 14, background: '#fff' },
    empty: { textAlign: 'center', padding: 40, color: '#999' },
    loading: { textAlign: 'center', padding: 40, color: '#999' },
    deleteBtn: { color: '#f44336', cursor: 'pointer', background: 'none', border: 'none', fontSize: 14 },
    toolbar: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 12 },
    totalInfo: { fontSize: 13, color: '#999' },
};

const statusColors = { 0: '#ffc107', 1: '#2196f3', 2: '#4caf50', 3: '#f44336', 4: '#7c3aed' };
const statusNames = { 0: '待处理', 1: '执行中', 2: '已完成', 3: '失败', 4: '上传中' };

export default function TaskListPage() {
    const [taskList, setTaskList] = useState([]);
    const [keyword, setKeyword] = useState('');       // 搜索输入框的值
    const [searchText, setSearchText] = useState(''); // 实际发起搜索的值（按回车触发）
    const [statusFilter, setStatusFilter] = useState('');
    const [page, setPage] = useState(1);
    const [pageSize] = useState(10);                  // 每页 10 条，方便看到分页效果
    const [total, setTotal] = useState(0);
    const [loading, setLoading] = useState(true);

    const totalPages = Math.max(1, Math.ceil(total / pageSize));

    // 用 useCallback 包装 loadTasks，便于 useEffect 依赖
    const loadTasks = useCallback(async () => {
        setLoading(true);
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
        } finally {
            setLoading(false);
        }
    }, [page, pageSize, searchText, statusFilter]);

    // page / searchText / statusFilter 变化时重新加载
    useEffect(() => {
        loadTasks();
    }, [loadTasks]);

    // W3: 任务列表自动刷新（10 秒轮询）
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
            setPage(1);  // 搜索后回到第 1 页
        }
    };

    // 状态筛选变化
    const handleStatusChange = (e) => {
        setStatusFilter(e.target.value);
        setPage(1);
    };

    // 翻页
    const handlePageChange = (newPage) => {
        setPage(newPage);
    };

    // 软删除任务
    const handleDelete = async (tid) => {
        if (!window.confirm(`确定删除任务 ${tid} 吗？`)) return;
        try {
            await tasks.delete(tid);
            // 删除后重新加载当前页
            loadTasks();
        } catch (err) {
            console.error('删除失败:', err);
            alert('删除失败: ' + (err.message || '未知错误'));
        }
    };

    if (loading && taskList.length === 0) {
        return <div style={styles.container}><p style={styles.loading}>⏳ 加载中...</p></div>;
    }

    return (
        <div style={styles.container}>
            <h2>全部任务</h2>

            {/* 搜索栏 + 状态筛选 */}
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
                        <option value="4">上传中</option>
                        <option value="2">已完成</option>
                        <option value="3">失败</option>
                    </select>
                </div>

                {/* 工具栏：总数 + 页码信息 */}
                <div style={styles.toolbar}>
                    <span style={styles.totalInfo}>共 {total} 条任务</span>
                    {searchText && (
                        <span style={{ ...styles.totalInfo, color: '#4a6cf7' }}>
                            搜索: "{searchText}"
                            <button
                                style={{ ...styles.deleteBtn, marginLeft: 8, fontSize: 12 }}
                                onClick={() => { setKeyword(''); setSearchText(''); setPage(1); }}
                            >
                                清除
                            </button>
                        </span>
                    )}
                </div>

                {taskList.length === 0 ? (
                    <p style={styles.empty}>{searchText ? '没有匹配的任务' : '暂无任务'}</p>
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
                                        <Link to={`/task/result?tid=${t.tid}`} style={{ color: '#4a6cf7', marginRight: 12 }}>查看</Link>
                                        <button style={styles.deleteBtn} onClick={() => handleDelete(t.tid)}>删除</button>
                                    </td>
                                </tr>
                            ))}
                        </tbody>
                    </table>
                )}

                {/* 分页组件 */}
                <Pagination page={page} totalPages={totalPages} onPageChange={handlePageChange} />
            </div>
        </div>
    );
}
