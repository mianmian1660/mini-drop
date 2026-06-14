// ============================================================
// App.js — React 主组件 小白版注释
// ============================================================
// 这个文件包含整个前端的所有页面和逻辑
// 分为 4 个部分：
//   1. 全局样式（CSS-in-JS，写在 JS 里的样式）
//   2. 主页组件（HomePage）    — Agent 列表 + 任务列表 + 新建任务
//   3. 任务列表页（TaskListPage）— 全部任务表格
//   4. 任务详情页（TaskResultPage）— 火焰图 + 热点 TopN
//
// React 语法小课堂：
//   function Xxx() { return (<div>...</div>); }  = 函数组件
//   useState(初始值)   = React Hook，创建"状态"变量
//   useEffect(fn, [])  = React Hook，组件加载时执行 fn
//   <Link to="/path">  = 路由链接（类似 <a> 标签，但不刷新页面）
// ============================================================

import React, { useState, useEffect } from 'react';
import { Routes, Route, Link } from 'react-router-dom';

// ============================================================
// CSS 样式（写在一个 JS 对象里，不用单独的 .css 文件）
// 优点：样式和组件在一起，方便管理
// ============================================================
const styles = {
    container: { maxWidth: 1200, margin: '0 auto', padding: 20, fontFamily: 'Arial, sans-serif' },
    header: { background: '#1a1a2e', color: '#fff', padding: '16px 24px', display: 'flex', justifyContent: 'space-between', alignItems: 'center' },
    nav: { display: 'flex', gap: 20 },
    navLink: { color: '#a0a0c0', textDecoration: 'none', fontSize: 16 },
    card: { background: '#fff', borderRadius: 8, padding: 24, marginBottom: 16, boxShadow: '0 2px 8px rgba(0,0,0,0.1)' },
    btn: { background: '#4a6cf7', color: '#fff', border: 'none', padding: '10px 20px', borderRadius: 6, cursor: 'pointer', fontSize: 14 },
    table: { width: '100%', borderCollapse: 'collapse' },
    th: { textAlign: 'left', padding: '12px 16px', borderBottom: '2px solid #e0e0e0', color: '#666', fontSize: 13 },
    td: { padding: '12px 16px', borderBottom: '1px solid #f0f0f0', fontSize: 14 },
    input: { width: '100%', padding: '8px 12px', border: '1px solid #ddd', borderRadius: 4, fontSize: 14, marginBottom: 12 },
    label: { display: 'block', marginBottom: 4, fontWeight: 'bold', fontSize: 13, color: '#555' },
    badge: { padding: '2px 8px', borderRadius: 10, fontSize: 12, fontWeight: 'bold' },
};

// 任务状态 → 颜色和中文名映射
const statusColors = { 0: '#ffc107', 1: '#2196f3', 2: '#4caf50', 3: '#f44336' };
const statusNames = { 0: '待处理', 1: '执行中', 2: '已完成', 3: '失败' };

