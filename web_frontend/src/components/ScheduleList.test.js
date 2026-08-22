// ============================================================
// components/ScheduleList.test.js — 周期任务列表组件测试
// ============================================================
// 覆盖：主机与全局周期列表都只展示周期计划（数据来自 schedules.list）；
// 列表字段与启停状态；点击进入正确详情路由（detailPrefix/sid）；
// 启停、删除操作；主机过滤（targetIp 传入 target_ip）。
// ============================================================

import React from 'react';
import { createRoot } from 'react-dom/client';
import { act, Simulate } from 'react-dom/test-utils';

jest.mock('react-router-dom', () => ({
    Link: ({ children, to }) => <a href={to}>{children}</a>,
}));

jest.mock('../api', () => ({
    schedules: { list: jest.fn(), toggle: jest.fn(), delete: jest.fn() },
}));

jest.mock('./Pagination', () => () => null);

jest.mock('../utils/collectors', () => ({
    collectorLabelFromTask: () => 'perf CPU 火焰图',
}));

jest.mock('../utils/time', () => ({
    formatDateTime: (v) => String(v || ''),
}));

import ScheduleList from './ScheduleList';
import { schedules } from '../api';

global.IS_REACT_ACT_ENVIRONMENT = true;

beforeEach(() => {
    jest.clearAllMocks();
    window.confirm = jest.fn(() => true);
    window.alert = jest.fn();
});

const seedSchedules = [
    { sid: 'sch-1', name: 'CPU 计划', cron_expr: '*/5 * * * *', target_ip: '1.2.3.4', task_kind: 'perf_cpu', enabled: true, user_name: 'user-a', can_manage: true, next_run_at: '2026-08-22T01:00:00Z' },
    { sid: 'sch-2', name: 'IO 计划', cron_expr: '*/10 * * * *', target_ip: '5.6.7.8', task_kind: 'ebpf_io', enabled: false, user_name: 'user-b', can_manage: true },
];

test('全局周期列表渲染周期计划，查看链接指向 /schedules/:sid', async () => {
    schedules.list.mockResolvedValue({ code: 0, data: { schedules: seedSchedules, total: 2 } });

    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<ScheduleList detailPrefix="/schedules" />);
    });
    await act(async () => { await Promise.resolve(); });

    // 只展示周期计划（来自 schedules.list），包含名称 / SID / 状态 / cron / 创建者
    expect(container.textContent).toContain('CPU 计划');
    expect(container.textContent).toContain('IO 计划');
    expect(container.textContent).toContain('sch-1');
    expect(container.textContent).toContain('每 5 分钟');
    expect(container.textContent).toContain('启用');
    expect(container.textContent).toContain('停用');
    expect(container.textContent).toContain('user-a');
    expect(container.textContent).toContain('perf CPU 火焰图');

    // 查看链接 → detailPrefix + sid
    const viewLink = Array.from(container.querySelectorAll('a')).find(a => a.textContent.includes('CPU 计划'));
    expect(viewLink).toBeTruthy();
    expect(viewLink.getAttribute('href')).toBe('/schedules/sch-1');

    act(() => root.unmount());
    container.remove();
});

test('主机周期列表按 target_ip 过滤，查看链接指向 /hosts/:id/schedules/:sid', async () => {
    schedules.list.mockResolvedValue({ code: 0, data: { schedules: [seedSchedules[0]], total: 1 } });

    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<ScheduleList targetIp="1.2.3.4" detailPrefix="/hosts/host-1/schedules" />);
    });
    await act(async () => { await Promise.resolve(); });

    // 请求携带 target_ip
    expect(schedules.list).toHaveBeenCalledWith(expect.objectContaining({ target_ip: '1.2.3.4' }));

    const viewLink = Array.from(container.querySelectorAll('a')).find(a => a.textContent.includes('CPU 计划'));
    expect(viewLink.getAttribute('href')).toBe('/hosts/host-1/schedules/sch-1');

    act(() => root.unmount());
    container.remove();
});

test('启停与删除操作调用对应 API，删除需确认', async () => {
    schedules.list.mockResolvedValue({ code: 0, data: { schedules: seedSchedules, total: 2 } });
    schedules.toggle.mockResolvedValue({ code: 0 });
    schedules.delete.mockResolvedValue({ code: 0 });

    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<ScheduleList detailPrefix="/schedules" />);
    });
    await act(async () => { await Promise.resolve(); });

    // 启用的计划：操作按钮为"停用"；停用的计划为"启用"
    const toggleButtons = Array.from(container.querySelectorAll('button')).filter(b => b.textContent === '停用' || b.textContent === '启用');
    expect(toggleButtons.length).toBe(2);

    // 点击第一个"停用" → schedules.toggle(sch-1)
    await act(async () => {
        Simulate.click(toggleButtons[0]);
    });
    await act(async () => { await Promise.resolve(); });
    expect(schedules.toggle).toHaveBeenCalledWith('sch-1');

    // 删除 → confirm + schedules.delete
    const deleteButtons = Array.from(container.querySelectorAll('button')).filter(b => b.textContent === '删除');
    await act(async () => {
        Simulate.click(deleteButtons[0]);
    });
    expect(window.confirm).toHaveBeenCalled();
    expect(schedules.delete).toHaveBeenCalledWith('sch-1');

    act(() => root.unmount());
    container.remove();
});
