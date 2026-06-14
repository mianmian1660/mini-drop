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

import React from 'react';                     // React 核心库
import ReactDOM from 'react-dom/client';       // React 的 DOM 渲染器
import { BrowserRouter } from 'react-router-dom'; // 路由库（让 URL 和页面关联）
import App from './App';                       // 引入主组件

// 找到 HTML 中的 <div id="root"></div>
const root = ReactDOM.createRoot(document.getElementById('root'));

// 渲染整个应用
root.render(
    <React.StrictMode>
        {/* BrowserRouter 包裹整个应用，提供路由功能 */}
        <BrowserRouter>
            <App />  {/* 主组件：包含顶部导航 + 页面路由 */}
        </BrowserRouter>
    </React.StrictMode>
);
