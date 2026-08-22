// ============================================================
// pages/TimelinePage.test.js — 周期性深度采样时间轴旧兼容入口测试
// ============================================================
// /timeline 展示全局周期任务列表（复用 ScheduleList，detailPrefix=/schedules），
// 点击周期任务进入独立时间轴详情页 /schedules/:sid。
// ============================================================

import React from 'react';
import { createRoot } from 'react-dom/client';
import { act } from 'react-dom/test-utils';

jest.mock('react-router-dom', () => ({
    Link: ({ children, to }) => <a href={to}>{children}</a>,
}));

jest.mock('../components/ScheduleList', () => ({ detailPrefix }) => (
    <div data-testid="schedule-list" data-prefix={detailPrefix} />
));

import TimelinePage from './TimelinePage';

global.IS_REACT_ACT_ENVIRONMENT = true;

test('/timeline 渲染全局周期任务列表并指向 /schedules/:sid 详情', async () => {
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<TimelinePage />);
    });
    await act(async () => { await Promise.resolve(); });

    // 引导文案
    expect(container.textContent).toContain('周期性深度采样时间轴');
    expect(container.textContent).toContain('请选择周期任务进入独立时间轴');

    // 复用全局周期任务列表（ScheduleList），详情前缀 /schedules
    const list = container.querySelector('[data-testid="schedule-list"]');
    expect(list).toBeTruthy();
    expect(list.getAttribute('data-prefix')).toBe('/schedules');

    // 返回主机列表链接
    const homeLink = Array.from(container.querySelectorAll('a')).find(a => a.textContent.includes('返回主机列表'));
    expect(homeLink).toBeTruthy();
    expect(homeLink.getAttribute('href')).toBe('/');

    act(() => root.unmount());
    container.remove();
});
