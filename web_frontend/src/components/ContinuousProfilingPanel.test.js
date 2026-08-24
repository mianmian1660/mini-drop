import React from 'react';
import { createRoot } from 'react-dom/client';
import { act, Simulate } from 'react-dom/test-utils';

jest.mock('../api', () => ({
    continuous: {
        timeline: jest.fn(),
        histogram: jest.fn(),
    },
    profiles: {
        flamegraph: jest.fn(),
        topn: jest.fn(),
        diff: jest.fn(),
        labelValues: jest.fn(),
        timeseries: jest.fn(),
        heapTasks: jest.fn(),
    },
    sentinelRules: {
        events: jest.fn(() => Promise.resolve({ code: 0, data: { events: [] } })),
    },
}));

jest.mock('./InteractiveFlamegraph', () => ({
    __esModule: true,
    default: () => null,
    countProfileNodes: () => 0,
}));

// HistogramTrendChart 内部用真实 d3 画 SVG 时间轴，和 TimelineChart.js 一样不在 jest 里跑真实 d3
// （见 TimelineChart.test.js / ScheduleDetailPage.test.js 的同款处理），这里只关心它被正确调用。
jest.mock('./HistogramTrendChart', () => ({
    __esModule: true,
    default: () => null,
}));

import {
    default as ContinuousProfilingPanel,
    CoverageBand,
    coverageAlertForReliability,
    DiagnosticDetails,
    TopNTable,
    HistogramPanel,
    customInputsToSlider,
    diagnosticText,
    formatDiagnosticJSON,
    makeSequentialDiffWindows,
    makeTimeWindow,
    coverageSegments,
    instanceFilters,
    processInstanceOptions,
    rangeOptionsForRetention,
    sampledProcessInstanceOptions,
    diffWindowsFromMinutes,
    normalizeEvenSpan,
    sequentialDiffWindowsFromStart,
    sliderMinutesToInputs,
    runtimeLabel,
    validateCustomTimeWindow,
    DBSnapshotPanel,
    signalTabsForSession,
} from './ContinuousProfilingPanel';
import { continuous, profiles } from '../api';

global.IS_REACT_ACT_ENVIRONMENT = true;

beforeEach(() => {
    jest.clearAllMocks();
});

test('diagnostic JSON uses two-space indentation for nested values', () => {
    expect(formatDiagnosticJSON({ outer: { enabled: true } })).toBe([
        '{',
        '  "outer": {',
        '    "enabled": true',
        '  }',
        '}',
    ].join('\n'));
});

test('diagnostic text preserves long values and tolerates missing or unusual values', () => {
    const longValue = 'x'.repeat(4096);
    const circular = { longValue, count: 12n };
    circular.self = circular;

    expect(() => formatDiagnosticJSON(undefined)).not.toThrow();
    expect(formatDiagnosticJSON({})).toBe('{}');
    expect(formatDiagnosticJSON(circular)).toContain(longValue);
    expect(formatDiagnosticJSON(circular)).toContain('"count": "12"');
    expect(formatDiagnosticJSON(circular)).toContain('"self": "[Circular]"');
    expect(() => diagnosticText()).not.toThrow();
});

test('diagnostic block exposes bounded wrapping styles and formatted fields', () => {
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    act(() => {
        root.render(<DiagnosticDetails
            target={{ id: 'target-1', continuous_session: { sampler: { name: 'perf' } } }}
            flamegraph={{ symbol_diagnostics: { unresolved: { count: 2 } } }}
            timeWindow={{ from: 'start', to: 'end' }}
            profileType="cpu"
        />);
    });

    const details = container.querySelector('.diagnostic-details');
    const output = container.querySelector('.diagnostic-output');
    expect(details).not.toBeNull();
    expect(output.style.maxWidth).toBe('100%');
    expect(output.style.whiteSpace).toBe('pre-wrap');
    expect(output.style.overflowWrap).toBe('anywhere');
    expect(output.textContent).toContain('continuous_session:\n{\n  "sampler": {');
    expect(output.textContent).toContain('symbol_diagnostics:\n{\n  "unresolved": {');

    act(() => root.unmount());
    container.remove();
});

