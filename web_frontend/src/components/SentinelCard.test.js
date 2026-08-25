import React from 'react';
import { createRoot } from 'react-dom/client';
import { act, Simulate } from 'react-dom/test-utils';

jest.mock('../api', () => ({
    sentinelRules: {
        list: jest.fn(),
        create: jest.fn(),
        delete: jest.fn(),
    },
}));

import { sentinelRules } from '../api';
import SentinelCard from './SentinelCard';

async function render(props) {
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<SentinelCard {...props} />);
    });
    await act(async () => { await Promise.resolve(); });
    return { container, root };
}

beforeEach(() => {
    sentinelRules.list.mockReset();
    sentinelRules.create.mockReset();
    sentinelRules.delete.mockReset();
});

test('没有 histogram 信号也没有 db_targets 时提示不可用，且提到数据库', async () => {
    sentinelRules.list.mockResolvedValue({ code: 0, data: { rules: [] } });
    const { container, root } = await render({ targetIP: '10.0.0.9', signals: ['cpu_profile'], hasDBTargets: false });

    expect(container.textContent).toContain('当前会话没有可用于哨兵判异的信号');
    expect(container.textContent).toContain('数据库');

    act(() => root.unmount());
    container.remove();
});

test('数据库规则展示不复用 histogram 的 ×1000 换算：锁等待按秒展示，慢SQL按倍数展示', async () => {
    sentinelRules.list.mockResolvedValue({
        code: 0,
        data: {
            rules: [
                { sid: 'sr-sched', signal: 'sched_latency', metric: 'p99', floor_value: 5000, cooldown_seconds: 900, can_manage: true },
                { sid: 'sr-lock', signal: 'db_snapshot', metric: 'lock_wait', floor_value: 3, cooldown_seconds: 900, can_manage: true },
                { sid: 'sr-digest', signal: 'db_snapshot', metric: 'digest', floor_value: 50000, k_factor: 5, cooldown_seconds: 900, can_manage: true },
            ],
        },
    });
    const { container, root } = await render({ targetIP: '10.0.0.9', signals: ['sched_latency'], hasDBTargets: true });

    expect(container.textContent).toContain('调度 · p99 > 5 ms');
    // 锁等待：3（秒），不能被误当成 ms 除以 1000 变成 0.003
    expect(container.textContent).toContain('数据库 · 锁等待 > 3 s');
    expect(container.textContent).not.toContain('锁等待 > 0.003');
    // 慢SQL环比：5倍，下限 50ms（50000us / 1000）
    expect(container.textContent).toContain('数据库 · 慢SQL环比 > 5 倍（下限 50 ms）');

    act(() => root.unmount());
    container.remove();
});

test('添加哨兵表单：选中数据库后可以在锁等待/慢SQL环比之间切换字段', async () => {
    sentinelRules.list.mockResolvedValue({ code: 0, data: { rules: [] } });
    const { container, root } = await render({ targetIP: '10.0.0.9', signals: ['sched_latency'], hasDBTargets: true });

    act(() => Simulate.click(container.querySelector('button')));

    const signalSelect = container.querySelectorAll('select')[0];
    act(() => Simulate.change(signalSelect, { target: { value: 'db_snapshot' } }));

    expect(container.textContent).toContain('判异方式');
    let metricSelect = container.querySelectorAll('select')[1];
    expect(metricSelect.value).toBe('lock_wait');
    expect(container.textContent).toContain('等待阈值（秒）');
    expect(container.textContent).not.toContain('最小耗时下限');

    act(() => Simulate.change(metricSelect, { target: { value: 'digest' } }));
    expect(container.textContent).toContain('最小耗时下限（ms）');
    expect(container.textContent).toContain('环比倍数');
    expect(container.textContent).not.toContain('等待阈值（秒）');

    act(() => root.unmount());
    container.remove();
});

test('添加哨兵表单：锁等待提交按秒传值，不做 ×1000 换算', async () => {
    sentinelRules.list.mockResolvedValue({ code: 0, data: { rules: [] } });
    sentinelRules.create.mockResolvedValue({ code: 0, data: {} });
    const { container, root } = await render({ targetIP: '10.0.0.9', signals: [], hasDBTargets: true });

    act(() => Simulate.click(container.querySelector('button')));
    // signals 为空时表单默认信号就是 db_snapshot，默认判异方式是 DB_SENTINEL_METRICS[0]=lock_wait
    const numberInputs = () => Array.from(container.querySelectorAll('input[type="number"]'));
    const lockInput = numberInputs()[0];
    act(() => Simulate.change(lockInput, { target: { value: '9' } }));

    const submitButton = Array.from(container.querySelectorAll('button')).find(b => b.textContent === '创建哨兵');
    await act(async () => { Simulate.click(submitButton); await Promise.resolve(); await Promise.resolve(); await Promise.resolve(); });

    expect(sentinelRules.create).toHaveBeenCalledWith(expect.objectContaining({
        signal: 'db_snapshot', metric: 'lock_wait', floor_value: 9,
    }));

    act(() => root.unmount());
    container.remove();
});

test('添加哨兵表单：慢SQL环比提交时下限按 ×1000 换算成微秒，倍数原样传', async () => {
    sentinelRules.list.mockResolvedValue({ code: 0, data: { rules: [] } });
    sentinelRules.create.mockResolvedValue({ code: 0, data: {} });
    const { container, root } = await render({ targetIP: '10.0.0.9', signals: [], hasDBTargets: true });

    act(() => Simulate.click(container.querySelector('button')));
    const metricSelect = container.querySelectorAll('select')[1];
    act(() => Simulate.change(metricSelect, { target: { value: 'digest' } }));

    const numberInputs = () => Array.from(container.querySelectorAll('input[type="number"]'));
    const [floorInput, kFactorInput] = numberInputs();
    act(() => Simulate.change(floorInput, { target: { value: '80' } }));
    act(() => Simulate.change(kFactorInput, { target: { value: '4' } }));

    const submitButton = Array.from(container.querySelectorAll('button')).find(b => b.textContent === '创建哨兵');
    await act(async () => { Simulate.click(submitButton); await Promise.resolve(); await Promise.resolve(); await Promise.resolve(); });

    expect(sentinelRules.create).toHaveBeenCalledWith(expect.objectContaining({
        signal: 'db_snapshot', metric: 'digest', floor_value: 80000, k_factor: 4,
    }));

    act(() => root.unmount());
    container.remove();
});
