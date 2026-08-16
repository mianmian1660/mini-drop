// ============================================================
// api/index.js — 后端 API 调用封装
// ============================================================
// 把每个后端接口封装成一个函数，页面组件直接调用
// 不用在每个组件里写 axios.get/post，统一管理
//
// 后端响应格式：{ request_id, data, error, code }。
// 为兼容旧页面，成功仍保留 code: 0，失败仍保留 code/message。
// 由于 client.js 的响应拦截器已解包，这里的返回值就是上面的 JSON
// ============================================================

import client from './client';

const HOST_URL = window.config?.HOST_URL || '';

export function apiURL(path) {
    if (/^https?:\/\//i.test(path)) return path;
    const base = HOST_URL.replace(/\/$/, '');
    return `${base}${path.startsWith('/') ? path : `/${path}`}`;
}

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
    // Agent 审计日志（GET /api/v1/agents/audits）
    audits: (params = {}) => client.get('/api/v1/agents/audits', { params }),
    // Agent 详情（GET /api/v1/agent/detail?ip=xxx）
    detail: (ip) => client.get('/api/v1/agent/detail', { params: { ip } }),
    // Agent 资源统计（GET /api/v1/agent/stat?ip=xxx）
    stat: (ip) => client.get('/api/v1/agent/stat', { params: { ip } }),
};

export const taskKinds = {
    list: (params = {}) => client.get('/api/v1/task-kinds', { params }),
};

// ---------- 任务 ----------
export const tasks = {
    // 创建任务（POST /api/v1/tasks）
    create: (data) => client.post('/api/v1/tasks', data),
    // 创建复合任务（POST /api/v1/tasks/composite）
    createComposite: (data) => client.post('/api/v1/tasks/composite', data),
    // 任务列表（GET /api/v1/tasks?page=&pageSize=&keyword=&status=）
    list: (params = {}) => client.get('/api/v1/tasks', { params }),
    // 任务详情（GET /api/v1/tasks/:tid）
    detail: (tid) => client.get(`/api/v1/tasks/${tid}`),
    // 复合任务子任务列表（GET /api/v1/tasks/:tid/children）
    children: (tid) => client.get(`/api/v1/tasks/${tid}/children`),
    // 删除任务（DELETE /api/v1/tasks/:tid）
    delete: (tid) => client.delete(`/api/v1/tasks/${tid}`),
    // 重试任务（POST /api/v1/tasks/:tid/retry）
    retry: (tid) => client.post(`/api/v1/tasks/${tid}/retry`),
    // 取消任务（POST /api/v1/tasks/:tid/cancel）
    cancel: (tid) => client.post(`/api/v1/tasks/${tid}/cancel`),
    // 任务级 Artifact 元数据
    artifacts: (tid) => client.get(`/api/v1/tasks/${tid}/artifacts`),
    // 任务级 Artifact 短期下载链接
    artifactDownload: (tid, artifactId) => client.get(`/api/v1/tasks/${tid}/artifacts/${artifactId}/download`),
    eventsStreamURL: (tid) => apiURL(`/api/v1/tasks/${encodeURIComponent(tid)}/events/stream`),
    suggestionsStreamURL: (tid) => apiURL(`/api/v1/tasks/${encodeURIComponent(tid)}/suggestions/stream`),
    // Periodic Deep Sampling 时间轴
    // 全部窗口:      tasks.timeline(masterTid)
    // 区间筛选:      tasks.timeline(masterTid, { from, to })    （RFC3339 字符串）
    // 按时刻回溯:    tasks.timeline(masterTid, { at, span })    at 为 RFC3339，span 为时长如 '30m'/'2h'
    //               返回 [at-span, at+span] 内的全部窗口，当时生效的那个 is_effective=true
    // GET /api/v1/tasks/timeline?master_tid=xxx&from=&to=&at=&span=
    timeline: (masterTid, { from, to, at, span, status, task_kind, has_result } = {}) =>
        client.get('/api/v1/tasks/timeline', { params: { master_tid: masterTid, from, to, at, span, status, task_kind, has_result } }),
    // 热点函数对比（基线 vs 对比），数据取两侧的 top.json
    // threshold 单位是百分点，|差值| 小于它的函数视为噪声滤掉，缺省 1
    // GET /api/v1/tasks/diff?baseline_tid=&compare_tid=&threshold=
    diff: (baselineTid, compareTid, threshold) =>
        client.get('/api/v1/tasks/diff', {
            params: { baseline_tid: baselineTid, compare_tid: compareTid, threshold },
        }),
};

// ---------- Native Continuous Profiling ----------
export const profiles = {
    targets: () => client.get('/api/v1/profile/targets'),
    flamegraph: (params = {}) => client.get('/api/v1/profile/flamegraph', { params }),
    topn: (params = {}) => client.get('/api/v1/profile/topn', { params }),
    diff: (params = {}) => client.get('/api/v1/profile/diff', { params }),
    labelValues: (params = {}) => client.get('/api/v1/profile/label-values', { params }),
};

export const continuous = {
    createSession: (data) => client.post('/api/v1/continuous/sessions', data),
    sessions: (params = {}) => client.get('/api/v1/continuous/sessions', { params }),
    stopSession: (sid) => client.post(`/api/v1/continuous/sessions/${sid}/stop`),
    timeline: (sid, params = {}) => client.get(`/api/v1/continuous/sessions/${sid}/timeline`, { params }),
    query: (params = {}) => client.get('/api/v1/continuous/query', { params }),
    histogram: (params = {}) => client.get('/api/v1/continuous/histogram', { params }),
};

// ---------- 分析结果 ----------
// 当前后端将建议、归因和产物汇总在任务详情中；在这里收敛为结果页可复用的数据接口。
export const analysisResults = {
    suggestions: async (tid) => {
        const response = await tasks.detail(tid);
        return { code: response.code, message: response.message, data: response.data?.suggestions || [] };
    },
    attribution: async (tid) => {
        const response = await tasks.detail(tid);
        return {
            code: response.code,
            message: response.message,
            data: {
                suggestions: response.data?.suggestions || [],
                files: response.data?.files || [],
            },
        };
    },
};

// ---------- 周期性深度采样 / Periodic Deep Sampling ----------
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
