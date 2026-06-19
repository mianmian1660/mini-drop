// ============================================================
// api/client.js — axios 实例 + 拦截器
// ============================================================
// 职责：
//   1. 创建统一的 axios 实例（baseURL、超时、withCredentials）
//   2. 请求拦截器：自动从 Cookie 读取用户信息并附加到 Header
//   3. 响应拦截器：统一处理 401 跳转登录、解包 data 字段
//
// axios 语法小课堂：
//   axios.create({...})      = 创建独立实例，互不影响
//   interceptors.request     = 请求发出前执行（类似中间件）
//   interceptors.response    = 响应回来后先执行（类似中间件）
// ============================================================

import axios from 'axios';

// 读取运行时配置（由 public/config.js 注入到 window.config）
const HOST_URL = window.config?.HOST_URL || '';

const client = axios.create({
    baseURL: HOST_URL,
    withCredentials: true,           // 跨域请求也带 Cookie
    timeout: 30000,                  // 30 秒超时
});

// ---------- 工具函数：读取 Cookie ----------
function getCookie(name) {
    const match = document.cookie.match(new RegExp('(^| )' + name + '=([^;]+)'));
    return match ? decodeURIComponent(match[2]) : '';
}

// ---------- 请求拦截器 ----------
// 每次发请求前，自动从 Cookie 读取用户 uid/name 放入 Header
// 这样后端 apiserver 就不需要自己做 Cookie 解析了
client.interceptors.request.use(
    (config) => {
        const uid = getCookie('drop_user_uid');
        const name = getCookie('drop_user_name');
        if (uid) config.headers['Drop_user_uid'] = uid;
        if (name) config.headers['Drop_user_name'] = name;
        return config;
    },
    (error) => Promise.reject(error)
);

// ---------- 响应拦截器 ----------
// 统一处理：
//   - 解包：axios 返回 response，我们只取 response.data（即后端 JSON）
//   - 401：自动跳转登录页
client.interceptors.response.use(
    (response) => {
        // 直接返回 data 部分（{ code: 0, data: {...} }）
        return response.data;
    },
    (error) => {
        // 401 鉴权失败 → 跳转登录
        if (error.response?.status === 401) {
            const redirect = encodeURIComponent(window.location.href);
            window.location.href = '/login?redirect=' + redirect;
        }

        // W5: 网络错误 → 附加友好消息
        if (!error.response) {
            // 无响应 = 网络不通或后端未启动
            error.userMessage = '无法连接到后端服务，请确认 apiserver 已启动';
        } else if (error.response.status >= 500) {
            error.userMessage = '服务器内部错误，请稍后重试';
        } else if (error.response.status === 404) {
            error.userMessage = '请求的资源不存在';
        } else if (error.response.status === 403) {
            error.userMessage = '没有权限执行此操作';
        }

        return Promise.reject(error);
    }
);

export default client;
