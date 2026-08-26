import React from 'react';
import { createRoot } from 'react-dom/client';
import { act, Simulate } from 'react-dom/test-utils';
import { MemoryRouter } from 'react-router-dom';
import { continuous } from '../api';
import ContinuousSessionList from './ContinuousSessionList';

jest.mock('../api', () => ({
    continuous: {
        sessions: jest.fn(),
        stopSession: jest.fn(),
    },
}));

global.IS_REACT_ACT_ENVIRONMENT = true;

test('list renders waiting process tasks and stop writes desired state through the API', async () => {
    continuous.sessions.mockResolvedValue({
        code: 0,
        data: {
            total: 1,
            sessions: [{
                sid: 'cps-worker', name: 'Worker', scope: 'process', selector_exe: '/opt/worker',
                selector_mode: 'pid_instance', selector_params: { pid: 42, process_start_ms: 1724160000000, exe: '/opt/worker' },
                desired_state: 'running', observed_state: 'waiting', continuity_mode: 'degraded',
                degradation_reason: 'target pid 42 is not currently present; collection stays waiting and will NOT follow a reused PID or a new process at the same path',
                signals: ['cpu_profile', 'io_latency'], active_processes: [], started_at: '2026-08-20T00:00:00Z',
                can_manage: true,
            }],
        },
    });
    continuous.stopSession.mockResolvedValue({ code: 0, data: { session: { desired_state: 'stopped', observed_state: 'stopping' } } });
    window.confirm = jest.fn(() => true);
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<MemoryRouter><ContinuousSessionList target={{ id: 'target', ip: '10.0.0.8' }} /></MemoryRouter>);
    });
    await act(async () => { await Promise.resolve(); });

    expect(container.textContent).toContain('等待进程');
    expect(container.textContent).toContain('单进程实例');
    expect(container.textContent).toContain('/opt/worker');
    expect(container.textContent).toContain('PID 42');
    expect(container.textContent).toContain('target pid 42 is not currently present');
    expect(container.querySelector('.table-scroll')).not.toBeNull();
    expect(container.querySelector('.table-scroll').style.maxWidth).toBe('100%');
    expect(container.textContent).toContain('共 1 条');
    expect(container.textContent).toContain('全部创建者');
    expect(continuous.sessions).toHaveBeenCalledWith(expect.objectContaining({ page: 1, page_size: 20, owner_filter: 'all' }));
    const ownerHelp = container.querySelector('button[aria-label="查看持续采集归属筛选说明"]');
    await act(async () => Simulate.mouseEnter(ownerHelp));
    expect(container.textContent).toContain('默认显示这台主机上全部创建者的任务和完整分页');
    const stop = Array.from(container.querySelectorAll('button')).find(button => button.textContent === '停止');
    await act(async () => Simulate.click(stop));
    expect(window.confirm).toHaveBeenCalled();
    expect(continuous.stopSession).toHaveBeenCalledWith('cps-worker');

    act(() => root.unmount());
    container.remove();
});

test('list uses server pagination so every shared session remains reachable', async () => {
    continuous.sessions
        .mockResolvedValueOnce({ code: 0, data: { total: 41, sessions: [{ sid: 'page-1', name: 'smoke leftover', scope: 'host', desired_state: 'stopped', observed_state: 'stopped', sample_count: 0, can_manage: true }] } })
        .mockResolvedValueOnce({ code: 0, data: { total: 41, sessions: [{ sid: 'page-2', name: 'Second page', scope: 'host', desired_state: 'stopped' }] } });
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<MemoryRouter><ContinuousSessionList target={{ id: 'target', ip: '10.0.0.8' }} /></MemoryRouter>);
    });
    await act(async () => { await Promise.resolve(); });

    expect(container.textContent).toContain('共 41 条');
    expect(container.textContent).toContain('第 1 / 3 页');
    expect(container.textContent).toContain('清理测试残留（1）');
    const cleanupHelp = container.querySelector('button[aria-label="查看清理测试残留说明"]');
    await act(async () => Simulate.mouseEnter(cleanupHelp));
    expect(container.textContent).toContain('不会删除有采集数据的任务');
    const next = Array.from(container.querySelectorAll('button')).find(button => button.textContent === '下一页');
    await act(async () => Simulate.click(next));
    await act(async () => { await Promise.resolve(); });

    expect(continuous.sessions).toHaveBeenLastCalledWith(expect.objectContaining({ page: 2, page_size: 20 }));
    expect(container.textContent).toContain('Second page');

    act(() => root.unmount());
    container.remove();
});
