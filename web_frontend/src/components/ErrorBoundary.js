// ============================================================
// components/ErrorBoundary.js — React 错误边界（W5）
// ============================================================
// 职责：捕获子组件树中的渲染期错误，防止整个页面白屏
// 用法：<ErrorBoundary><App /></ErrorBoundary>
//
// React 语法小课堂：
//   componentDidCatch(error, errorInfo) = 类组件生命周期，捕获子组件错误
//   函数组件无法直接实现 ErrorBoundary，必须用类组件
// ============================================================

import React from 'react';

const styles = {
    container: {
        display: 'flex', justifyContent: 'center', alignItems: 'center',
        minHeight: '100vh', background: '#f0f2f5', fontFamily: 'Arial, sans-serif',
    },
    card: {
        background: '#fff', borderRadius: 12, padding: '40px 48px',
        boxShadow: '0 4px 24px rgba(0,0,0,0.1)', maxWidth: 500, textAlign: 'center',
    },
    title: { fontSize: 24, color: '#f44336', marginBottom: 12 },
    message: { fontSize: 14, color: '#666', marginBottom: 24, lineHeight: 1.6 },
    detail: {
        background: '#f5f5f5', borderRadius: 6, padding: 16, marginBottom: 20,
        fontSize: 12, color: '#999', textAlign: 'left', maxHeight: 200, overflow: 'auto',
        whiteSpace: 'pre-wrap', wordBreak: 'break-all',
    },
    btn: {
        background: '#4a6cf7', color: '#fff', border: 'none',
        padding: '10px 24px', borderRadius: 6, cursor: 'pointer', fontSize: 14,
    },
};

export default class ErrorBoundary extends React.Component {
    constructor(props) {
        super(props);
        this.state = { hasError: false, error: null, errorInfo: null };
    }

    static getDerivedStateFromError(error) {
        // 更新 state 使下次渲染显示降级 UI
        return { hasError: true, error };
    }

    componentDidCatch(error, errorInfo) {
        // 记录错误信息（生产环境可上报到监控系统）
        console.error('[ErrorBoundary] 捕获到渲染错误:', error, errorInfo);
        this.setState({ errorInfo });
    }

    handleRetry = () => {
        this.setState({ hasError: false, error: null, errorInfo: null });
    };

    render() {
        if (this.state.hasError) {
            return (
                <div style={styles.container}>
                    <div style={styles.card}>
                        <h1 style={styles.title}>⚠️ 页面出错了</h1>
                        <p style={styles.message}>
                            应用遇到了意外的渲染错误，请尝试刷新页面。<br />
                            如果问题持续存在，请联系管理员。
                        </p>
                        {this.state.error && (
                            <details style={{ marginBottom: 20 }}>
                                <summary style={{ fontSize: 12, color: '#999', cursor: 'pointer' }}>
                                    查看错误详情
                                </summary>
                                <div style={styles.detail}>
                                    {this.state.error.toString()}
                                    {this.state.errorInfo?.componentStack}
                                </div>
                            </details>
                        )}
                        <button style={styles.btn} onClick={this.handleRetry}>
                            重试
                        </button>
                        <button
                            style={{ ...styles.btn, background: '#999', marginLeft: 12 }}
                            onClick={() => window.location.reload()}
                        >
                            刷新页面
                        </button>
                    </div>
                </div>
            );
        }

        return this.props.children;
    }
}
