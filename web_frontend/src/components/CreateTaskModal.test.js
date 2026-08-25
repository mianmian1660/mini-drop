// ============================================================
// components/CreateTaskModal.test.js — 新建单次/周期采样弹窗测试
// 覆盖：
//   1. 周期模式不再渲染 Cron 输入，展示采样间隔预设 + 开始时间
//   2. 间隔预设与自定义间隔正确提交 interval_seconds
//   3. 采样时长超过间隔时禁止提交（重叠校验）
//   4. 指定开始时间时提交 start_at（ISO UTC）
// ============================================================

import React from 'react';
import { createRoot } from 'react-dom/client';
import { act, Simulate } from 'react-dom/test-utils';

jest.mock('../api', () => ({
    tasks: { create: jest.fn() },
    agents: { list: jest.fn() },
    schedules: { create: jest.fn() },
    taskKinds: { list: jest.fn() },
}));

jest.mock('../utils/collectors', () => ({
    capabilityLabel: (cap) => cap,
    parseStringList: () => [],
}));

jest.mock('react-router-dom', () => ({
    Link: ({ children, to }) => <a href={to}>{children}</a>,
}));

import CreateTaskModal from './CreateTaskModal';
import { agents, schedules, taskKinds } from '../api';

global.IS_REACT_ACT_ENVIRONMENT = true;

const perfCpuKind = {
    id: 'perf_cpu',
    display_name: 'perf CPU 火焰图',
    runner: 'perf',
    max_duration: 3600,
    schema: [
        { name: 'duration', label: '采样时长(秒)', type: 'number', default: 10, min: 1, required: true },
        { name: 'frequency', label: '采样频率(Hz)', type: 'number', default: 99, min: 1, required: true },
        { name: 'callgraph', label: '调用栈模式', type: 'select', options: ['fp', 'dwarf'], default: 'fp' },
        { name: 'event', label: '采样事件', type: 'text', default: 'cpu-clock' },
    ],
    default: {},
};

async function renderModal() {
    agents.list.mockResolvedValue({ code: 0, data: { agents: [{ ip_addr: '1.2.3.4', hostname: 'node', online: true, capabilities: null }] } });
    taskKinds.list.mockResolvedValue({ code: 0, data: { task_kinds: [perfCpuKind] } });
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<CreateTaskModal onClose={() => {}} onSuccess={() => {}} initialTargetIP="1.2.3.4" />);
    });
    // 等待 agents / taskKinds 异步加载完成
    await act(async () => { await Promise.resolve(); });
    await act(async () => { await Promise.resolve(); });
    return { container, root };
}

function enablePeriodic(container) {
    const checkboxes = Array.from(container.querySelectorAll('input[type="checkbox"]'));
    const periodic = checkboxes.find(cb => cb.closest('label')?.textContent.includes('周期性深度采样'));
    act(() => Simulate.change(periodic, { target: { checked: true } }));
    return periodic;
}

test('周期模式不渲染 Cron 输入，展示间隔预设与下一次采集时间', async () => {
    const { container, root } = await renderModal();
    enablePeriodic(container);

    // 不再有 Cron 表达式输入框
    expect(container.querySelector('input[placeholder="*/5 * * * *"]')).toBeNull();
    expect(container.textContent).not.toContain('Cron 周期');
    // 展示间隔预设（分钟）与开始时间
    expect(container.textContent).toContain('每 1 分钟');
    expect(container.textContent).toContain('每 5 分钟');
    expect(container.textContent).toContain('每 30 分钟');
    expect(container.textContent).toContain('立即开始');
    expect(container.textContent).toContain('指定时间');
    // 默认每 5 分钟 → 下一次采集预估
    expect(container.textContent).toMatch(/下一次采集/);
    expect(container.textContent).toContain('每 5 分钟');

    act(() => root.unmount());
    container.remove();
});