test('TopN table labels cumulative and self columns with percentages', () => {
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    act(() => {
        root.render(<TopNTable data={{
            unit: 'samples',
            total: 4,
            items: [{ name: '0x57f4 [postgres]', display_name: '[未解析] postgres', value: 2, self: 1, percent: 50, self_percent: 25, unresolved: true, unit: 'samples' }],
        }} />);
    });

    expect(container.textContent).toContain('累计占比');
    expect(container.textContent).toContain('自身占比');
    expect(container.textContent).toContain('[未解析] postgres');
    expect(container.textContent).toContain('50.0%');
    expect(container.textContent).toContain('25.0%');

    act(() => root.unmount());
    container.remove();
});

test('TopN loading state does not render an empty no-sample message first', () => {
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    act(() => {
        root.render(<TopNTable loading />);
    });

    expect(container.textContent).toContain('正在查询 TopN');
    expect(container.textContent).not.toContain('暂无热点函数');

    act(() => root.unmount());
    container.remove();
});

test('Histogram table renders visible distribution bars and Chinese event counts', () => {
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    act(() => {
        root.render(<HistogramPanel title="块 IO 延迟" data={{
            source: 'mini-drop-native',
            backend: 'ebpf',
            unit: 'us',
            event_count: 897,
            summary: { p50: 512, p95: 2048, p99: 4096 },
            buckets: [
                { range: '[256, 512)', count: 162 },
                { range: '[512, 1024)', count: 465 },
            ],
            trend: [{ window_start: '2026-08-21T12:00:00Z', p50: 512, p95: 2048, p99: 4096, event_count: 162 }],
        }} />);
    });

    expect(container.textContent).toContain('延迟桶');
    expect(container.textContent).toContain('分布');
    expect(container.textContent).toContain('事件数');
    expect(container.textContent).toContain('总事件 897 个');
    expect(container.textContent).toContain('897 个');
    expect(container.textContent).toContain('162 个');
    expect(container.textContent).not.toContain('samples events');
    const bars = container.querySelectorAll('[data-testid="histogram-bar"]');
    expect(bars.length).toBe(2);
    expect(parseFloat(bars[0].style.width)).toBeGreaterThan(0);
    expect(bars[1].style.width).toBe('100%');

    act(() => root.unmount());
    container.remove();
});

test('process instance filters keep PID reuse identities separate', () => {
    expect(instanceFilters('42|1724160000123')).toEqual({ pid: '42', process_start_ms: '1724160000123' });
    expect(instanceFilters('42')).toEqual({});
});

test('process instance options retain historical PID identities after restart', () => {
	const options = processInstanceOptions(
		[{ pid: 42, process_start_ms: 1724160000222 }],
		['42|1724160000111', '42|1724160000222', '77|1724160000333', 'invalid'],
	);
	expect(options).toEqual([
		{ value: '42|1724160000222', pid: 42, processStartMs: 1724160000222, active: true },
		{ value: '77|1724160000333', pid: 77, processStartMs: 1724160000333, active: false },
		{ value: '42|1724160000111', pid: 42, processStartMs: 1724160000111, active: false },
	]);
});

test('sampled process instance options only expose instances with samples', () => {
    const options = sampledProcessInstanceOptions(
        [{ pid: 42, process_start_ms: 1724160000222 }, { pid: 99, process_start_ms: 1724160000999 }],
        ['42|1724160000222', '77|1724160000333'],
    );
    expect(options).toEqual([
        { value: '42|1724160000222', pid: 42, processStartMs: 1724160000222, active: true },
        { value: '77|1724160000333', pid: 77, processStartMs: 1724160000333, active: false },
    ]);
});

function toLocalInput(value) {
    const date = new Date(value);
    const local = new Date(date.getTime() - date.getTimezoneOffset() * 60 * 1000);
    return local.toISOString().slice(0, 16);
}

test('quick ranges are recalculated from the supplied current time', () => {
    const first = makeTimeWindow('30m', new Date('2026-08-19T10:00:00Z'));
    const second = makeTimeWindow('30m', new Date('2026-08-19T10:05:00Z'));
    expect(first).toEqual({ from: '2026-08-19T09:30:00.000Z', to: '2026-08-19T10:00:00.000Z' });
    expect(second.to).toBe('2026-08-19T10:05:00.000Z');
});

