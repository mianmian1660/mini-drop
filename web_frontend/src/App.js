// ============================================================
// App.js — React 主组件
// ============================================================
// 职责：
//   1. 应用启动时检查登录态（调 /api/v1/auth/check）
//   2. 未登录 → 显示登录页
//   3. 已登录 → 显示顶部导航（含用户名）+ 页面路由
//
// 页面路由：
//   /                → HomePage（Agent 列表 + 任务列表）
//   /tasks           → TaskListPage（全部任务）
//   /task/result     → TaskResultPage（任务详情 + 火焰图）
//   /login           → LoginPage（登录页）
// ============================================================

import React, { useState, useEffect } from 'react';
import { Routes, Route, Link, useNavigate } from 'react-router-dom';
import { auth, users } from './api';
import HomePage from './pages/HomePage';
import TaskListPage from './pages/TaskListPage';
import TaskResultPage from './pages/TaskResultPage';
import LoginPage from './pages/LoginPage';

// ============================================================
// CSS 样式
// ============================================================
const styles = {
    header: { background: '#1a1a2e', color: '#fff', padding: '16px 24px', display: 'flex', justifyContent: 'space-between', alignItems: 'center' },
    nav: { display: 'flex', gap: 20, alignItems: 'center' },
    navLink: { color: '#a0a0c0', textDecoration: 'none', fontSize: 16 },
    userInfo: { color: '#fff', fontSize: 14, display: 'flex', alignItems: 'center', gap: 8 },
    logoutBtn: { background: 'transparent', color: '#f44336', border: '1px solid #f44336', padding: '4px 12px', borderRadius: 4, cursor: 'pointer', fontSize: 12 },
    loadingScreen: { display: 'flex', justifyContent: 'center', alignItems: 'center', minHeight: '100vh', background: '#f0f2f5', color: '#999', fontSize: 18 },
};

// ============================================================
// App — 最外层组件
// ============================================================
export default function App() {
    const [authChecking, setAuthChecking] = useState(true);  // 是否正在检查登录态
    const [isLoggedIn, setIsLoggedIn] = useState(false);    // 是否已登录
    const [userName, setUserName] = useState('');            // 当前用户名
    const [userUid, setUserUid] = useState('');             // 当前用户 UID
    const navigate = useNavigate();

    // 应用启动时检查登录态
    useEffect(() => {
        checkAuth();
    }, []);

    const checkAuth = async () => {
        try {
            // 先调 auth/check 检查 cookie 是否有效
            const authRes = await auth.check();
            if (authRes.code === 0 && authRes.data) {
                // 再获取用户详细信息
                try {
                    const userRes = await users.current();
                    if (userRes.code === 0 && userRes.data) {
                        setUserName(userRes.data.user_name || userRes.data.name || '未知用户');
                        setUserUid(userRes.data.uid || '');
                    }
                } catch (e) {
                    // 用户接口失败不阻断登录
                    setUserName(authRes.data.user_name || '用户');
                    setUserUid(authRes.data.uid || '');
                }
                setIsLoggedIn(true);
            } else {
                setIsLoggedIn(false);
            }
        } catch (err) {
            // 后端不可达或 401 → 未登录
            console.log('鉴权检查失败:', err.message);
            setIsLoggedIn(false);
        } finally {
            setAuthChecking(false); // 标记检查完成
        }
    };

    // 登录成功后的回调（由 LoginPage 在写入 cookie 后触发）
    const handleLoginSuccess = (name, uid) => {
        setUserName(name);
        setUserUid(uid);
        setIsLoggedIn(true);
    };

    // 退出登录
    const handleLogout = () => {
        // 清除 cookie
        document.cookie = 'drop_user_uid=;expires=Thu, 01 Jan 1970 00:00:00 GMT;path=/';
        document.cookie = 'drop_user_name=;expires=Thu, 01 Jan 1970 00:00:00 GMT;path=/';
        setIsLoggedIn(false);
        navigate('/login');
    };

    // 鉴权检查中 → 显示加载页
    if (authChecking) {
        return <div style={styles.loadingScreen}>⏳ 正在验证登录状态...</div>;
    }

    // 未登录 → 只渲染登录页
    if (!isLoggedIn) {
        return (
            <div style={{ minHeight: '100vh', background: '#f0f2f5' }}>
                <Routes>
                    <Route path="*" element={<LoginPage onLoginSuccess={handleLoginSuccess} />} />
                </Routes>
            </div>
        );
    }

    // 已登录 → 渲染完整布局
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
                    <span style={{ color: '#555' }}>|</span>
                    <span style={styles.userInfo}>
                        👤 {userName}
                    </span>
                    <button style={styles.logoutBtn} onClick={handleLogout}>退出</button>
                </nav>
            </header>

            {/* URL 路由 */}
            <Routes>
                <Route path="/" element={<HomePage />} />
                <Route path="/tasks" element={<TaskListPage />} />
                <Route path="/task/result" element={<TaskResultPage />} />
                <Route path="/login" element={<LoginPage onLoginSuccess={handleLoginSuccess} />} />
            </Routes>
        </div>
    );
}