// ============================================================
// HomePage — 主页
// 路由：/
// 功能：显示 Agent 列表 + 任务列表 + 新建任务按钮
// ============================================================
function HomePage() {
    const [agents, setAgents] = useState([]);
    const [tasks, setTasks] = useState([]);
    const [showCreate, setShowCreate] = useState(false);

    // 组件加载时初始化数据
    useEffect(() => {
        setAgents([
            { id: 1, hostname: 'demo-host', ipAddr: '127.0.0.1', online: true, version: '1.0.0' },
        ]);
        setTasks([
            { tid: 'task-001', name: 'CPU采样', status: 2, targetIP: '127.0.0.1', createTime: '2026-06-14 10:00' },
            { tid: 'task-002', name: '内存分析', status: 0, targetIP: '127.0.0.1', createTime: '2026-06-14 10:30' },
        ]);
    }, []);

    return (
        <div style={styles.container}>
            {/* ===== Agent 列表 ===== */}
            <h2>Agent 列表</h2>
            <div style={styles.card}>
                <table style={styles.table}>
                    <thead>
                        <tr>
                            <th style={styles.th}>主机名</th>
                            <th style={styles.th}>IP 地址</th>
                            <th style={styles.th}>状态</th>
                            <th style={styles.th}>版本</th>
                        </tr>
                    </thead>
                    <tbody>
                        {agents.map(a => (
                            <tr key={a.id}>
                                <td style={styles.td}>{a.hostname}</td>
                                <td style={styles.td}>{a.ipAddr}</td>
                                <td style={styles.td}>
                                    <span style={{ ...styles.badge, background: a.online ? '#4caf50' : '#f44336', color: '#fff' }}>
                                        {a.online ? '在线' : '离线'}
                                    </span>
                                </td>
                                <td style={styles.td}>{a.version}</td>
                            </tr>
                        ))}
                    </tbody>
                </table>
            </div>

            {/* ===== 任务列表标题 + 新建按钮 ===== */}
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                <h2>任务列表</h2>
                <button style={styles.btn} onClick={() => setShowCreate(!showCreate)}>新建采样</button>
            </div>

            {showCreate && <CreateTaskModal onClose={() => setShowCreate(false)} />}

            {/* ===== 任务表格 ===== */}
            <div style={styles.card}>
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
                        {tasks.map(t => (
                            <tr key={t.tid}>
                                <td style={styles.td}>{t.tid}</td>
                                <td style={styles.td}>{t.name}</td>
                                <td style={styles.td}>{t.targetIP}</td>
                                <td style={styles.td}>
                                    <span style={{ ...styles.badge, background: statusColors[t.status], color: '#fff' }}>
                                        {statusNames[t.status]}
                                    </span>
                                </td>
                                <td style={styles.td}>{t.createTime}</td>
                                <td style={styles.td}>
                                    <Link to={`/task/result?tid=${t.tid}`} style={{ color: '#4a6cf7' }}>查看</Link>
                                </td>
                            </tr>
                        ))}
                    </tbody>
                </table>
            </div>
        </div>
    );
}

// ============================================================
// CreateTaskModal — 新建任务弹窗
// ============================================================
function CreateTaskModal({ onClose }) {
    const [form, setForm] = useState({ pid: '', duration: 10, hz: 99, targetIP: '127.0.0.1' });

    const handleSubmit = () => {
        alert(`创建任务: PID=${form.pid}, 时长=${form.duration}s, 频率=${form.hz}Hz`);
        onClose();
    };

    return (
        <div style={{ ...styles.card, background: '#f8f9ff' }}>
            <h3>新建采样任务</h3>
            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16 }}>
                <div>
                    <label style={styles.label}>目标 PID</label>
                    <input style={styles.input} type="number" value={form.pid}
                        onChange={e => setForm({ ...form, pid: e.target.value })} />
                </div>
                <div>
                    <label style={styles.label}>目标 IP</label>
                    <input style={styles.input} value={form.targetIP}
                        onChange={e => setForm({ ...form, targetIP: e.target.value })} />
                </div>
                <div>
                    <label style={styles.label}>采样时长（秒）</label>
                    <input style={styles.input} type="number" value={form.duration}
                        onChange={e => setForm({ ...form, duration: e.target.value })} />
                </div>
                <div>
                    <label style={styles.label}>采样频率（Hz）</label>
                    <input style={styles.input} type="number" value={form.hz}
                        onChange={e => setForm({ ...form, hz: e.target.value })} />
                </div>
            </div>
            <div style={{ marginTop: 16, display: 'flex', gap: 10 }}>
                <button style={styles.btn} onClick={handleSubmit}>提交任务</button>
                <button style={{ ...styles.btn, background: '#999' }} onClick={onClose}>取消</button>
            </div>
        </div>
    );
}

