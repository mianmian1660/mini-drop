// ============================================================
// api/index.js — 后端 API 调用封装
// ============================================================
// 把每个后端接口封装成一个函数，页面组件直接调用
// 不用在每个组件里写 axios.get/post，统一管理
//
// 后端响应格式：{ code: 0, data: {...} }  成功
//              { code: xxx, message: "..." }  失败
// 由于 client.js 的响应拦截器已解包，这里的返回值就是上面的 JSON
// ============================================================

import client from './client';

// ---------- 鉴权 ----------
export const auth = {
    // 检查登录状态（GET /api/v1/auth/check）
    check: () => client.get('/api/v1/auth/check'),
};

// ---------- 用户 ----------
export const users = {
    // 获取当前用户信息（GET /api/v1/users）
    current: () => client.get('/api/v1/users'),
};

// ---------- Agent ----------
export const agents = {
    // Agent 列表（GET /api/v1/agents）
    list: () => client.get('/api/v1/agents'),
    // Agent 资源统计（GET /api/v1/agent/stat?ip=xxx）
    stat: (ip) => client.get('/api/v1/agent/stat', { params: { ip } }),
};

// ---------- 任务 ----------
export const tasks = {
    // 创建任务（POST /api/v1/tasks）
    create: (data) => client.post('/api/v1/tasks', data),
    // 任务列表（GET /api/v1/tasks?page=&pageSize=&keyword=&status=）
    list: (params = {}) => client.get('/api/v1/tasks', { params }),
    // 任务详情（GET /api/v1/tasks/:tid）
    detail: (tid) => client.get(`/api/v1/tasks/${tid}`),
    // 删除任务（DELETE /api/v1/tasks/:tid）
    delete: (tid) => client.delete(`/api/v1/tasks/${tid}`),
    // 重试任务（POST /api/v1/tasks/:tid/retry）
    retry: (tid) => client.post(`/api/v1/tasks/${tid}/retry`),
    // Continuous Profiling 时间轴（GET /api/v1/tasks/timeline?master_tid=xxx）
    timeline: (masterTid) => client.get('/api/v1/tasks/timeline', { params: { master_tid: masterTid } }),
};

// ---------- 定时任务 / Continuous Profiling ----------
export const schedules = {
    create: (data) => client.post('/api/v1/schedule/task', data),
    list: () => client.get('/api/v1/schedule/tasks'),
    delete: (sid) => client.delete(`/api/v1/schedule/${sid}`),
    toggle: (sid) => client.post(`/api/v1/schedule/${sid}/toggle`),
};

// ---------- 文件（W4: 火焰图等产物） ----------
export const cosfiles = {
    list: (tid) => client.get('/api/v1/cosfiles', { params: { tid } }),
};
