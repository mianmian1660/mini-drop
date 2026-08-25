// ============================================================
// pages/HostDetailPage.test.js — 主机页单次/周期分类测试
// ============================================================
// 覆盖：概览"最近单次任务"只请求 task_scope=single；概览与"周期任务"Tab
// 都渲染周期任务列表（ScheduleList），且带主机 target_ip 过滤与
// /hosts/:id/schedules 详情前缀。
// ============================================================

import React from 'react';
import { createRoot } from 'react-dom/client';
import { act } from 'react-dom/test-utils';

const mockSearchParams = new URLSearchParams();

jest.mock('react-router-dom', () => ({
    Link: ({ children, to }) => <a href={to}>{children}</a>,
    useParams: () => ({ targetId: 'target-1' }),
    useSearchParams: () => [mockSearchParams, jest.fn()],
}));

jest.mock('../api', () => ({
    agents: { detail: jest.fn() },
    continuous: { sessions: jest.fn(), stopSession: jest.fn() },
    profiles: { targets: jest.fn() },
    schedules: { list: jest.fn() },
    storage: { status: jest.fn() },
    tasks: { list: jest.fn() },
}));

jest.mock('../components/CreateTaskModal', () => () => null);
jest.mock('../components/CreateContinuousSessionModal', () => () => null);
jest.mock('../components/ContinuousSessionList', () => () => null);
jest.mock('../components/Pagination', () => () => null);
jest.mock('../components/ScheduleList', () => ({ detailPrefix, targetIp }) => (
    <div data-testid="schedule-list" data-prefix={detailPrefix} data-target={targetIp || ''} />
));

import HostDetailPage from './HostDetailPage';
import { agents, continuous, profiles, schedules, storage, tasks } from '../api';

global.IS_REACT_ACT_ENVIRONMENT = true;

const target = { id: 'target-1', ip: '1.2.3.4', hostname: 'node', service_name: 'hotmethod', environment: 'prod', drop_agent_status: 'online', profile_status: 'running' };
const singleTasks = [
    { tid: 't1', name: '普通任务', task_kind: 'perf_cpu', status: 2, create_time: '2026-08-22T00:00:00Z', user_name: 'user-a', target_ip: '1.2.3.4' },
];
const hostSchedules = [
    { sid: 'sch-1', name: 'CPU 计划', target_ip: '1.2.3.4', cron_expr: '*/5 * * * *', enabled: true, user_name: 'user-a' },
];

async function setupApiMocks() {
    profiles.targets.mockResolvedValue({ code: 0, data: { targets: [target] } });
    agents.detail.mockResolvedValue({
        code: 0,
        data: {
            agent: { online: true, version: '1.0.0', last_seen: '2026-08-25T10:00:00Z', capabilities: 'perf_cpu' },
            stat: {
                source: 'grpc',
                cpu_percent: 1,
                memory_kb: 100,
                read_kb_per_s: 5,
                write_kb_per_s: 3,
                host: null,
                host_metadata: null,
                host_metadata_source: 'none',
            },
            audits: [],
        },
    });
    tasks.list.mockResolvedValue({ code: 0, data: { tasks: singleTasks, total: 1 } });
    schedules.list.mockResolvedValue({ code: 0, data: { schedules: hostSchedules, total: 1 } });
    continuous.sessions.mockResolvedValue({ code: 0, data: { sessions: [] } });
    storage.status.mockResolvedValue({ code: 0, data: { level: 'normal', available_bytes: 20 * 1024 * 1024 * 1024 } });
}

beforeEach(() => {
    jest.clearAllMocks();
    mockSearchParams.delete('tab');
});

async function renderHost() {
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<HostDetailPage />);
    });
    await act(async () => { await Promise.resolve(); });
    return { container, root };
}

test('概览最近单次任务请求 task_scope=single，周期区域渲染主机周期列表', async () => {
    await setupApiMocks();
    const { container, root } = await renderHost();

    // 概览"最近单次任务"只请求单次任务（排除周期窗口）
    expect(tasks.list).toHaveBeenCalledWith(expect.objectContaining({ task_scope: 'single' }));
    expect(container.textContent).toContain('最近单次任务');

    // 概览周期区域渲染 ScheduleList：目标为该主机、详情前缀为 /hosts/:id/schedules
    const list = container.querySelector('[data-testid="schedule-list"]');
    expect(list).toBeTruthy();
    expect(list.getAttribute('data-prefix')).toBe('/hosts/target-1/schedules');
    expect(list.getAttribute('data-target')).toBe('1.2.3.4');

    act(() => root.unmount());
    container.remove();
});

test('周期任务 Tab 只展示当前主机的周期任务列表', async () => {
    mockSearchParams.set('tab', 'timeline');
    await setupApiMocks();
    const { container, root } = await renderHost();

    const list = container.querySelector('[data-testid="schedule-list"]');
    expect(list).toBeTruthy();
    expect(list.getAttribute('data-prefix')).toBe('/hosts/target-1/schedules');
    expect(list.getAttribute('data-target')).toBe('1.2.3.4');

    act(() => root.unmount());
    container.remove();
});

