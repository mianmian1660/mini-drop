// pages/TimelinePage.test.js — 全局周期任务入口页测试
// 计划列表组件与独立时间轴详情分别由各自测试覆盖；这里验证页面组合与路由前缀。

import React, { act } from 'react';
import { createRoot } from 'react-dom/client';

jest.mock('react-router-dom', () => ({
    Link: ({ children, to }) => <a href={to}>{children}</a>,
}));

jest.mock('../components/ScheduleList', () => ({ detailPrefix }) => (
    <div data-testid="schedule-list" data-detail-prefix={detailPrefix}>周期任务列表</div>
));

import TimelinePage from './TimelinePage';

global.IS_REACT_ACT_ENVIRONMENT = true;

test('渲染周期任务入口并将详情链接前缀设为 /schedules', async () => {
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);

    await act(async () => {
        root.render(<TimelinePage />);
    });

    expect(container.textContent).toContain('周期性深度采样时间轴');
    expect(container.textContent).toContain('请选择周期任务进入独立时间轴');
    expect(container.querySelector('a[href="/"]')).toBeTruthy();

    const list = container.querySelector('[data-testid="schedule-list"]');
    expect(list).toBeTruthy();
    expect(list.getAttribute('data-detail-prefix')).toBe('/schedules');

    await act(async () => {
        root.unmount();
    });
    container.remove();
});
