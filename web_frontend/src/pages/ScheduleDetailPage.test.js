// ============================================================
// pages/ScheduleDetailPage.test.js — 周期任务独立时间轴详情页测试
// ============================================================
// 覆盖：主机入口 /hosts/:targetId/schedules/:sid 与全局入口 /schedules/:sid
// 均加载对应 SID 的时间轴；顶部展示计划状态与关键参数；返回链接正确；
// 时间轴筛选、取消与内嵌基线 Diff 能力不回归。
// ============================================================

import React from 'react';
import { createRoot } from 'react-dom/client';
import { act, Simulate } from 'react-dom/test-utils';
import { MemoryRouter, Route, Routes } from 'react-router-dom';

jest.mock('../api', () => ({
    schedules: { detail: jest.fn(), toggle: jest.fn(), delete: jest.fn() },
    tasks: { timeline: jest.fn(), diff: jest.fn(), diffFlamegraph: jest.fn(), cancel: jest.fn() },
}));

jest.mock('../components/TimelineChart', () => {
    const MockTimelineChart = () => null;
    MockTimelineChart.statusColor = () => '#4caf50';
    return MockTimelineChart;
});

// InlineDiffPanel（经 ScheduleTimeline 渲染）里的 InteractiveFlamegraph 拉真实
// d3-selection/d3-flame-graph（ESM），jest 不跑真实 d3，见同款处理
// ContinuousProfilingPanel.test.js / InlineDiffPanel.test.js。
jest.mock('../components/InteractiveFlamegraph', () => ({
    __esModule: true,
    default: () => null,
}));

jest.mock('../utils/time', () => ({
    browserTimeZoneLabel: () => '本地时区',
    formatDateTime: (v) => String(v || ''),
    localDateTimeToISO: (v) => v,
}));

jest.mock('../utils/collectors', () => ({
    collectorLabelFromTask: () => 'perf CPU 火焰图',
    parseRequestParams: () => ({ duration: 10, frequency: 99 }),
}));

import ScheduleDetailPage from './ScheduleDetailPage';
import { schedules, tasks } from '../api';

global.IS_REACT_ACT_ENVIRONMENT = true;

const scheduleDetail = {
    sid: 'sch-1',
    name: '定时任务A',
    cron_expr: '*/5 * * * *',
    target_ip: '1.2.3.4',
    task_kind: 'perf_cpu',
    enabled: true,
    can_manage: true,
    user_name: 'user-a',
    created_at: '2026-08-22T00:00:00Z',
    last_run_at: '2026-08-22T00:05:00Z',
    next_run_at: '2026-08-22T00:10:00Z',
};

const points = [
    { tid: 't1', name: '窗口1', status: 2, has_result: true, window_start: '2026-08-22T00:00:00Z' },
    { tid: 't2', name: '窗口2', status: 2, has_result: true, window_start: '2026-08-22T00:05:00Z' },
];

beforeEach(() => {
    jest.clearAllMocks();
    window.confirm = jest.fn(() => true);
    window.alert = jest.fn();
});

async function renderAt(path) {
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(
            <MemoryRouter initialEntries={[path]}>
                <Routes>
                    <Route path="/hosts/:targetId/schedules/:sid" element={<ScheduleDetailPage />} />
                    <Route path="/schedules/:sid" element={<ScheduleDetailPage />} />
                </Routes>
            </MemoryRouter>
        );
    });
    await act(async () => { await Promise.resolve(); });
    return { container, root };
}

test('主机入口加载对应 SID 的时间轴并展示计划参数', async () => {
    schedules.detail.mockResolvedValue({ code: 0, data: scheduleDetail });
    tasks.timeline.mockResolvedValue({ code: 0, data: { points, trends: { total: 2 } } });
    tasks.diff.mockResolvedValue({ code: 0, data: { baseline: {}, compare: {}, functions: [] } });

    const { container, root } = await renderAt('/hosts/host-1/schedules/sch-1');

    // 详情按 SID 加载
    expect(schedules.detail).toHaveBeenCalledWith('sch-1');
    // 顶部计划状态与关键参数
    expect(container.textContent).toContain('定时任务A');
    expect(container.textContent).toContain('sch-1');
    expect(container.textContent).toContain('启用');
    expect(container.textContent).toContain('每 5 分钟');
    expect(container.textContent).toContain('1.2.3.4');
    expect(container.textContent).toContain('perf CPU 火焰图');
    // 返回该主机链接
    const backLink = Array.from(container.querySelectorAll('a')).find(a => a.textContent.includes('返回该主机'));
    expect(backLink).toBeTruthy();
    expect(backLink.getAttribute('href')).toBe('/hosts/host-1?tab=timeline');

    // 时间轴按 SID 加载
    expect(tasks.timeline).toHaveBeenCalledWith('sch-1', expect.anything());
    expect(container.textContent).toContain('窗口1');
    expect(container.textContent).toContain('窗口2');

    // 时间轴窗口提供"设为基线"
    const baselineBtns = Array.from(container.querySelectorAll('button')).filter(b => b.textContent.includes('设为基线'));
    expect(baselineBtns.length).toBeGreaterThanOrEqual(2);

    // 点击设为基线 → ★基线，其他窗口出现"与基线对比"
    await act(async () => { Simulate.click(baselineBtns[0]); });
    await act(async () => { await Promise.resolve(); });
    expect(container.textContent).toContain('★ 基线');

    // 点击"与基线对比" → 内嵌展开 Diff 面板并请求 diff
    const diffBtn = Array.from(container.querySelectorAll('button')).find(b => b.textContent.includes('与基线对比'));
    expect(diffBtn).toBeTruthy();
    await act(async () => { Simulate.click(diffBtn); });
    await act(async () => { await Promise.resolve(); });
    expect(tasks.diff).toHaveBeenCalled();
    expect(container.textContent).toContain('时间窗 Diff');

    // 面板内提供"在独立页面查看"链接，带 baseline/compare 参数
    const pageLink = Array.from(container.querySelectorAll('a')).find(a => a.textContent.includes('在独立页面查看'));
    expect(pageLink).toBeTruthy();
    expect(pageLink.getAttribute('href')).toContain('baseline=');
    expect(pageLink.getAttribute('href')).toContain('compare=');

    act(() => root.unmount());
    container.remove();
});