test('间隔预设提交 interval_seconds', async () => {
    const { container, root } = await renderModal();
    schedules.create.mockResolvedValue({ code: 0, data: { sid: 'sch-new' } });
    enablePeriodic(container);

    const name = container.querySelector('input[placeholder="CPU采样-nginx"]');
    act(() => Simulate.change(name, { target: { value: '周期计划' } }));

    // 点击"每 1 分钟"预设
    const presetBtn = Array.from(container.querySelectorAll('button')).find(b => b.textContent === '每 1 分钟');
    act(() => Simulate.click(presetBtn));

    const submit = Array.from(container.querySelectorAll('button')).find(b => b.textContent === '创建周期性深度采样');
    await act(async () => Simulate.click(submit));

    expect(schedules.create).toHaveBeenCalledWith(expect.objectContaining({
        name: '周期计划',
        interval_seconds: 60,
    }));
    // 不携带 cron_expr / window_seconds
    const call = schedules.create.mock.calls[0][0];
    expect(call.cron_expr).toBeUndefined();
    expect(call.window_seconds).toBeUndefined();

    act(() => root.unmount());
    container.remove();
});

test('自定义间隔（分钟）生效', async () => {
    const { container, root } = await renderModal();
    schedules.create.mockResolvedValue({ code: 0, data: { sid: 'sch-custom' } });
    enablePeriodic(container);

    const name = container.querySelector('input[placeholder="CPU采样-nginx"]');
    act(() => Simulate.change(name, { target: { value: '自定义周期' } }));

    const customInput = container.querySelector('input[aria-label="自定义采样间隔（分钟）"]');
    act(() => Simulate.change(customInput, { target: { value: '3' } }));
    expect(container.textContent).toContain('每 3 分钟');

    const submit = Array.from(container.querySelectorAll('button')).find(b => b.textContent === '创建周期性深度采样');
    await act(async () => Simulate.click(submit));
    expect(schedules.create).toHaveBeenCalledWith(expect.objectContaining({ interval_seconds: 180 }));

    act(() => root.unmount());
    container.remove();
});

test('采样时长等于或超过间隔时禁止提交', async () => {
    const { container, root } = await renderModal();
    enablePeriodic(container);

    const name = container.querySelector('input[placeholder="CPU采样-nginx"]');
    act(() => Simulate.change(name, { target: { value: '重叠计划' } }));

    // 默认间隔 300s，把采样时长改成 300（等于间隔）→ 触发重叠提示并禁止提交
    const durationInput = container.querySelector('#duration');
    act(() => Simulate.change(durationInput, { target: { value: '300' } }));

    expect(container.textContent).toContain('相邻窗口会重叠');
    const submit = Array.from(container.querySelectorAll('button')).find(b => b.textContent === '创建周期性深度采样');
    expect(submit.disabled).toBe(false); // 按钮不禁用，但点击会报错
    await act(async () => Simulate.click(submit));
    expect(schedules.create).not.toHaveBeenCalled();
    expect(container.textContent).toContain('需小于采样间隔');

    act(() => root.unmount());
    container.remove();
});

test('指定开始时间时提交 start_at（ISO UTC）', async () => {
    const { container, root } = await renderModal();
    schedules.create.mockResolvedValue({ code: 0, data: { sid: 'sch-start' } });
    enablePeriodic(container);

    const name = container.querySelector('input[placeholder="CPU采样-nginx"]');
    act(() => Simulate.change(name, { target: { value: '定时开始' } }));

    const scheduleRadio = Array.from(container.querySelectorAll('input[type="radio"]')).find(r => r.closest('label')?.textContent.includes('指定时间'));
    act(() => Simulate.change(scheduleRadio, { target: { checked: true } }));

    const startInput = container.querySelector('input[aria-label="计划开始时间"]');
    act(() => Simulate.change(startInput, { target: { value: '2030-01-01T08:00' } }));

    const submit = Array.from(container.querySelectorAll('button')).find(b => b.textContent === '创建周期性深度采样');
    await act(async () => Simulate.click(submit));
    expect(schedules.create).toHaveBeenCalledWith(expect.objectContaining({
        // datetime-local 无时区 → 按浏览器本地时区转 ISO（UTC）
        start_at: new Date('2030-01-01T08:00').toISOString(),
    }));

    act(() => root.unmount());
    container.remove();
});