test('coverage segments preserve covered and gap proportions', () => {
    const coverage = { from: '2026-08-19T10:00:00Z', to: '2026-08-19T10:10:00Z' };
    const segments = coverageSegments(coverage, [{
        start: '2026-08-19T10:04:00Z', end: '2026-08-19T10:06:00Z',
    }]);
    expect(segments.map(item => item.type)).toEqual(['covered', 'gap', 'covered']);
    expect(segments.map(item => Math.round(item.percent))).toEqual([40, 20, 40]);
});

test('coverage segments clip gaps to the selected range', () => {
    const coverage = { from: '2026-08-19T10:00:00Z', to: '2026-08-19T10:10:00Z' };
    const segments = coverageSegments(coverage, [{
        start: '2026-08-19T09:59:00Z', end: '2026-08-19T10:01:00Z',
    }]);
    expect(segments[0].type).toBe('gap');
    expect(Math.round(segments[0].percent)).toBe(10);
});

test('runtime labels cover supported languages', () => {
    expect(runtimeLabel('python')).toBe('Python');
    expect(runtimeLabel('java')).toBe('Java/JVM');
    expect(runtimeLabel('native')).toBe('Native');
});

test('custom ranges enforce future and retention boundaries', () => {
    const now = new Date('2026-08-19T12:00:00Z');
    const valid = validateCustomTimeWindow(
        toLocalInput('2026-08-19T11:00:00Z'),
        toLocalInput('2026-08-19T12:00:00Z'),
        24,
        '',
        now,
    );
    expect(valid.error).toBe('');
    expect(valid.window).toEqual({ from: '2026-08-19T11:00:00.000Z', to: '2026-08-19T12:00:00.000Z' });

    const future = validateCustomTimeWindow(
        toLocalInput('2026-08-19T11:00:00Z'),
        toLocalInput('2026-08-19T12:01:00Z'),
        24,
        '',
        now,
    );
    expect(future.error).toContain('不能晚于当前时间');

    const expired = validateCustomTimeWindow(
        toLocalInput('2026-08-18T11:00:00Z'),
        toLocalInput('2026-08-18T12:00:00Z'),
        24,
        '',
        now,
    );
    expect(expired.error).toContain('数据保留边界');
});

test('slider minute helpers preserve selected windows inside retention', () => {
    const now = new Date('2026-08-19T12:00:00Z');
    const inputs = sliderMinutesToInputs(60, 120, 24, now);
    const slider = customInputsToSlider(inputs.fromInput, inputs.toInput, 24, now);
    expect(slider.fromMinute).toBe(60);
    expect(slider.toMinute).toBe(120);
});

test('slider helpers use the supplied anchor without drifting', () => {
    const now = new Date('2026-08-19T12:00:00Z');
    const first = sliderMinutesToInputs(60, 120, 24, now);
    const second = sliderMinutesToInputs(60, 120, 24, now);
    expect(first).toEqual(second);
});

test('diff quick windows are adjacent, equal, and filtered by total retention', () => {
    const windows = makeSequentialDiffWindows('15m', new Date('2026-08-19T12:00:00Z'));
    expect(windows.baseWindow.to).toBe(windows.compareWindow.from);
    expect(new Date(windows.baseWindow.to) - new Date(windows.baseWindow.from))
        .toBe(new Date(windows.compareWindow.to) - new Date(windows.compareWindow.from));

    expect(rangeOptionsForRetention(24).map(([value]) => value)).toContain('24h');
    expect(rangeOptionsForRetention(24, true).map(([value]) => value)).toContain('12h');
    expect(rangeOptionsForRetention(24, true).map(([value]) => value)).not.toContain('24h');
});

test('diff slider windows remain adjacent and equal length', () => {
    const bounds = { from: new Date('2026-08-19T10:00:00Z'), to: new Date('2026-08-19T12:00:00Z') };
    const windows = sequentialDiffWindowsFromStart(30, 15, bounds);
    expect(windows.baseWindow.to).toBe(windows.compareWindow.from);
    expect(new Date(windows.baseWindow.to) - new Date(windows.baseWindow.from)).toBe(15 * 60 * 1000);
    expect(new Date(windows.compareWindow.to) - new Date(windows.compareWindow.from)).toBe(15 * 60 * 1000);
});

