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

    // 阶段六：默认 selector 类型是 pid_instance（具体实例），可切换 exe 全实例。
    expect(container.textContent).toContain('单进程实例');
    expect(container.textContent).toContain('同路径全部实例');
    expect(container.textContent).toContain('PID 42');
    expect(container.textContent).toContain('不跟随重启');
    expect(container.textContent).toContain('我已了解并允许降级运行');
    expect(container.textContent).toContain('采集信号');

    const name = container.querySelector('input[placeholder="例如：API 服务持续剖析"]');
    // 进程按 RSS 排序（43 在前），显式选中 PID 42 的实例。
    const pid42Label = Array.from(container.querySelectorAll('label.continuous-process-row')).find(label => label.textContent.includes('PID 42'));
    const instance = pid42Label.querySelector('input[type="radio"]');
    const checkboxes = Array.from(container.querySelectorAll('input[type="checkbox"]'));
    const signalCheckboxes = checkboxes.slice(0, 4);
    const confirmation = checkboxes[4];
    const submit = Array.from(container.querySelectorAll('button')).find(button => button.textContent === '创建并开始采集');
    act(() => {
        Simulate.change(name, { target: { value: 'API 服务' } });
        Simulate.change(instance, { target: { checked: true } });
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
        selector_mode: 'pid_instance',
        selector_params: { pid: 42, process_start_ms: 1000, exe: '/opt/api' },
        signals: expect.arrayContaining(['cpu_profile', 'io_latency']),
        allow_degraded: true,
    }));
    expect(onSuccess).toHaveBeenCalledWith({ sid: 'cps-new' });

    act(() => root.unmount());
    container.remove();
});

// 阶段六：切换 exe_all_instances 后按 exe 分组选择，提交 exe_all_instances payload。
test('switching to exe_all_instances submits exe selector', async () => {
    continuous.processes.mockResolvedValue({
        code: 0,
        data: {
            fresh: true,
            agent_state: { strict_capable: true },
            processes: [
                { pid: 42, process_start_ms: 1000, comm: 'api', exe: '/opt/api', rss_bytes: 1024 },
                { pid: 43, process_start_ms: 2000, comm: 'api', exe: '/opt/api', rss_bytes: 2048 },
            ],
        },
    });
    continuous.createSession.mockResolvedValue({ code: 0, data: { session: { sid: 'cps-exe' } } });
    const onSuccess = jest.fn();
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<CreateContinuousSessionModal target={{ ip: '10.0.0.8', hostname: 'node' }} onClose={() => {}} onSuccess={onSuccess} />);
    });
    await act(async () => { await Promise.resolve(); });

    const exeTab = Array.from(container.querySelectorAll('button')).find(button => button.textContent === '同路径全部实例');
    act(() => Simulate.click(exeTab));
    expect(container.textContent).toContain('跟随该 exe 的全部实例');
    expect(container.textContent).toContain('2 个实例');

    const name = container.querySelector('input[placeholder="例如：API 服务持续剖析"]');
    const exe = container.querySelector('input[type="radio"]');
    const submit = Array.from(container.querySelectorAll('button')).find(button => button.textContent === '创建并开始采集');
    act(() => {
        Simulate.change(name, { target: { value: 'API 服务' } });
        Simulate.change(exe, { target: { checked: true } });
    });
    // strict_capable=true 时无降级确认 checkbox，直接可提交。
    expect(submit.disabled).toBe(false);

    await act(async () => Simulate.click(submit));
    expect(continuous.createSession).toHaveBeenCalledWith(expect.objectContaining({
        scope: 'process',
        selector_exe: '/opt/api',
        selector_mode: 'exe_all_instances',
        selector_params: { exe: '/opt/api' },
    }));
    expect(onSuccess).toHaveBeenCalledWith({ sid: 'cps-exe' });

    act(() => root.unmount());
    container.remove();
});

// 阶段六：cgroup/container_id 从 Agent 快照选择。
test('cgroup selector picks from snapshot', async () => {
    continuous.processes.mockResolvedValue({
        code: 0,
        data: {
            fresh: true,
            agent_state: { strict_capable: true },
            processes: [
                { pid: 42, process_start_ms: 1000, comm: 'python', exe: '/usr/bin/python3', rss_bytes: 1024, cgroup_path: '/system.slice/docker-abc123def456.scope', container_id: 'abc123def456' },
                { pid: 43, process_start_ms: 2000, comm: 'nginx', exe: '/usr/sbin/nginx', rss_bytes: 2048, cgroup_path: '/system.slice/nginx.service', container_id: '' },
            ],
        },
    });
    continuous.createSession.mockResolvedValue({ code: 0, data: { session: { sid: 'cps-cg' } } });
    const onSuccess = jest.fn();
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<CreateContinuousSessionModal target={{ ip: '10.0.0.8', hostname: 'node' }} onClose={() => {}} onSuccess={onSuccess} />);
    });
    await act(async () => { await Promise.resolve(); });

    const cgroupTab = Array.from(container.querySelectorAll('button')).find(button => button.textContent === 'cgroup');
    act(() => Simulate.click(cgroupTab));
    expect(container.textContent).toContain('/system.slice/docker-abc123def456.scope');
    // cgroup 按 RSS 排序（nginx 在前），显式选中 docker cgroup。
    const dockerLabel = Array.from(container.querySelectorAll('label.continuous-process-row')).find(label => label.textContent.includes('docker-abc123def456'));
    const cgroupRadio = dockerLabel.querySelector('input[type="radio"]');
    const name = container.querySelector('input[placeholder="例如：API 服务持续剖析"]');
    const submit = Array.from(container.querySelectorAll('button')).find(button => button.textContent === '创建并开始采集');
    act(() => {
        Simulate.change(name, { target: { value: '容器组' } });
        Simulate.change(cgroupRadio, { target: { checked: true } });
    });
    expect(submit.disabled).toBe(false);
    await act(async () => Simulate.click(submit));
    expect(continuous.createSession).toHaveBeenCalledWith(expect.objectContaining({
        scope: 'process',
        selector_mode: 'cgroup',
        selector_params: { cgroup: '/system.slice/docker-abc123def456.scope' },
    }));
    expect(onSuccess).toHaveBeenCalledWith({ sid: 'cps-cg' });

    act(() => root.unmount());
    container.remove();
});