// ============================================================
// TaskResultPage — 任务详情页
// 路由：/task/result?tid=xxx
// ============================================================
function TaskResultPage() {
    const params = new URLSearchParams(window.location.search);
    const tid = params.get('tid');

    return (
        <div style={styles.container}>
            <h2>任务详情: {tid}</h2>
            <div style={styles.card}>
                <p><strong>状态:</strong> <span style={{ ...styles.badge, background: '#4caf50', color: '#fff' }}>已完成</span></p>
                <p><strong>目标:</strong> 127.0.0.1 / PID=1234</p>
                <p><strong>采样:</strong> 10秒 / 99Hz</p>
                <p><strong>创建时间:</strong> 2026-06-14 10:00:00</p>
            </div>

            <h3>🔥 火焰图</h3>
            <div style={{ ...styles.card, textAlign: 'center', padding: 40, background: '#f5f5fa', color: '#999' }}>
                <p style={{ fontSize: 48, margin: 0 }}>📊</p>
                <p>火焰图区域 — 连接到后端后此处将渲染真实火焰图</p>
                <p style={{ fontSize: 12 }}>（使用 d3-flame-graph 或 iframe 加载 SVG）</p>
            </div>

            <h3>🔥 热点 TopN</h3>
            <div style={styles.card}>
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
                        <tr><td style={styles.td}>1</td><td style={styles.td}>main()</td><td style={styles.td}>1000</td><td style={styles.td}>100%</td></tr>
                        <tr><td style={styles.td}>2</td><td style={styles.td}>processData()</td><td style={styles.td}>850</td><td style={styles.td}>85%</td></tr>
                        <tr><td style={styles.td}>3</td><td style={styles.td}>sortArray()</td><td style={styles.td}>620</td><td style={styles.td}>62%</td></tr>
                        <tr><td style={styles.td}>4</td><td style={styles.td}>malloc()</td><td style={styles.td}>340</td><td style={styles.td}>34%</td></tr>
                        <tr><td style={styles.td}>5</td><td style={styles.td}>printf()</td><td style={styles.td}>180</td><td style={styles.td}>18%</td></tr>
                    </tbody>
                </table>
            </div>
        </div>
    );
}

// ============================================================
// TaskListPage — 全部任务列表页
// 路由：/tasks
// ============================================================
function TaskListPage() {
    const tasks = [
        { tid: 'task-001', name: 'CPU采样', status: 2, targetIP: '127.0.0.1', createTime: '2026-06-14 10:00' },
        { tid: 'task-002', name: '内存分析', status: 1, targetIP: '127.0.0.1', createTime: '2026-06-14 10:30' },
        { tid: 'task-003', name: 'IO采集', status: 3, targetIP: '127.0.0.1', createTime: '2026-06-14 09:00' },
        { tid: 'task-004', name: 'eBPF追踪', status: 0, targetIP: '127.0.0.1', createTime: '2026-06-14 11:00' },
        { tid: 'task-005', name: 'Java堆分析', status: 2, targetIP: '127.0.0.1', createTime: '2026-06-13 15:00' },
    ];

    return (
        <div style={styles.container}>
            <h2>全部任务</h2>
            <div style={styles.card}>
                <input style={{ ...styles.input, marginBottom: 16 }} placeholder="搜索任务..." />
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
                        {tasks.map(t => (
                            <tr key={t.tid}>
                                <td style={styles.td}>{t.tid}</td>
                                <td style={styles.td}>{t.name}</td>
                                <td style={styles.td}>{t.targetIP}</td>
                                <td style={styles.td}>
                                    <span style={{ ...styles.badge, background: statusColors[t.status], color: '#fff' }}>
                                        {statusNames[t.status]}
                                    </span>
                                </td>
                                <td style={styles.td}>{t.createTime}</td>
                                <td style={styles.td}>
                                    <Link to={`/task/result?tid=${t.tid}`} style={{ color: '#4a6cf7', marginRight: 12 }}>查看</Link>
                                    <span style={{ color: '#f44336', cursor: 'pointer' }}>删除</span>
                                </td>
                            </tr>
                        ))}
                    </tbody>
                </table>
            </div>
        </div>
    );
}

// ============================================================
// App — 最外层组件（整个应用的"框架"）
// export default：让 index.js 可以 import 这个组件
// ============================================================
export default function App() {
    return (
        <div style={{ minHeight: '100vh', background: '#f0f2f5' }}>
            {/* 顶部导航栏 */}
            <header style={styles.header}>
                <div>
                    <h1 style={{ margin: 0, fontSize: 20 }}>🔥 Mini-Drop</h1>
                    <span style={{ fontSize: 12, color: '#888' }}>性能分析平台</span>
                </div>
                <nav style={styles.nav}>
                    <Link to="/" style={styles.navLink}>主页</Link>
                    <Link to="/tasks" style={styles.navLink}>任务列表</Link>
                </nav>
            </header>

            {/* URL 路由：不同路径显示不同页面 */}
            <Routes>
                <Route path="/" element={<HomePage />} />
                <Route path="/tasks" element={<TaskListPage />} />
                <Route path="/task/result" element={<TaskResultPage />} />
            </Routes>
        </div>
    );
}
