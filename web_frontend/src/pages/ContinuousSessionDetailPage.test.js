import React from 'react';
import { createRoot } from 'react-dom/client';
import { act, Simulate } from 'react-dom/test-utils';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { continuous, profiles } from '../api';
import ContinuousSessionDetailPage from './ContinuousSessionDetailPage';

jest.mock('../api', () => ({
    continuous: {
        detail: jest.fn(),
        stopSession: jest.fn(),
    },
    profiles: { targets: jest.fn() },
}));

jest.mock('../components/ContinuousProfilingPanel', () => ({
    __esModule: true,
    default: ({ fixedSession }) => <div data-testid="profiling-panel">{fixedSession.sid}</div>,
}));

global.IS_REACT_ACT_ENVIRONMENT = true;

test('detail route binds the SID to its host and exposes the stopping flow', async () => {
    profiles.targets.mockResolvedValue({ code: 0, data: { targets: [{ id: 'target-1', ip: '10.0.0.8', hostname: 'node' }] } });
    continuous.detail.mockResolvedValue({
        code: 0,
        data: {
            session: {
                sid: 'cps-api', name: 'API', target_ip: '10.0.0.8', scope: 'process', selector_exe: '/opt/api',
                desired_state: 'running', observed_state: 'degraded', continuity_mode: 'degraded',
                active_processes: [{ pid: 42, process_start_ms: 1724160000000 }],
            },
        },
    });
    continuous.stopSession.mockResolvedValue({ code: 0 });
    window.confirm = jest.fn(() => true);
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<MemoryRouter initialEntries={['/hosts/target-1/continuous/cps-api']}>
            <Routes><Route path="/hosts/:targetId/continuous/:sid" element={<ContinuousSessionDetailPage />} /></Routes>
        </MemoryRouter>);
    });
    await act(async () => { await Promise.resolve(); });

    expect(container.textContent).toContain('API');
    expect(container.textContent).toContain('PID 42');
    expect(container.querySelector('[data-testid="profiling-panel"]').textContent).toBe('cps-api');
    const stop = Array.from(container.querySelectorAll('button')).find(button => button.textContent === '停止持续采集');
    await act(async () => Simulate.click(stop));
    expect(continuous.stopSession).toHaveBeenCalledWith('cps-api');

    act(() => root.unmount());
    container.remove();
});
