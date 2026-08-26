// ============================================================
// pages/TaskListPage.test.js — 单次任务列表测试
// ============================================================
// 覆盖：/tasks 单次任务列表只请求 task_scope=single（排除周期计划直接生成的
// 采集窗口），渲染普通/复合/人工重试任务。
// ============================================================

import React from 'react';
import { createRoot } from 'react-dom/client';
import { act, Simulate } from 'react-dom/test-utils';

jest.mock('react-router-dom', () => ({
    Link: ({ children, to }) => <a href={to}>{children}</a>,
}));

jest.mock('../api', () => ({
    tasks: { list: jest.fn(), delete: jest.fn() },
}));

jest.mock('../components/Pagination', () => () => null);
jest.mock('../components/TaskCancelButton', () => () => <button>取消</button>);

jest.mock('../utils/collectors', () => ({
    collectorLabelFromTask: () => 'perf CPU 火焰图',
}));

import TaskListPage from './TaskListPage';
import { tasks } from '../api';

global.IS_REACT_ACT_ENVIRONMENT = true;

const singleTasks = [
    { tid: 't-normal', name: '普通任务', target_ip: '1.2.3.4', task_kind: 'perf_cpu', status: 2, create_time: '2026-08-22T00:00:00Z', user_name: 'user-a', can_manage: true },
    { tid: 't-retry', name: '人工重试', target_ip: '1.2.3.4', task_kind: 'perf_cpu', status: 0, create_time: '2026-08-22T00:05:00Z', user_name: 'user-a', can_manage: true },
];

beforeEach(() => {
    jest.clearAllMocks();
    window.confirm = jest.fn(() => true);
});

async function renderPage() {
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<TaskListPage />);
    });
    await act(async () => { await Promise.resolve(); });
    return { container, root };
}

test('单次任务列表请求 task_scope=single 且渲染普通/重试任务', async () => {
    tasks.list.mockResolvedValue({ code: 0, data: { tasks: singleTasks, total: 2 } });

    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<TaskListPage />);
    });
    await act(async () => { await Promise.resolve(); });

    // 请求携带 task_scope=single（周期窗口不会出现在单次列表，由后端过滤）
    expect(tasks.list).toHaveBeenCalledWith(expect.objectContaining({ task_scope: 'single', owner_filter: 'mine' }));

    // 页面标题与任务渲染
    expect(container.textContent).toContain('单次任务');
    expect(container.textContent).toContain('普通任务');
    expect(container.textContent).toContain('人工重试');
    // 已完成的普通任务显示"已完成"状态
    expect(container.textContent).toContain('已完成');

    act(() => root.unmount());
    container.remove();
});

test('取消任务显示已取消并可按取消状态筛选', async () => {
    tasks.list.mockResolvedValue({
        code: 0,
        data: { tasks: [{ ...singleTasks[0], tid: 't-canceled', name: '已取消任务', status: 5 }], total: 1 },
    });

    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<TaskListPage />);
    });
    await act(async () => { await Promise.resolve(); });

    expect(container.textContent).toContain('已取消任务');
    expect(container.textContent).toContain('已取消');
    const statusSelect = Array.from(container.querySelectorAll('select')).find(select =>
        Array.from(select.options).some(option => option.value === '5' && option.textContent === '已取消'));
    expect(statusSelect).toBeTruthy();
    await act(async () => Simulate.change(statusSelect, { target: { value: '5' } }));
    await act(async () => { await Promise.resolve(); });
    expect(tasks.list).toHaveBeenLastCalledWith(expect.objectContaining({ status: '5' }));

    act(() => root.unmount());
    container.remove();
});

test('采集完成但分析失败显示分析失败', async () => {
    tasks.list.mockResolvedValue({
        code: 0,
        data: { tasks: [{ ...singleTasks[0], tid: 't-analysis-failed', name: '分析失败任务', status: 2, analysis_status: 3 }], total: 1 },
    });

    const { container, root } = await renderPage();
    expect(container.textContent).toContain('分析失败任务');
    expect(container.textContent).toContain('分析失败');

    act(() => root.unmount());
    container.remove();
});

test('无任务时展示空态提示', async () => {
    tasks.list.mockResolvedValue({ code: 0, data: { tasks: [], total: 0 } });

    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<TaskListPage />);
    });
    await act(async () => { await Promise.resolve(); });

    expect(container.textContent).toContain('暂无任务');
    expect(tasks.list).toHaveBeenCalledWith(expect.objectContaining({ task_scope: 'single' }));

    act(() => root.unmount());
    container.remove();
});
