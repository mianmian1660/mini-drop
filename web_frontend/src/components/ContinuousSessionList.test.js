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
                desired_state: 'running', observed_state: 'waiting', continuity_mode: 'degraded',
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
    expect(container.textContent).toContain('/opt/worker');
    expect(container.querySelector('.table-scroll')).not.toBeNull();
    expect(container.querySelector('.table-scroll').style.maxWidth).toBe('100%');
    expect(container.textContent).toContain('共 1 条');
    expect(continuous.sessions).toHaveBeenCalledWith(expect.objectContaining({ page: 1, page_size: 20, owner_filter: 'all' }));
    const stop = Array.from(container.querySelectorAll('button')).find(button => button.textContent === '停止');
    await act(async () => Simulate.click(stop));
    expect(window.confirm).toHaveBeenCalled();
    expect(continuous.stopSession).toHaveBeenCalledWith('cps-worker');

    act(() => root.unmount());
    container.remove();
});

test('list uses server pagination so every shared session remains reachable', async () => {
    continuous.sessions
        .mockResolvedValueOnce({ code: 0, data: { total: 41, sessions: [{ sid: 'page-1', name: 'First page', scope: 'host', desired_state: 'stopped' }] } })
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
    const next = Array.from(container.querySelectorAll('button')).find(button => button.textContent === '下一页');
    await act(async () => Simulate.click(next));
    await act(async () => { await Promise.resolve(); });

    expect(continuous.sessions).toHaveBeenLastCalledWith(expect.objectContaining({ page: 2, page_size: 20 }));
    expect(container.textContent).toContain('Second page');

    act(() => root.unmount());
    container.remove();
});
