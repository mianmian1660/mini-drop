// ============================================================
// pages/HostDetailPage.test.js — 主机页单次/周期分类测试
// ============================================================
// 覆盖：概览"最近单次任务"只请求 task_scope=single；概览与"周期任务"Tab
// 都渲染周期任务列表（ScheduleList），且带主机 target_ip 过滤与
// /hosts/:id/schedules 详情前缀。
// ============================================================

import React from 'react';
import { createRoot } from 'react-dom/client';
import { act } from 'react-dom/test-utils';

const mockSearchParams = new URLSearchParams();

jest.mock('react-router-dom', () => ({
    Link: ({ children, to }) => <a href={to}>{children}</a>,
    useParams: () => ({ targetId: 'target-1' }),
    useSearchParams: () => [mockSearchParams, jest.fn()],
}));

jest.mock('../api', () => ({
    agents: { detail: jest.fn() },
    continuous: { sessions: jest.fn(), stopSession: jest.fn() },
    profiles: { targets: jest.fn() },
    schedules: { list: jest.fn() },
    tasks: { list: jest.fn() },
}));

jest.mock('../components/CreateTaskModal', () => () => null);
jest.mock('../components/CreateContinuousSessionModal', () => () => null);
jest.mock('../components/ContinuousSessionList', () => () => null);
jest.mock('../components/Pagination', () => () => null);
jest.mock('../components/ScheduleList', () => ({ detailPrefix, targetIp }) => (
    <div data-testid="schedule-list" data-prefix={detailPrefix} data-target={targetIp || ''} />
));

import HostDetailPage from './HostDetailPage';
import { agents, continuous, profiles, schedules, tasks } from '../api';

global.IS_REACT_ACT_ENVIRONMENT = true;

const target = { id: 'target-1', ip: '1.2.3.4', hostname: 'node', service_name: 'hotmethod', environment: 'prod', drop_agent_status: 'online', profile_status: 'running' };
const singleTasks = [
    { tid: 't1', name: '普通任务', task_kind: 'perf_cpu', status: 2, create_time: '2026-08-22T00:00:00Z', user_name: 'user-a', target_ip: '1.2.3.4' },
];
const hostSchedules = [
    { sid: 'sch-1', name: 'CPU 计划', target_ip: '1.2.3.4', cron_expr: '*/5 * * * *', enabled: true, user_name: 'user-a' },
];

async function setupApiMocks() {
    profiles.targets.mockResolvedValue({ code: 0, data: { targets: [target] } });
    agents.detail.mockResolvedValue({
        code: 0,
        data: {
            agent: { online: true, capabilities: 'perf_cpu' },
            stat: { source: 'grpc', cpu_percent: 1, memory_kb: 100, host: null },
            audits: [],
        },
    });
    tasks.list.mockResolvedValue({ code: 0, data: { tasks: singleTasks, total: 1 } });
    schedules.list.mockResolvedValue({ code: 0, data: { schedules: hostSchedules, total: 1 } });
    continuous.sessions.mockResolvedValue({ code: 0, data: { sessions: [] } });
}

beforeEach(() => {
    jest.clearAllMocks();
    mockSearchParams.delete('tab');
});

async function renderHost() {
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<HostDetailPage />);
    });
    await act(async () => { await Promise.resolve(); });
    return { container, root };
}

test('概览最近单次任务请求 task_scope=single，周期区域渲染主机周期列表', async () => {
    await setupApiMocks();
    const { container, root } = await renderHost();

    // 概览"最近单次任务"只请求单次任务（排除周期窗口）
    expect(tasks.list).toHaveBeenCalledWith(expect.objectContaining({ task_scope: 'single' }));
    expect(container.textContent).toContain('最近单次任务');

    // 概览周期区域渲染 ScheduleList：目标为该主机、详情前缀为 /hosts/:id/schedules
    const list = container.querySelector('[data-testid="schedule-list"]');
    expect(list).toBeTruthy();
    expect(list.getAttribute('data-prefix')).toBe('/hosts/target-1/schedules');
    expect(list.getAttribute('data-target')).toBe('1.2.3.4');

    act(() => root.unmount());
    container.remove();
});

test('周期任务 Tab 只展示当前主机的周期任务列表', async () => {
    mockSearchParams.set('tab', 'timeline');
    await setupApiMocks();
    const { container, root } = await renderHost();

    const list = container.querySelector('[data-testid="schedule-list"]');
    expect(list).toBeTruthy();
    expect(list.getAttribute('data-prefix')).toBe('/hosts/target-1/schedules');
    expect(list.getAttribute('data-target')).toBe('1.2.3.4');

    act(() => root.unmount());
    container.remove();
});