test('diff window range normalizes to adjacent equal halves', () => {
    const bounds = { from: new Date('2026-08-19T10:00:00Z'), to: new Date('2026-08-19T12:00:00Z') };
    const normalized = normalizeEvenSpan(11, 46, 120);
    expect((normalized.toMinute - normalized.fromMinute) % 2).toBe(0);
    const windows = diffWindowsFromMinutes(11, 46, bounds);
    expect(windows.baseWindow.to).toBe(windows.compareWindow.from);
    expect(new Date(windows.baseWindow.to) - new Date(windows.baseWindow.from))
        .toBe(new Date(windows.compareWindow.to) - new Date(windows.compareWindow.from));
});

test('coverage bar hover reveals segment details', () => {
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    act(() => {
        root.render(<CoverageBand reliability={{
            coverage: { from: '2026-08-19T10:00:00Z', to: '2026-08-19T10:10:00Z', ratio: 0.8 },
            gaps: [{ start: '2026-08-19T10:04:00Z', end: '2026-08-19T10:06:00Z' }],
        }} />);
    });
    const gap = container.querySelector('[data-testid="coverage-gap"]');
    expect(gap).not.toBeNull();
    act(() => Simulate.mouseEnter(gap));
    act(() => Simulate.mouseMove(gap));
    expect(container.textContent).toContain('采集缺口');
    expect(container.textContent).toContain('该时段无样本或存在上传空档');
    act(() => root.unmount());
    container.remove();
});

test('coverage alert summarizes large gaps clearly', () => {
    const alert = coverageAlertForReliability({
        coverage: { from: '2026-08-19T10:00:00Z', to: '2026-08-19T10:10:00Z', ratio: 0.8 },
        gaps: [{ start: '2026-08-19T10:04:00Z', end: '2026-08-19T10:06:00Z', duration_seconds: 120 }],
    });
    expect(alert.summary).toContain('覆盖 80.0%');
    expect(alert.summary).toContain('最长 2.0 分钟');
    expect(alert.detail).toContain('累计缺口');
});

test('stable target fields prevent parent polling from re-querying profiles', async () => {
    profiles.flamegraph.mockResolvedValue({ code: 0, data: { nodes: [], empty: true } });
    profiles.topn.mockResolvedValue({ code: 0, data: { items: [], empty: true } });
    profiles.labelValues.mockResolvedValue({ code: 0, data: { values: [], available: false } });
    continuous.timeline.mockResolvedValue({ code: 0, data: null });

    const session = {
        sid: 'cps-api',
        name: 'API',
        scope: 'host',
        observed_state: 'running',
        desired_state: 'running',
        retention_hours: 24,
    };
    const target = { id: 'target-1', ip: '10.0.0.8', hostname: 'node', service_name: 'hotmethod' };
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<ContinuousProfilingPanel target={target} fixedSession={session} />);
    });
    await act(async () => { await Promise.resolve(); });
    expect(profiles.flamegraph).toHaveBeenCalledTimes(1);

    await act(async () => {
        root.render(<ContinuousProfilingPanel target={{ ...target }} fixedSession={{ ...session, last_upload_at: '2026-08-19T12:00:00Z' }} />);
    });
    await act(async () => { await Promise.resolve(); });
    expect(profiles.flamegraph).toHaveBeenCalledTimes(1);

    act(() => root.unmount());
    container.remove();
});

test('session signal tabs only show signals configured on the session', async () => {
    profiles.flamegraph.mockResolvedValue({ code: 0, data: { nodes: [], empty: true } });
    profiles.topn.mockResolvedValue({ code: 0, data: { items: [], empty: true } });
    profiles.labelValues.mockResolvedValue({ code: 0, data: { values: [], available: false } });
    continuous.timeline.mockResolvedValue({ code: 0, data: null });

    const session = {
        sid: 'cps-cpu',
        name: 'CPU only',
        scope: 'host',
        observed_state: 'running',
        desired_state: 'running',
        signals: ['cpu_profile'],
    };
    const target = { id: 'target-1', ip: '10.0.0.8', hostname: 'node', service_name: 'hotmethod' };
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<ContinuousProfilingPanel target={target} fixedSession={session} />);
    });
    await act(async () => { await Promise.resolve(); });

    expect(container.textContent).toContain('CPU');
    expect(container.textContent).not.toContain('块 IO');
    expect(container.textContent).not.toContain('系统调用 IO');
    expect(container.textContent).not.toContain('调度延迟');

    act(() => root.unmount());
    container.remove();
});

