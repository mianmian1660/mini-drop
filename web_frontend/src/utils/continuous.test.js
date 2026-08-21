import { continuousStateLabel, decodeJSONField, signalLabel } from './continuous';

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