test('运行中的窗口保留"停止"取消能力', async () => {
    const withRunning = [
        ...points,
        { tid: 't3', name: '窗口3', status: 1, has_result: false, window_start: '2026-08-22T00:10:00Z', can_manage: true },
    ];
    schedules.detail.mockResolvedValue({ code: 0, data: scheduleDetail });
    tasks.timeline.mockResolvedValue({ code: 0, data: { points: withRunning, trends: null } });
    tasks.cancel.mockResolvedValue({ code: 0 });

    const { container, root } = await renderAt('/schedules/sch-1');

    const cancelBtn = Array.from(container.querySelectorAll('button')).find(b => b.textContent === '停止');
    expect(cancelBtn).toBeTruthy();
    await act(async () => { Simulate.click(cancelBtn); });
    await act(async () => { await Promise.resolve(); });
    expect(tasks.cancel).toHaveBeenCalledWith('t3');

    act(() => root.unmount());
    container.remove();
});

test('已取消窗口显示明确状态而不是未知', async () => {
    schedules.detail.mockResolvedValue({ code: 0, data: scheduleDetail });
    tasks.timeline.mockResolvedValue({
        code: 0,
        data: { points: [{ tid: 't-canceled', name: '取消窗口', status: 5, has_result: false, window_start: '2026-08-22T00:10:00Z' }], trends: null },
    });

    const { container, root } = await renderAt('/schedules/sch-1');

    expect(container.querySelector('table tbody').textContent).toContain('已取消');
    expect(container.querySelector('table tbody').textContent).not.toContain('未知');

    act(() => root.unmount());
    container.remove();
});

test('时间轴筛选应用后携带状态参数重新查询', async () => {
    schedules.detail.mockResolvedValue({ code: 0, data: scheduleDetail });
    tasks.timeline.mockResolvedValue({ code: 0, data: { points, trends: null } });

    const { container, root } = await renderAt('/schedules/sch-1');

    const statusSelect = Array.from(container.querySelectorAll('select')).find(s => Array.from(s.options).some(o => o.textContent === '已完成'));
    await act(async () => {
        Simulate.change(statusSelect, { target: { value: '2' } });
    });
    const applyBtn = Array.from(container.querySelectorAll('button')).find(b => b.textContent === '应用筛选');
    await act(async () => { Simulate.click(applyBtn); });
    await act(async () => { await Promise.resolve(); });

    expect(tasks.timeline).toHaveBeenLastCalledWith('sch-1', expect.objectContaining({ status: '2' }));

    act(() => root.unmount());
    container.remove();
});

test('全局入口 /schedules/:sid 返回周期任务列表，停用计划调用 toggle', async () => {
    schedules.detail.mockResolvedValue({ code: 0, data: scheduleDetail });
    tasks.timeline.mockResolvedValue({ code: 0, data: { points: [], trends: null } });
    schedules.toggle.mockResolvedValue({ code: 0, data: { enabled: false } });

    const { container, root } = await renderAt('/schedules/sch-1');

    const backLink = Array.from(container.querySelectorAll('a')).find(a => a.textContent.includes('返回周期任务列表'));
    expect(backLink).toBeTruthy();
    expect(backLink.getAttribute('href')).toBe('/timeline');

    const toggleBtn = Array.from(container.querySelectorAll('button')).find(b => b.textContent.includes('停用计划'));
    expect(toggleBtn).toBeTruthy();
    await act(async () => { Simulate.click(toggleBtn); });
    await act(async () => { await Promise.resolve(); });
    expect(schedules.toggle).toHaveBeenCalledWith('sch-1');

    act(() => root.unmount());
    container.remove();
});

