import { continuousStateLabel, decodeJSONField, selectorIdentity, selectorModeLabel, signalLabel } from './continuous';

test('continuous task states and signals have stable user-facing labels', () => {
    expect(continuousStateLabel('waiting')).toBe('等待进程');
    expect(continuousStateLabel('degraded')).toBe('降级运行');
    expect(signalLabel('io_syscall_latency')).toBe('系统调用 IO');
});

test('JSON fields accept raw arrays and base64 encoded JSON', () => {
    expect(decodeJSONField(['cpu_profile'])).toEqual(['cpu_profile']);
    const encoded = window.btoa(JSON.stringify([{ pid: 42, process_start_ms: 1000 }]));
    expect(decodeJSONField(encoded)).toEqual([{ pid: 42, process_start_ms: 1000 }]);
});

// 阶段六：selector 身份与跟随策略展示。
test('selector identity describes exact instance, exe, cgroup and container', () => {
    expect(selectorModeLabel('pid_instance')).toBe('单进程实例');
    expect(selectorModeLabel('exe_all_instances')).toBe('同路径全部实例');

    const pid = selectorIdentity({ selector_mode: 'pid_instance', selector_exe: '/opt/api', selector_params: { pid: 42, process_start_ms: 1724160000000, exe: '/opt/api' } });
    expect(pid.mode).toBe('pid_instance');
    expect(pid.exe).toBe('/opt/api');
    expect(pid.detail).toContain('PID 42');
    expect(pid.follow).toContain('不跟随重启');

    const exe = selectorIdentity({ selector_mode: 'exe_all_instances', selector_exe: '/opt/api' });
    expect(exe.mode).toBe('exe_all_instances');
    expect(exe.detail).toBe('全部实例');
    expect(exe.follow).toContain('自动跟随');

    const cgroup = selectorIdentity({ selector_mode: 'cgroup', selector_params: { cgroup: '/system.slice/docker-abc.scope' } });
    expect(cgroup.detail).toBe('/system.slice/docker-abc.scope');
    expect(cgroup.follow).toContain('cgroup');

    const container = selectorIdentity({ selector_mode: 'container_id', selector_params: { container_id: 'abc123def456' } });
    expect(container.detail).toBe('abc123def456');
    expect(container.follow).toContain('容器');
});
