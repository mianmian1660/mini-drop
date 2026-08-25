// ============================================================
// components/InlineDiffPanel.test.js — 时间窗 Diff 面板单测
// ============================================================
// 覆盖（3a6230f 专项）：正负差异渲染 / 空差异 / 接口失败 / 缺少对比任务不请求
// ============================================================

import React from 'react';
import { createRoot } from 'react-dom/client';
import { act } from 'react-dom/test-utils';

jest.mock('react-router-dom', () => ({
    Link: ({ children, to }) => <a href={to}>{children}</a>,
}));

jest.mock('../api', () => ({
    tasks: { diff: jest.fn(), diffFlamegraph: jest.fn() },
}));

// InteractiveFlamegraph 拉真实 d3-selection/d3-flame-graph（ESM），jest 不跑真实 d3
// （和 ContinuousProfilingPanel.test.js 的处理一致），这里只关心表格 tab 的既有断言。
jest.mock('./InteractiveFlamegraph', () => ({
    __esModule: true,
    default: () => null,
}));

import InlineDiffPanel from './InlineDiffPanel';
import { tasks } from '../api';

global.IS_REACT_ACT_ENVIRONMENT = true;

beforeEach(() => {
    jest.clearAllMocks();
});

test('渲染正负差异表格', async () => {
    tasks.diff.mockResolvedValue({ code: 0, data: {
        baseline: { name: 'b1' },
        compare: { name: 'c1' },
        functions: [
            { function: 'hotFunc', direction: 'up', baseline_percentage: 30, compare_percentage: 45, delta_percentage: 15 },
            { function: 'coldFunc', direction: 'down', baseline_percentage: 20, compare_percentage: 5, delta_percentage: -15 },
        ],
    } });
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<InlineDiffPanel baselineTid="1" compareTid="2" onClose={() => {}} />);
    });
    await act(async () => { await Promise.resolve(); });
    expect(tasks.diff).toHaveBeenCalledWith('1', '2', '1');
    expect(container.textContent).toContain('时间窗 Diff');
    expect(container.textContent).toContain('hotFunc');
    expect(container.textContent).toContain('更热');
    expect(container.textContent).toContain('coldFunc');
    expect(container.textContent).toContain('更冷');
    act(() => root.unmount());
    container.remove();
});

test('空数据时显示无差异提示', async () => {
    tasks.diff.mockResolvedValue({ code: 0, data: { functions: [] } });
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<InlineDiffPanel baselineTid="1" compareTid="2" onClose={() => {}} />);
    });
    await act(async () => { await Promise.resolve(); });
    expect(container.textContent).toContain('没有超过阈值的差异');
    act(() => root.unmount());
    container.remove();
});

test('接口失败显示错误信息', async () => {
    tasks.diff.mockRejectedValue({ response: { data: { message: '对比任务不存在' } } });
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<InlineDiffPanel baselineTid="1" compareTid="999" onClose={() => {}} />);
    });
    await act(async () => { await Promise.resolve(); });
    expect(container.textContent).toContain('对比任务不存在');
    act(() => root.unmount());
    container.remove();
});

test('缺少对比任务时不发起请求', () => {
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    act(() => {
        root.render(<InlineDiffPanel baselineTid="1" compareTid="" onClose={() => {}} />);
    });
    expect(tasks.diff).not.toHaveBeenCalled();
    act(() => root.unmount());
    container.remove();
});
