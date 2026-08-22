// ============================================================
// pages/TimelinePage.test.js — Timeline 集成测试
// ============================================================
// 覆盖（3a6230f 专项）：定时任务加载 / 时间轴窗口渲染 / 区间对比（设为基线）
// 入口 / 空结果与错误提示
// ============================================================

import React from 'react';
import { createRoot } from 'react-dom/client';
import { act, Simulate } from 'react-dom/test-utils';

jest.mock('react-router-dom', () => ({
    Link: ({ children, to }) => <a href={to}>{children}</a>,
}));

jest.mock('../api', () => ({
    tasks: { timeline: jest.fn() },
    schedules: { list: jest.fn(), toggle: jest.fn(), delete: jest.fn() },
}));

jest.mock('../components/TimelineChart', () => {
    const MockTimelineChart = () => null;
    MockTimelineChart.statusColor = () => '#4caf50';
    return MockTimelineChart;
});

jest.mock('../utils/time', () => ({
    browserTimeZoneLabel: () => '本地时区',
    formatDateTime: (v) => String(v || ''),
    localDateTimeToISO: (v) => v,
}));

import TimelinePage from './TimelinePage';
import { tasks, schedules } from '../api';

global.IS_REACT_ACT_ENVIRONMENT = true;

beforeEach(() => {
    jest.clearAllMocks();
    window.confirm = jest.fn(() => true);
    window.alert = jest.fn();
});

test('加载定时任务并渲染已完成窗口，提供设为基线入口', async () => {
    schedules.list.mockResolvedValue({ code: 0, data: {
        schedules: [{ sid: 'sch-1', name: '定时任务A', cron_expr: '*/30 * * * *', user_name: 'user-a', enabled: true, can_manage: true }],
    } });
    tasks.timeline.mockResolvedValue({ code: 0, data: {
        points: [
            { tid: 't1', name: '窗口1', status: 2, has_result: true, window_start: '2026-08-22T00:00:00Z' },
            { tid: 't2', name: '窗口2', status: 2, has_result: true, window_start: '2026-08-22T01:00:00Z' },
        ],
        trends: { total: 2, success: 2 },
    } });

    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<TimelinePage />);
    });
    await act(async () => { await Promise.resolve(); });

    // 定时任务列表渲染
    expect(container.textContent).toContain('定时任务A');
    expect(container.textContent).toContain('*/30 * * * *');

    // 点击 schedule 行（cursor:pointer 的外层行）-> 加载 timeline
    const schRow = Array.from(container.querySelectorAll('div')).find(
        d => d.textContent.includes('SID: sch-1') && (d.getAttribute('style') || '').includes('cursor: pointer')
    );
    expect(schRow).toBeTruthy();
    await act(async () => {
        Simulate.click(schRow);
    });
    await act(async () => { await Promise.resolve(); });

    expect(tasks.timeline).toHaveBeenCalledWith('sch-1', expect.objectContaining({}));
    expect(container.textContent).toContain('窗口1');
    expect(container.textContent).toContain('窗口2');

    // 有结果的窗口提供"设为基线"（区间对比入口）
    const baselineBtns = Array.from(container.querySelectorAll('button')).filter(b => b.textContent.includes('设为基线'));
    expect(baselineBtns.length).toBeGreaterThanOrEqual(2);

    // 点击第一个窗口的"设为基线" -> 该窗口显示"★ 基线"，其他窗口出现"与基线对比"链接
    await act(async () => {
        Simulate.click(baselineBtns[0]);
    });
    await act(async () => { await Promise.resolve(); });
    expect(container.textContent).toContain('★ 基线');
    const diffLink = Array.from(container.querySelectorAll('a')).find(a => a.textContent.includes('与基线对比'));
    expect(diffLink).toBeTruthy();
    expect(diffLink.getAttribute('href')).toContain('baseline=t1');
    expect(diffLink.getAttribute('href')).toContain('compare=t2');

    act(() => root.unmount());
    container.remove();
});

test('时间轴接口失败时显示错误信息', async () => {
    schedules.list.mockResolvedValue({ code: 0, data: { schedules: [{ sid: 'sch-1', name: '定时任务A', enabled: true }] } });
    tasks.timeline.mockRejectedValue(new Error('网络异常'));

    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<TimelinePage />);
    });
    await act(async () => { await Promise.resolve(); });

    const schRow = Array.from(container.querySelectorAll('div')).find(
        d => d.textContent.includes('SID: sch-1') && (d.getAttribute('style') || '').includes('cursor: pointer')
    );
    await act(async () => {
        Simulate.click(schRow);
    });
    await act(async () => { await Promise.resolve(); });

    expect(container.textContent).toContain('请求失败');
    act(() => root.unmount());
    container.remove();
});

test('无子任务窗口时显示空态提示', async () => {
    schedules.list.mockResolvedValue({ code: 0, data: { schedules: [{ sid: 'sch-1', name: '定时任务A', enabled: true }] } });
    tasks.timeline.mockResolvedValue({ code: 0, data: { points: [], trends: null } });

    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<TimelinePage />);
    });
    await act(async () => { await Promise.resolve(); });

    const schRow = Array.from(container.querySelectorAll('div')).find(
        d => d.textContent.includes('SID: sch-1') && (d.getAttribute('style') || '').includes('cursor: pointer')
    );
    await act(async () => {
        Simulate.click(schRow);
    });
    await act(async () => { await Promise.resolve(); });

    expect(container.textContent).toContain('该定时任务暂无子任务记录');
    act(() => root.unmount());
    container.remove();
});
