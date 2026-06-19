// ============================================================
// pages/HomePage.js — 主页（/）
// ============================================================
// 功能：Agent 列表 + 我的任务列表 + 新建任务按钮
// 接真实 apiserver API，不再使用 mock 数据
// ============================================================

import React, { useState, useEffect } from 'react';
import { Link } from 'react-router-dom';
import { agents, tasks } from '../api';
import CreateTaskModal from '../components/CreateTaskModal';

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
};

const statusColors = { 0: '#ffc107', 1: '#2196f3', 2: '#4caf50', 3: '#f44336' };
const statusNames = { 0: '待处理', 1: '执行中', 2: '已完成', 3: '失败' };

export default function HomePage() {
    const [agentList, setAgentList] = useState([]);
    const [taskList, setTaskList] = useState([]);
    const [showCreate, setShowCreate] = useState(false);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState('');

    // 组件加载时从后端拉数据
    useEffect(() => {
        loadData();
    }, []);

    const loadData = async () => {
        setLoading(true);
        setError('');
        try {
            const [agentRes, taskRes] = await Promise.all([
                agents.list(),
                tasks.list(),
            ]);
            // 后端返回格式: { code: 0, data: { agents: [...], total: N } }
            if (agentRes.code === 0) {
                setAgentList(agentRes.data?.agents || []);
            }
            if (taskRes.code === 0) {
                setTaskList(taskRes.data?.tasks || []);
            }
        } catch (err) {
            console.error('加载数据失败:', err);
            setError('无法连接到后端服务，请确认 apiserver 已启动');
        } finally {
            setLoading(false);
        }
    };

    // 任务创建成功后刷新列表
    const handleTaskCreated = () => {
        setShowCreate(false);
        loadData();
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

            {/* ===== 任务表格 ===== */}
            <div style={styles.card}>
                {taskList.length === 0 ? (
                    <p style={styles.empty}>暂无任务，点击"新建采样"开始</p>
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
                                        <Link to={`/task/result?tid=${t.tid}`} style={{ color: '#4a6cf7' }}>查看</Link>
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
