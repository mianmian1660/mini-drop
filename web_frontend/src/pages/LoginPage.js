// ============================================================
// pages/LoginPage.js — 登录页
// ============================================================
// 流程：
//   1. 用户输入用户名 → 点击登录
//   2. 前端写 Cookie (drop_user_uid / drop_user_name)
//   3. 调 /api/v1/auth/check 验证
//   4. 成功后跳回之前的页面（或主页）
//
// React 语法小课堂：
//   useNavigate()  = React Router v6 的页面跳转 hook
//   useSearchParams() = 读取 URL 查询参数（?redirect=xxx）
// ============================================================

import React, { useState, useEffect } from 'react';
import { useNavigate, useSearchParams } from 'react-router-dom';
import { auth } from '../api';

const styles = {
    container: {
        display: 'flex', justifyContent: 'center', alignItems: 'center',
        minHeight: '100vh', background: '#f0f2f5',
    },
    card: {
        background: '#fff', borderRadius: 12, padding: '40px 48px',
        boxShadow: '0 4px 24px rgba(0,0,0,0.1)', width: 400, textAlign: 'center',
    },
    title: { fontSize: 28, fontWeight: 'bold', color: '#1a1a2e', marginBottom: 8 },
    subtitle: { fontSize: 14, color: '#999', marginBottom: 32 },
    input: {
        width: '100%', padding: '10px 16px', border: '1px solid #ddd',
        borderRadius: 6, fontSize: 15, marginBottom: 20, boxSizing: 'border-box',
    },
    btn: {
        width: '100%', padding: '12px', background: '#4a6cf7', color: '#fff',
        border: 'none', borderRadius: 6, fontSize: 16, cursor: 'pointer',
    },
    error: { color: '#f44336', fontSize: 13, marginTop: 12 },
    hint: { fontSize: 12, color: '#ccc', marginTop: 24 },
};

export default function LoginPage({ onLoginSuccess }) {
    const navigate = useNavigate();
    const [searchParams] = useSearchParams();
    const [name, setName] = useState('');
    const [loading, setLoading] = useState(false);
    const [error, setError] = useState('');

    // 如果已经有 cookie，直接验证并跳转
    useEffect(() => {
        const uid = getCookie('drop_user_uid');
        if (uid) {
            // 已有登录态，验证后跳转
            auth.check()
                .then(() => {
                    const redirect = searchParams.get('redirect') || '/';
                    navigate(redirect, { replace: true });
                })
                .catch(() => {
                    // cookie 无效，让用户重新登录
                });
        }
    }, [navigate, searchParams]);

    const handleLogin = async () => {
        const trimmed = name.trim();
        if (!trimmed) {
            setError('请输入用户名');
            return;
        }

        setLoading(true);
        setError('');

        // 模拟登录：写入 Cookie（生产环境由 OAuth 回调写入）
        const uid = 'user-' + trimmed.toLowerCase().replace(/\s+/g, '-');
        setCookie('drop_user_uid', uid, 7);
        setCookie('drop_user_name', trimmed, 7);

        try {
            // 验证登录态
            await auth.check();
            // 通知父组件登录成功
            if (onLoginSuccess) {
                onLoginSuccess(trimmed, uid);
            }
            const redirect = searchParams.get('redirect') || '/';
            navigate(redirect, { replace: true });
        } catch (err) {
            setError('登录验证失败: ' + (err.message || '未知错误'));
            setLoading(false);
        }
    };

    // 回车也能提交
    const handleKeyDown = (e) => {
        if (e.key === 'Enter') handleLogin();
    };

    return (
        <div style={styles.container}>
            <div style={styles.card}>
                <h1 style={styles.title}>🔥 Mini-Drop</h1>
                <p style={styles.subtitle}>性能分析平台 · 登录</p>
                <input
                    style={styles.input}
                    placeholder="请输入用户名"
                    value={name}
                    onChange={e => setName(e.target.value)}
                    onKeyDown={handleKeyDown}
                    autoFocus
                />
                <button
                    style={{ ...styles.btn, opacity: loading ? 0.7 : 1 }}
                    onClick={handleLogin}
                    disabled={loading}
                >
                    {loading ? '验证中...' : '登 录'}
                </button>
                {error && <p style={styles.error}>{error}</p>}
                <p style={styles.hint}>
                    MVP 阶段：输入任意用户名即可登录
                </p>
            </div>
        </div>
    );
}

// ---------- Cookie 工具函数 ----------
function setCookie(name, value, days) {
    const d = new Date();
    d.setTime(d.getTime() + days * 24 * 60 * 60 * 1000);
    document.cookie = name + '=' + encodeURIComponent(value) +
        ';expires=' + d.toUTCString() + ';path=/';
}

function getCookie(name) {
    const match = document.cookie.match(new RegExp('(^| )' + name + '=([^;]+)'));
    return match ? decodeURIComponent(match[2]) : '';
}
