import React from 'react';
import { createRoot } from 'react-dom/client';
import { act, Simulate } from 'react-dom/test-utils';
import { continuous } from '../api';
import CreateContinuousSessionModal from './CreateContinuousSessionModal';

jest.mock('../api', () => ({
    continuous: {
        processes: jest.fn(),
        createSession: jest.fn(),
    },
}));

global.IS_REACT_ACT_ENVIRONMENT = true;

test('process creation follows all exe instances and requires degraded confirmation', async () => {
    continuous.processes.mockResolvedValue({
        code: 0,
        data: {
            fresh: true,
            agent_state: { strict_capable: false },
            processes: [
                { pid: 42, process_start_ms: 1000, comm: 'api', exe: '/opt/api', rss_bytes: 1024 },
                { pid: 43, process_start_ms: 2000, comm: 'api', exe: '/opt/api', rss_bytes: 2048 },
            ],
        },
    });
    continuous.createSession.mockResolvedValue({ code: 0, data: { session: { sid: 'cps-new' } } });
    const onSuccess = jest.fn();
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<CreateContinuousSessionModal target={{ ip: '10.0.0.8', hostname: 'node' }} onClose={() => {}} onSuccess={onSuccess} />);
    });
    await act(async () => { await Promise.resolve(); });

    expect(container.textContent).toContain('跟随该 exe 的全部实例');
    expect(container.textContent).toContain('2 个实例');
    expect(container.textContent).toContain('我已了解并允许降级运行');
    expect(container.textContent).toContain('采集信号');

    const name = container.querySelector('input[placeholder="例如：API 服务持续剖析"]');
    const exe = container.querySelector('input[type="radio"]');
    const checkboxes = Array.from(container.querySelectorAll('input[type="checkbox"]'));
    const signalCheckboxes = checkboxes.slice(0, 4);
    const confirmation = checkboxes[4];
    const submit = Array.from(container.querySelectorAll('button')).find(button => button.textContent === '创建并开始采集');
    act(() => {
        Simulate.change(name, { target: { value: 'API 服务' } });
        Simulate.change(exe, { target: { checked: true } });
    });
    expect(submit.disabled).toBe(true);
    act(() => Simulate.change(signalCheckboxes[1], { target: { checked: true } }));
    act(() => Simulate.change(confirmation, { target: { checked: true } }));
    expect(submit.disabled).toBe(false);

    await act(async () => Simulate.click(submit));
    expect(continuous.createSession).toHaveBeenCalledWith(expect.objectContaining({
        name: 'API 服务',
        scope: 'process',
        selector_exe: '/opt/api',
        selector_mode: 'all_instances',
        signals: expect.arrayContaining(['cpu_profile', 'io_latency']),
        allow_degraded: true,
    }));
    expect(onSuccess).toHaveBeenCalledWith({ sid: 'cps-new' });

    act(() => root.unmount());
    container.remove();
});
