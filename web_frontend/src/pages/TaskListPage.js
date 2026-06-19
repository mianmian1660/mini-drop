// ============================================================
// pages/TaskListPage.js — 全部任务列表页（/tasks）
// ============================================================
// 功能：全部任务表格 + 搜索 + 删除
// ============================================================

import React, { useState, useEffect } from 'react';
import { Link } from 'react-router-dom';
import { tasks } from '../api';

const styles = {
    container: { maxWidth: 1200, margin: '0 auto', padding: 20, fontFamily: 'Arial, sans-serif' },
    card: { background: '#fff', borderRadius: 8, padding: 24, marginBottom: 16, boxShadow: '0 2px 8px rgba(0,0,0,0.1)' },
    table: { width: '100%', borderCollapse: 'collapse' },
    th: { textAlign: 'left', padding: '12px 16px', borderBottom: '2px solid #e0e0e0', color: '#666', fontSize: 13 },
    td: { padding: '12px 16px', borderBottom: '1px solid #f0f0f0', fontSize: 14 },
    badge: { padding: '2px 8px', borderRadius: 10, fontSize: 12, fontWeight: 'bold' },
    input: { width: '100%', padding: '8px 12px', border: '1px solid #ddd', borderRadius: 4, fontSize: 14 },
    empty: { textAlign: 'center', padding: 40, color: '#999' },
    loading: { textAlign: 'center', padding: 40, color: '#999' },
    deleteBtn: { color: '#f44336', cursor: 'pointer', background: 'none', border: 'none', fontSize: 14 },
};

const statusColors = { 0: '#ffc107', 1: '#2196f3', 2: '#4caf50', 3: '#f44336' };
const statusNames = { 0: '待处理', 1: '执行中', 2: '已完成', 3: '失败' };

export default function TaskListPage() {
    const [taskList, setTaskList] = useState([]);
    const [search, setSearch] = useState('');
    const [loading, setLoading] = useState(true);

    useEffect(() => {
        loadTasks();
    }, []);

    const loadTasks = async () => {
        setLoading(true);
        try {
            const res = await tasks.list();
            if (res.code === 0) {
                setTaskList(res.data?.tasks || []);
            }
        } catch (err) {
            console.error('加载任务列表失败:', err);
        } finally {
            setLoading(false);
        }
    };

    // 软删除任务
    const handleDelete = async (tid) => {
        if (!window.confirm(`确定删除任务 ${tid} 吗？`)) return;
        try {
            await tasks.delete(tid);
            // 从列表中移除
            setTaskList(prev => prev.filter(t => t.tid !== tid));
        } catch (err) {
            console.error('删除失败:', err);
            alert('删除失败: ' + (err.message || '未知错误'));
        }
    };

    // 前端搜索过滤
    const filtered = taskList.filter(t => {
        if (!search) return true;
        const q = search.toLowerCase();
        return (t.tid || '').toLowerCase().includes(q) ||
            (t.name || '').toLowerCase().includes(q) ||
            (t.target_ip || '').toLowerCase().includes(q);
    });

    if (loading) {
        return <div style={styles.container}><p style={styles.loading}>⏳ 加载中...</p></div>;
    }

    return (
        <div style={styles.container}>
            <h2>全部任务</h2>
            <div style={styles.card}>
                <input
                    style={{ ...styles.input, marginBottom: 16 }}
                    placeholder="搜索任务 ID / 名称 / IP..."
                    value={search}
                    onChange={e => setSearch(e.target.value)}
                />
                {filtered.length === 0 ? (
                    <p style={styles.empty}>{search ? '没有匹配的任务' : '暂无任务'}</p>
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
                            {filtered.map(t => (
                                <tr key={t.tid}>
                                    <td style={styles.td}>{t.tid}</td>
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
            </div>
        </div>
    );
}
