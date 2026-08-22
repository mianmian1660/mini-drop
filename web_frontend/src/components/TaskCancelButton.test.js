// ============================================================
// components/TaskCancelButton.test.js — 任务"停止"按钮单测
// ============================================================
// 覆盖（3a6230f 专项）：非活跃状态不渲染 / 确认 / 成功回调 / 失败提示 /
// 确认被拒时不下发
// ============================================================

import React from 'react';
import { createRoot } from 'react-dom/client';
import { act, Simulate } from 'react-dom/test-utils';

jest.mock('../api', () => ({
    tasks: { cancel: jest.fn() },
}));

import TaskCancelButton from './TaskCancelButton';
import { tasks } from '../api';

global.IS_REACT_ACT_ENVIRONMENT = true;

beforeEach(() => {
    jest.clearAllMocks();
    window.confirm = jest.fn(() => true);
    window.alert = jest.fn();
});

test('非活跃状态或不可管理时不渲染', () => {
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    act(() => {
        root.render(<TaskCancelButton tid="1" status={2} />); // 已完成
    });
    expect(container.textContent).toBe('');
    act(() => root.unmount());
    container.remove();

    const c2 = document.createElement('div');
    document.body.appendChild(c2);
    const root2 = createRoot(c2);
    act(() => {
        root2.render(<TaskCancelButton tid="1" status={0} canManage={false} />);
    });
    expect(c2.textContent).toBe('');
    act(() => root2.unmount());
    c2.remove();
});

test('确认后调用取消接口并触发 onCancelled', async () => {
    window.confirm = jest.fn(() => true);
    tasks.cancel.mockResolvedValue({ code: 0 });
    const onCancelled = jest.fn();
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    act(() => {
        root.render(<TaskCancelButton tid="7" status={1} onCancelled={onCancelled} />);
    });
    expect(container.textContent).toContain('停止');
    await act(async () => {
        Simulate.click(container.querySelector('button'));
    });
    expect(window.confirm).toHaveBeenCalledWith('确定停止任务 7 吗？');
    expect(tasks.cancel).toHaveBeenCalledWith('7');
    expect(onCancelled).toHaveBeenCalled();
    act(() => root.unmount());
    container.remove();
});

test('取消确认被拒绝时不下发接口', async () => {
    window.confirm = jest.fn(() => false);
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    act(() => {
        root.render(<TaskCancelButton tid="8" status={4} />);
    });
    await act(async () => {
        Simulate.click(container.querySelector('button'));
    });
    expect(tasks.cancel).not.toHaveBeenCalled();
    act(() => root.unmount());
    container.remove();
});

test('取消失败时弹出错误提示', async () => {
    window.confirm = jest.fn(() => true);
    tasks.cancel.mockRejectedValue({ response: { data: { message: '后端拒绝停止' } } });
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    act(() => {
        root.render(<TaskCancelButton tid="9" status={1} />);
    });
    await act(async () => {
        Simulate.click(container.querySelector('button'));
    });
    expect(window.alert).toHaveBeenCalledWith(expect.stringContaining('后端拒绝停止'));
    act(() => root.unmount());
    container.remove();
});