// ============================================================
// 数据库快照面板（3a6230f 专项）
// ============================================================

test('数据库快照面板：加载态显示正在查询', () => {
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    act(() => {
        root.render(<DBSnapshotPanel data={null} loading />);
    });
    expect(container.textContent).toContain('正在查询数据库快照');
    act(() => root.unmount());
    container.remove();
});

test('数据库快照面板：空态显示暂无数据', () => {
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    act(() => {
        root.render(<DBSnapshotPanel data={{ empty: true, message: '该时间范围暂无数据库快照数据' }} loading={false} />);
    });
    expect(container.textContent).toContain('该时间范围暂无数据库快照数据');
    act(() => root.unmount());
    container.remove();
});

test('数据库快照面板：digest 表渲染各列', () => {
    const data = {
        digests: [{
            instance_label: 'mysql-a',
            schema_name: 'mydb',
            digest_text: 'SELECT * FROM t WHERE id = ?',
            call_count: 42,
            total_latency_us: 1000000,
            avg_latency_us: 23809,
            rows_examined: 100,
        }],
        lock_waits: [],
    };
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    act(() => {
        root.render(<DBSnapshotPanel data={data} loading={false} />);
    });
    expect(container.textContent).toContain('慢查询 digest');
    expect(container.textContent).toContain('SELECT * FROM t WHERE id = ?');
    expect(container.textContent).toContain('mysql-a');
    expect(container.textContent).toContain('mydb');
    expect(container.textContent).toContain('调用次数');
    expect(container.textContent).toContain('累计耗时');
    expect(container.textContent).toContain('平均耗时');
    expect(container.textContent).toContain('扫描行数');
    act(() => root.unmount());
    container.remove();
});

test('数据库快照面板：锁等待表渲染阻塞关系', () => {
    const data = {
        digests: [],
        lock_waits: [{
            instance_label: 'mysql-a',
            timestamp: '2026-08-22T00:00:00Z',
            waiting_pid: 1001,
            waiting_query: 'UPDATE t SET x=1',
            blocking_pid: 1002,
            blocking_query: 'SELECT * FROM t FOR UPDATE',
            wait_seconds: 12,
            locked_table: 'db.t',
        }],
    };
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    act(() => {
        root.render(<DBSnapshotPanel data={data} loading={false} />);
    });
    expect(container.textContent).toContain('锁等待链');
    expect(container.textContent).toContain('最长锁等待');
    expect(container.textContent).toContain('1001');
    expect(container.textContent).toContain('1002');
    expect(container.textContent).toContain('12 s');
    expect(container.textContent).toContain('db.t');
    act(() => root.unmount());
    container.remove();
});

test('数据库快照面板：空 digest/锁等待 显示对应空态文案', () => {
    const data = { digests: [], lock_waits: [], source: 'mini-drop-native' };
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    act(() => {
        root.render(<DBSnapshotPanel data={data} loading={false} />);
    });
    // 无 digests 且无 lock_waits -> 空态
    expect(container.textContent).toContain('该时间范围暂无数据库快照数据');
    act(() => root.unmount());
    container.remove();
});

test('signalTabsForSession 始终包含数据库 tab', () => {
    const tabs = signalTabsForSession('cpu_profile|io_latency');
    const dbTab = tabs.find(t => t.tab === 'db');
    expect(dbTab).toBeTruthy();
    expect(dbTab.label).toBe('数据库');
    // 纯 db_snapshot 信号也至少给出数据库 tab
    const dbOnly = signalTabsForSession('db_snapshot');
    expect(dbOnly.some(t => t.tab === 'db')).toBe(true);
});