test('详情加载失败时展示错误并保留返回入口', async () => {
    schedules.detail.mockRejectedValue(new Error('网络异常'));

    const { container, root } = await renderAt('/schedules/sch-1');

    expect(container.textContent).toContain('网络异常');
    const backLink = Array.from(container.querySelectorAll('a')).find(a => a.textContent.includes('返回周期任务列表'));
    expect(backLink).toBeTruthy();

    act(() => root.unmount());
    container.remove();
});

test('窗口列表状态列只显示状态，不带 task_kind/有结果；超过一页时分页展示', async () => {
    const manyPoints = Array.from({ length: 25 }, (_, i) => ({
        tid: `t${i + 1}`,
        name: `窗口${i + 1}`,
        status: i % 2 === 0 ? 2 : 3,   // 已完成 / 失败
        has_result: i % 3 === 0,
        task_kind: 'perf_cpu',
        window_start: `2026-08-22T00:${String(i).padStart(2, '0')}:00Z`,
    }));
    schedules.detail.mockResolvedValue({ code: 0, data: scheduleDetail });
    tasks.timeline.mockResolvedValue({ code: 0, data: { points: manyPoints, trends: null } });

    const { container, root } = await renderAt('/schedules/sch-1');

    // 第一页只渲染 PAGE_SIZE=20 行
    const rows = Array.from(container.querySelectorAll('table tbody tr'));
    expect(rows.length).toBe(20);

    // 窗口列表状态列干净：不含 task_kind / 有结果（筛选下拉框里"有结果/无结果"属筛选 UI，不算）
    const tableBodyText = container.querySelector('table tbody').textContent;
    expect(tableBodyText).not.toContain('perf_cpu');
    expect(tableBodyText).not.toContain('有结果');
    // 但状态徽章文案保留
    expect(tableBodyText).toContain('已完成');
    expect(tableBodyText).toContain('失败');

    // 分页控件出现
    expect(container.textContent).toContain('共 2 页');
    const nextBtn = Array.from(container.querySelectorAll('button')).find(b => b.textContent.includes('下一页'));
    expect(nextBtn).toBeTruthy();

    // 翻到第 2 页 → 剩余 5 行（最新窗口在第 1 页，第 2 页是最早的 窗口5..窗口1）
    await act(async () => { Simulate.click(nextBtn); });
    await act(async () => { await Promise.resolve(); });
    const rows2 = Array.from(container.querySelectorAll('table tbody tr'));
    expect(rows2.length).toBe(5);
    expect(container.textContent).toContain('窗口1');

    act(() => root.unmount());
    container.remove();
});

// 间隔型计划详情：顶部周期显示"每 X 分钟"（来自 interval_seconds），
// 新增"采样间隔/窗口时长"项，悬停 title 提供原始值与含义；不展示内部 cron。
test('间隔型计划详情展示"每 X 分钟"、采样间隔与窗口时长，无内部 Cron 字段', async () => {
    const intervalDetail = {
        sid: 'sch-int',
        name: '间隔计划',
        interval_seconds: 300,
        cron_expr: '',
        target_ip: '1.2.3.4',
        task_kind: 'perf_cpu',
        enabled: true,
        can_manage: true,
        user_name: 'user-a',
        created_at: '2026-08-22T00:00:00Z',
        last_run_at: '2026-08-22T00:05:00Z',
        next_run_at: '2026-08-22T00:10:00Z',
        request_params: JSON.stringify({ duration: 290, frequency: 19 }),
    };
    schedules.detail.mockResolvedValue({ code: 0, data: intervalDetail });
    tasks.timeline.mockResolvedValue({ code: 0, data: { points: [], trends: null } });

    const { container, root } = await renderAt('/schedules/sch-int');

    // 周期显示"每 5 分钟"（来自 interval_seconds），不显示 cron
    expect(container.textContent).toContain('每 5 分钟');
    expect(container.textContent).not.toContain('*/5 * * * *');
    // 新增采样间隔 / 窗口时长项（parseRequestParams 被 mock 为 duration:10）
    expect(container.textContent).toContain('采样间隔');
    expect(container.textContent).toContain('窗口时长');
    expect(container.textContent).toContain('每窗 10s');

    // 周期悬停 title：说明自动触发 + 间隔秒数
    const periodSpan = Array.from(container.querySelectorAll('span')).find(s => s.title && s.title.includes('自动触发一次深度采样'));
    expect(periodSpan).toBeTruthy();
    expect(periodSpan.title).toContain('300');

    // 状态徽章 tooltip：可执行解释
    const statusSpan = Array.from(container.querySelectorAll('span')).find(s => s.title && s.title.includes('计划运行中'));
    expect(statusSpan).toBeTruthy();

    act(() => root.unmount());
    container.remove();
});