test('container_id selector picks from snapshot', async () => {
    continuous.processes.mockResolvedValue({
        code: 0,
        data: {
            fresh: true,
            agent_state: { strict_capable: true },
            processes: [
                { pid: 42, process_start_ms: 1000, comm: 'python', exe: '/usr/bin/python3', rss_bytes: 1024, cgroup_path: '/system.slice/docker-abc123def456.scope', container_id: 'abc123def456' },
                { pid: 43, process_start_ms: 2000, comm: 'nginx', exe: '/usr/sbin/nginx', rss_bytes: 2048, cgroup_path: '/system.slice/nginx.service', container_id: '' },
            ],
        },
    });
    continuous.createSession.mockResolvedValue({ code: 0, data: { session: { sid: 'cps-ct' } } });
    const onSuccess = jest.fn();
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<CreateContinuousSessionModal target={{ ip: '10.0.0.8', hostname: 'node' }} onClose={() => {}} onSuccess={onSuccess} />);
    });
    await act(async () => { await Promise.resolve(); });

    const containerTab = Array.from(container.querySelectorAll('button')).find(button => button.textContent === '容器');
    act(() => Simulate.click(containerTab));
    expect(container.textContent).toContain('abc123def456');
    const containerRadio = container.querySelector('input[type="radio"]');
    const name = container.querySelector('input[placeholder="例如：API 服务持续剖析"]');
    const submit = Array.from(container.querySelectorAll('button')).find(button => button.textContent === '创建并开始采集');
    act(() => {
        Simulate.change(name, { target: { value: '容器组' } });
        Simulate.change(containerRadio, { target: { checked: true } });
    });
    expect(submit.disabled).toBe(false);
    await act(async () => Simulate.click(submit));
    expect(continuous.createSession).toHaveBeenCalledWith(expect.objectContaining({
        scope: 'process',
        selector_mode: 'container_id',
        selector_params: { container_id: 'abc123def456' },
    }));
    expect(onSuccess).toHaveBeenCalledWith({ sid: 'cps-ct' });

    act(() => root.unmount());
    container.remove();
});

// 保留时间与后端一致：原始数据最长保留 24 小时。24h 可提交，25h 不可提交，
// UI 不再出现 1–720h 的旧限制。
test('保留时间限制为 24 小时：24h 可提交、25h 不可提交', async () => {
    continuous.processes.mockResolvedValue({
        code: 0,
        data: {
            fresh: true,
            agent_state: { strict_capable: true },
            processes: [
                { pid: 42, process_start_ms: 1000, comm: 'api', exe: '/opt/api', rss_bytes: 1024 },
            ],
        },
    });
    continuous.createSession.mockResolvedValue({ code: 0, data: { session: { sid: 'cps-ret' } } });
    const onSuccess = jest.fn();
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<CreateContinuousSessionModal target={{ ip: '10.0.0.8', hostname: 'node' }} onClose={() => {}} onSuccess={onSuccess} />);
    });
    await act(async () => { await Promise.resolve(); });

    // 高级设置中的保留时间输入框：max 为 24（不再 720）
    const retentionInput = container.querySelector('#cps-retention');
    expect(retentionInput).toBeTruthy();
    expect(retentionInput.getAttribute('max')).toBe('24');
    // 悬停说明提到 1–24
    expect(container.textContent).toContain('最长保留 24 小时');
    expect(container.textContent).not.toContain('1–720');

    const name = container.querySelector('input[placeholder="例如：API 服务持续剖析"]');
    act(() => Simulate.change(name, { target: { value: '保留测试' } }));
    const radio = container.querySelector('input[type="radio"]');
    act(() => Simulate.change(radio, { target: { checked: true } }));
    const submit = Array.from(container.querySelectorAll('button')).find(button => button.textContent === '创建并开始采集');

    // 25h → 无效，提交禁用
    act(() => Simulate.change(retentionInput, { target: { value: '25' } }));
    expect(submit.disabled).toBe(true);

    // 24h → 有效，可提交
    act(() => Simulate.change(retentionInput, { target: { value: '24' } }));
    expect(submit.disabled).toBe(false);
    await act(async () => Simulate.click(submit));
    expect(continuous.createSession).toHaveBeenCalledWith(expect.objectContaining({ retention_hours: 24 }));
    expect(onSuccess).toHaveBeenCalledWith({ sid: 'cps-ret' });

    act(() => root.unmount());
    container.remove();
});
