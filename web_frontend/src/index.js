// ============================================================
// index.js — React 前端入口 小白版注释
// ============================================================
// 这是整个前端应用的"起点"
// React 从这里开始渲染，把 <App /> 组件挂到 HTML 页面上
//
// React 语法小课堂：
//   import X from 'y'        = 引入模块（类似 Python 的 import）
//   <Component />            = JSX 语法，看起来像 HTML，实际是 JavaScript
//   React.StrictMode         = 开发模式，会检查潜在问题
//   BrowserRouter            = 让 React 支持 URL 路由（多个页面）
// ============================================================

import React from 'react';
import ReactDOM from 'react-dom/client';
import { BrowserRouter } from 'react-router-dom';
import App from './App';
import ErrorBoundary from './components/ErrorBoundary';

const root = ReactDOM.createRoot(document.getElementById('root'));

root.render(
    <React.StrictMode>
        {/* ErrorBoundary 捕获子组件渲染错误，防止白屏 */}
        <ErrorBoundary>
            <BrowserRouter>
                <App />
            </BrowserRouter>
        </ErrorBoundary>
    </React.StrictMode>
);