// ---------- 阶段 0：存储压力提示条 ----------

test('存储压力 normal 时主机上下文不渲染提示条', async () => {
    await setupApiMocks();
    const { container, root } = await renderHost();
    expect(container.querySelector('[data-testid="storage-pressure-banner"]')).toBeNull();
    act(() => root.unmount());
    container.remove();
});

test('存储压力 warning 时主机上下文渲染单行提示并显示剩余容量', async () => {
    await setupApiMocks();
    storage.status.mockResolvedValue({ code: 0, data: { level: 'warning', available_bytes: 6 * 1024 * 1024 * 1024 } });
    const { container, root } = await renderHost();

    const banner = container.querySelector('[data-testid="storage-pressure-banner"]');
    expect(banner).toBeTruthy();
    expect(banner.textContent).toContain('服务端存储空间偏低');
    expect(banner.textContent).toContain('6.0 GiB');

    act(() => root.unmount());
    container.remove();
});

test('存储压力 emergency 时显示新采集已暂停', async () => {
    await setupApiMocks();
    storage.status.mockResolvedValue({ code: 0, data: { level: 'emergency', available_bytes: 512 * 1024 * 1024 } });
    const { container, root } = await renderHost();

    const banner = container.querySelector('[data-testid="storage-pressure-banner"]');
    expect(banner).toBeTruthy();
    expect(banner.textContent).toContain('新采集已暂停');

    act(() => root.unmount());
    container.remove();
});

test('存储状态接口失败时不渲染错误横幅（静默保留上一次成功状态）', async () => {
    await setupApiMocks();
    storage.status.mockRejectedValue(new Error('network down'));
    const { container, root } = await renderHost();
    expect(container.querySelector('[data-testid="storage-pressure-banner"]')).toBeNull();
    act(() => root.unmount());
    container.remove();
});

// ---------- 主机信息（HostMetadata）区域 ----------

const fullHostMetadata = {
    os_name: 'Ubuntu',
    os_version: '24.04',
    kernel_version: '6.8.0-31-generic',
    architecture: 'x86_64',
    cpu_model: 'AMD EPYC 7B12',
    cpu_cores: 8,
    uptime_seconds: 86400,
    collected_at: '2026-08-25T10:30:00Z',
};

test('主机信息区域渲染 OS、内核、架构、CPU 型号、核数', async () => {
    await setupApiMocks();
    agents.detail.mockResolvedValue({
        code: 0,
        data: {
            agent: { online: true, version: '1.0.0', last_seen: '2026-08-25T10:00:00Z', capabilities: 'perf_cpu' },
            stat: {
                source: 'grpc',
                cpu_percent: 1,
                memory_kb: 100,
                host: { cpu_percent: 10, cpu_available: true, memory_percent: 20, memory_available: true, disk_percent: 30, disk_available: true, collected_at: '2026-08-25T10:30:00Z' },
                host_metadata: fullHostMetadata,
                host_metadata_source: 'grpc',
            },
            audits: [],
        },
    });
    const { container, root } = await renderHost();

    expect(container.textContent).toContain('主机信息');
    expect(container.textContent).toContain('Ubuntu 24.04');
    expect(container.textContent).toContain('6.8.0-31-generic');
    expect(container.textContent).toContain('x86_64');
    expect(container.textContent).toContain('AMD EPYC 7B12');
    expect(container.textContent).toContain('8 核');

    act(() => root.unmount());
    container.remove();
});

test('主机元数据缺失时显示明确的未上报状态', async () => {
    await setupApiMocks();
    const { container, root } = await renderHost();

    // 默认 mock：host_metadata=null → 显示"暂未上报"原因，不显示 0 冒充
    expect(container.textContent).toContain('暂未上报主机信息');
    expect(container.textContent).toContain('操作系统');
    expect(container.textContent).toContain('内核版本');

    act(() => root.unmount());
    container.remove();
});

test('gRPC 数据和数据库快照显示不同来源', async () => {
    await setupApiMocks();
    agents.detail.mockResolvedValue({
        code: 0,
        data: {
            agent: { online: true, version: '1.0.0', last_seen: '2026-08-25T10:00:00Z', capabilities: 'perf_cpu' },
            stat: {
                source: 'grpc',
                cpu_percent: 1,
                memory_kb: 100,
                host: null,
                host_metadata: fullHostMetadata,
                host_metadata_source: 'grpc',
            },
            audits: [],
        },
    });
    const { container, root } = await renderHost();
    expect(container.textContent).toContain('实时 gRPC');
    act(() => root.unmount());
    container.remove();

    // 数据库快照来源
    await setupApiMocks();
    agents.detail.mockResolvedValue({
        code: 0,
        data: {
            agent: { online: false, version: '1.0.0', last_seen: '2026-08-25T10:00:00Z', capabilities: 'perf_cpu' },
            stat: {
                source: 'db',
                cpu_percent: 0,
                memory_kb: 0,
                host: null,
                host_metadata: fullHostMetadata,
                host_metadata_source: 'db',
            },
            audits: [],
        },
    });
    const { container: container2, root: root2 } = await renderHost();
    expect(container2.textContent).toContain('数据库快照');
    act(() => root2.unmount());
    container2.remove();
});

test('离线时保留最后已知信息并显示最后采集于', async () => {
    await setupApiMocks();
    agents.detail.mockResolvedValue({
        code: 0,
        data: {
            agent: { online: false, version: '1.0.0', last_seen: '2026-08-25T10:00:00Z', capabilities: 'perf_cpu' },
            stat: {
                source: 'db',
                cpu_percent: 0,
                memory_kb: 0,
                host: { cpu_percent: 10, cpu_available: true, memory_percent: 20, memory_available: true, disk_percent: 30, disk_available: true, collected_at: '2026-08-25T10:30:00Z' },
                host_metadata: fullHostMetadata,
                host_metadata_source: 'db',
            },
            audits: [],
        },
    });
    const { container, root } = await renderHost();

    // 离线时仍显示最后已知主机信息
    expect(container.textContent).toContain('Ubuntu 24.04');
    expect(container.textContent).toContain('AMD EPYC 7B12');
    // 资源块显示采集时间
    expect(container.textContent).toContain('采集于');

    act(() => root.unmount());
    container.remove();
});

// ---------- 运行状态（替换性能状态概览） ----------

test('运行状态显示任务计数和数据新鲜度', async () => {
    await setupApiMocks();
    agents.detail.mockResolvedValue({
        code: 0,
        data: {
            agent: { online: true, version: '1.0.0', last_seen: '2026-08-25T10:00:00Z', capabilities: 'perf_cpu' },
            stat: {
                source: 'grpc',
                cpu_percent: 1,
                memory_kb: 100,
                read_kb_per_s: 5,
                write_kb_per_s: 3,
                host: { cpu_percent: 10, cpu_available: true, memory_percent: 20, memory_available: true, disk_percent: 30, disk_available: true, collected_at: new Date().toISOString() },
                host_metadata: fullHostMetadata,
                host_metadata_source: 'grpc',
            },
            audits: [],
        },
    });
    const { container, root } = await renderHost();

    // 标题已替换为"运行状态"，不再出现"性能状态概览"
    expect(container.textContent).toContain('运行状态');
    expect(container.textContent).not.toContain('性能状态概览');
    // 任务计数：1 个成功 / 0 个失败
    expect(container.textContent).toContain('1 / 0');
    // 数据新鲜度（host 采集时间刚刚 → 新鲜）
    expect(container.textContent).toContain('数据新鲜');

    act(() => root.unmount());
    container.remove();
});

test('采集器 CPU/内存只出现在诊断区域', async () => {
    await setupApiMocks();
    const { container, root } = await renderHost();

    // 默认不展开诊断：主指标区不出现"采集器进程 CPU"
    expect(container.textContent).not.toContain('采集器进程 CPU');
    expect(container.textContent).not.toContain('采集器进程内存');
    // 主指标区出现"当前是否可采集"与"Agent 版本"
    expect(container.textContent).toContain('当前是否可采集');
    expect(container.textContent).toContain('Agent 版本');

    // 展开诊断后出现
    const buttons = Array.from(container.querySelectorAll('button'));
    const diagButton = buttons.find(btn => btn.textContent.includes('展开采集器诊断'));
    expect(diagButton).toBeTruthy();
    act(() => { diagButton.click(); });
    expect(container.textContent).toContain('采集器进程 CPU');
    expect(container.textContent).toContain('采集器进程内存');
    expect(container.textContent).toContain('读取吞吐');

    act(() => root.unmount());
    container.remove();
});

test('采集能力显示中文名称和用途说明', async () => {
    await setupApiMocks();
    agents.detail.mockResolvedValue({
        code: 0,
        data: {
            agent: { online: true, version: '1.0.0', last_seen: '2026-08-25T10:00:00Z', capabilities: 'perf_cpu,ebpf_io' },
            stat: {
                source: 'grpc',
                cpu_percent: 1,
                memory_kb: 100,
                host: null,
                host_metadata: null,
                host_metadata_source: 'none',
            },
            audits: [],
        },
    });
    const { container, root } = await renderHost();

    // 展开采集器状态查看能力说明
    const buttons = Array.from(container.querySelectorAll('button'));
    const expandButton = buttons.find(btn => btn.textContent.includes('展开采集器状态'));
    expect(expandButton).toBeTruthy();
    act(() => { expandButton.click(); });

    expect(container.textContent).toContain('CPU 采样');
    expect(container.textContent).toContain('eBPF IO');
    expect(container.textContent).toContain('定位 CPU 热点');
    expect(container.textContent).toContain('IO 延迟分析');

    act(() => root.unmount());
    container.remove();
});
