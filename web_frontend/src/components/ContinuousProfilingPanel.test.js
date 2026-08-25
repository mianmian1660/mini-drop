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
        memoryProfiles: jest.fn(),
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
    coverageBandsFromReliability,
    coverageStatusColor,
    coverageStatusText,
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
    storageSourceLabel,
    resolutionLabel,
} from './ContinuousProfilingPanel';
import { continuous, profiles, sentinelRules } from '../api';

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
    const gap = container.querySelector('[data-testid="coverage-real_gap"]');
    expect(gap).not.toBeNull();
    act(() => Simulate.mouseEnter(gap));
    act(() => Simulate.mouseMove(gap));
    expect(container.textContent).toContain('确认缺少数据');
    expect(container.textContent).toContain('这段时间已经超过等待时间');
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

test('coverage alert uses exact gap seconds from the signal coverage', () => {
    const alert = coverageAlertForReliability({
        coverage: { from: '2026-08-19T10:00:00Z', to: '2026-08-19T10:10:00Z', ratio: 0.5 },
        signal_coverage: {
            cpu_profile: {
                coverage: { from: '2026-08-19T10:00:00Z', to: '2026-08-19T10:10:00Z', ratio: 0.5, gap_seconds: 300 },
                gaps: [{ start: '2026-08-19T10:04:00Z', end: '2026-08-19T10:06:00Z', duration_seconds: 120 }],
                gap_count_total: 1,
                status: 'real_gap',
                coverage_bands: [],
            },
        },
    }, 'cpu_profile');
    expect(alert.summary).toContain('覆盖 50.0%');
    // 累计缺口用精确 gap_seconds（300s = 5 分钟），而不是截断后的 120s。
    expect(alert.detail).toContain('累计缺口 5.0 分钟');
});

test('coverage bands prefer the current signal coverage', () => {
    const reliability = {
        coverage: { from: '2026-08-19T10:00:00Z', to: '2026-08-19T10:10:00Z', ratio: 0.5 },
        gaps: [{ start: '2026-08-19T10:04:00Z', end: '2026-08-19T10:06:00Z' }],
        signal_coverage: {
            cpu_profile: {
                coverage: { from: '2026-08-19T10:00:00Z', to: '2026-08-19T10:10:00Z', ratio: 1.0 },
                gaps: [],
                gap_count_total: 0,
                status: 'healthy',
                coverage_bands: [
                    { start: '2026-08-19T10:00:00Z', end: '2026-08-19T10:10:00Z', status: 'healthy', duration_seconds: 600, sample_count: 100 },
                ],
                status_summary: { status: 'healthy', label: '数据正常', explanation: '这段时间已经收到有效采集数据', suggestion: '' },
            },
            io_latency: {
                coverage: { from: '2026-08-19T10:00:00Z', to: '2026-08-19T10:10:00Z', ratio: 0 },
                gaps: [{ start: '2026-08-19T10:00:00Z', end: '2026-08-19T10:10:00Z', duration_seconds: 600 }],
                gap_count_total: 1,
                status: 'real_gap',
                coverage_bands: [
                    { start: '2026-08-19T10:00:00Z', end: '2026-08-19T10:10:00Z', status: 'real_gap', duration_seconds: 600, sample_count: 0 },
                ],
                status_summary: { status: 'real_gap', label: '确认缺少数据', explanation: '这段时间已经超过等待时间，仍没有收到数据', suggestion: '请检查 Agent 状态与网络上传' },
            },
        },
    };
    const cpu = coverageBandsFromReliability(reliability, 'cpu_profile');
    expect(cpu.ratio).toBe(1.0);
    expect(cpu.bands[0].status).toBe('healthy');
    expect(cpu.gapCountTotal).toBe(0);
    const io = coverageBandsFromReliability(reliability, 'io_latency');
    expect(io.ratio).toBe(0);
    expect(io.bands[0].status).toBe('real_gap');
    expect(io.gapCountTotal).toBe(1);
});

test('coverage status colors map red/yellow/gray/green correctly', () => {
    expect(coverageStatusColor('healthy')).toBe('#12b76a');
    expect(coverageStatusColor('real_gap')).toBe('#d92d20');
    expect(coverageStatusColor('collector_failed')).toBe('#b42318');
    expect(coverageStatusColor('pending_upload')).toBe('#f79009');
    expect(coverageStatusColor('target_idle')).toBe('#98a2b3');
    expect(coverageStatusColor('startup_grace')).toBe('#98a2b3');
    expect(coverageStatusColor('shutdown_grace')).toBe('#98a2b3');
    expect(coverageStatusColor('unknown')).toBe('#98a2b3');
});

test('coverage status text provides Chinese labels and explanations', () => {
    expect(coverageStatusText('healthy').label).toBe('数据正常');
    expect(coverageStatusText('real_gap').label).toBe('确认缺少数据');
    expect(coverageStatusText('pending_upload').label).toBe('数据整理中');
    expect(coverageStatusText('target_idle').label).toBe('目标暂时空闲');
    expect(coverageStatusText('startup_grace').label).toBe('正在启动采集');
    expect(coverageStatusText('shutdown_grace').label).toBe('停止收尾中');
    expect(coverageStatusText('collector_failed').label).toBe('采集异常');
    expect(coverageStatusText('unknown').label).toBe('状态未知');
    expect(coverageStatusText('real_gap').explanation).toContain('超过等待时间');
});

test('coverage band renders gray for idle and yellow for pending without red gaps', () => {
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    act(() => {
        root.render(<CoverageBand reliability={{
            coverage: { from: '2026-08-19T10:00:00Z', to: '2026-08-19T10:10:00Z', ratio: 1.0 },
            status_summary: { status: 'mixed', label: '部分异常', explanation: '部分时段缺少数据或状态异常', suggestion: '请查看下方色带定位具体时段' },
            signal_coverage: {
                cpu_profile: {
                    coverage: { from: '2026-08-19T10:00:00Z', to: '2026-08-19T10:10:00Z', ratio: 1.0 },
                    gaps: [],
                    gap_count_total: 0,
                    status: 'pending_upload',
                    coverage_bands: [
                        { start: '2026-08-19T10:00:00Z', end: '2026-08-19T10:02:00Z', status: 'startup_grace', duration_seconds: 120, sample_count: 0 },
                        { start: '2026-08-19T10:02:00Z', end: '2026-08-19T10:08:00Z', status: 'target_idle', duration_seconds: 360, sample_count: 0 },
                        { start: '2026-08-19T10:08:00Z', end: '2026-08-19T10:10:00Z', status: 'pending_upload', duration_seconds: 120, sample_count: 0 },
                    ],
                    status_summary: { status: 'pending_upload', label: '数据整理中', explanation: '采集器已经工作，数据还在上传或整理', suggestion: '稍后刷新；如果持续超过上传周期，请检查 Agent 状态' },
                },
            },
        }} signal="cpu_profile" />);
    });
    expect(container.querySelector('[data-testid="coverage-startup_grace"]')).not.toBeNull();
    expect(container.querySelector('[data-testid="coverage-target_idle"]')).not.toBeNull();
    expect(container.querySelector('[data-testid="coverage-pending_upload"]')).not.toBeNull();
    expect(container.querySelector('[data-testid="coverage-real_gap"]')).toBeNull();
    expect(container.textContent).toContain('数据整理中');
    expect(container.textContent).toContain('CPU');
    // Session 总体状态概览独立展示。
    expect(container.textContent).toContain('Session 总体');
    expect(container.textContent).toContain('部分异常');
    act(() => root.unmount());
    container.remove();
});

test('coverage band falls back to legacy coverage/gaps when no bands exist', () => {
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    act(() => {
        root.render(<CoverageBand reliability={{
            coverage: { from: '2026-08-19T10:00:00Z', to: '2026-08-19T10:10:00Z', ratio: 0.8 },
            gaps: [{ start: '2026-08-19T10:04:00Z', end: '2026-08-19T10:06:00Z', duration_seconds: 120 }],
        }} />);
    });
    expect(container.querySelector('[data-testid="coverage-real_gap"]')).not.toBeNull();
    expect(container.querySelector('[data-testid="coverage-healthy"]')).not.toBeNull();
    expect(container.textContent).toContain('80.0%');
    act(() => root.unmount());
    container.remove();
});

test('coverage band renders server bands without generating extra DOM', () => {
    const bands = Array.from({ length: 600 }, (_, i) => ({
        start: new Date(2026, 7, 19, 10, 0, 0, i * 1000).toISOString(),
        end: new Date(2026, 7, 19, 10, 0, 0, (i + 1) * 1000).toISOString(),
        status: i % 2 === 0 ? 'healthy' : 'real_gap',
        duration_seconds: 1,
        sample_count: i % 2 === 0 ? 10 : 0,
    }));
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    act(() => {
        root.render(<CoverageBand reliability={{
            coverage: { from: '2026-08-19T10:00:00Z', to: '2026-08-19T10:10:00Z', ratio: 0.5 },
            coverage_bands: bands,
            gaps: [],
        }} />);
    });
    expect(container.querySelectorAll('[data-testid^="coverage-"]').length).toBe(600);
    act(() => root.unmount());
    container.remove();
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
// 阶段七：Memory 视图（Memray profiles + RSS 诊断）
// ============================================================

test('Memory 视图：Memray profile 列表展示状态/时间窗口/进程身份', async () => {
    profiles.flamegraph.mockResolvedValue({ code: 0, data: { nodes: [], empty: true } });
    profiles.topn.mockResolvedValue({ code: 0, data: { items: [], empty: true } });
    profiles.labelValues.mockResolvedValue({ code: 0, data: { values: [], available: false } });
    profiles.timeseries.mockResolvedValue({ code: 0, data: { series: [], empty: true, diagnostics: ['没有 python_rss 采集窗口'] } });
    profiles.memoryProfiles.mockResolvedValue({ code: 0, data: { profiles: [
        { profile_id: 'memray-1-100', window_start: '2026-08-25T10:00:00Z', window_end: '2026-08-25T10:01:00Z', pid: 1001, process_start_ms: 1724160000123, comm: 'python3', exe: '/usr/bin/python3', sample_count: 4096, status: 'ready' },
        { profile_id: 'memray-1-100', window_start: '2026-08-25T10:01:00Z', window_end: '2026-08-25T10:02:00Z', pid: 1001, process_start_ms: 1724160000123, comm: 'python3', exe: '/usr/bin/python3', sample_count: 4096, status: 'duplicate' },
        { profile_id: 'memray-1-200', window_start: '2026-08-25T10:00:00Z', window_end: '2026-08-25T10:01:00Z', pid: 0, comm: '', exe: '', sample_count: 0, status: 'failed', reason: 'profile 无可用样本' },
    ], total: 3, empty: false } });
    profiles.heapTasks.mockResolvedValue({ code: 0, data: { tasks: [] } });
    continuous.timeline.mockResolvedValue({ code: 0, data: null });

    const session = {
        sid: 'cps-mem',
        name: 'Memory',
        scope: 'host',
        observed_state: 'running',
        desired_state: 'running',
        signals: ['cpu_profile', 'python_rss', 'python_memory'],
    };
    const target = { id: 'target-1', ip: '10.0.0.8', hostname: 'node', service_name: 'hotmethod' };
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<ContinuousProfilingPanel target={target} fixedSession={session} />);
    });
    await act(async () => { await Promise.resolve(); });

    // 切到 Memory 类型
    const select = container.querySelector('select');
    const selects = container.querySelectorAll('select');
    const profileTypeSelect = Array.from(selects).find(el => Array.from(el.options).some(opt => opt.value === 'memory'));
    expect(profileTypeSelect).not.toBeNull();
    await act(async () => {
        Simulate.change(profileTypeSelect, { target: { value: 'memory' } });
    });
    await act(async () => { await Promise.resolve(); });
    await act(async () => { await Promise.resolve(); });

    expect(profiles.memoryProfiles).toHaveBeenCalled();
    expect(container.textContent).toContain('Memray Allocation Profiles');
    expect(container.textContent).toContain('memray-1-100');
    expect(container.textContent).toContain('ready');
    expect(container.textContent).toContain('duplicate');
    expect(container.textContent).toContain('failed');
    expect(container.textContent).toContain('profile 无可用样本');
    // RSS 空态诊断
    expect(container.textContent).toContain('没有 python_rss 采集窗口');

    act(() => root.unmount());
    container.remove();
});

test('Memory 视图：RSS 趋势展示截断诊断', async () => {
    profiles.flamegraph.mockResolvedValue({ code: 0, data: { nodes: [], empty: true } });
    profiles.topn.mockResolvedValue({ code: 0, data: { items: [], empty: true } });
    profiles.labelValues.mockResolvedValue({ code: 0, data: { values: [], available: false } });
    profiles.timeseries.mockResolvedValue({ code: 0, data: {
        series: [{ pid: 123, process_start_ms: 1000, comm: 'python3', exe: '/usr/bin/python3', peak: 1048576, points: [{ timestamp: '2026-08-25T10:00:00Z', value: 1048576 }] }],
        empty: false, rss_truncated: 3, process_count: 1,
        diagnostics: ['Agent 侧进程数超过上限，RSS 采样被截断（最多 3 个进程）'],
    } });
    profiles.memoryProfiles.mockResolvedValue({ code: 0, data: { profiles: [], total: 0, empty: true } });
    profiles.heapTasks.mockResolvedValue({ code: 0, data: { tasks: [] } });
    continuous.timeline.mockResolvedValue({ code: 0, data: null });

    const session = {
        sid: 'cps-mem2',
        name: 'Memory',
        scope: 'host',
        observed_state: 'running',
        desired_state: 'running',
        signals: ['cpu_profile', 'python_rss', 'python_memory'],
    };
    const target = { id: 'target-1', ip: '10.0.0.8', hostname: 'node', service_name: 'hotmethod' };
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<ContinuousProfilingPanel target={target} fixedSession={session} />);
    });
    await act(async () => { await Promise.resolve(); });

    const selects = container.querySelectorAll('select');
    const profileTypeSelect = Array.from(selects).find(el => Array.from(el.options).some(opt => opt.value === 'memory'));
    await act(async () => {
        Simulate.change(profileTypeSelect, { target: { value: 'memory' } });
    });
    await act(async () => { await Promise.resolve(); });
    await act(async () => { await Promise.resolve(); });

    expect(container.textContent).toContain('Python RSS 趋势');
    expect(container.textContent).toContain('已截断 3');
    expect(container.textContent).toContain('RSS 采样被截断');

    act(() => root.unmount());
    container.remove();
});

// ============================================================
// 阶段八：存储来源 / 分辨率展示（前端统一使用 API 字段）
// ============================================================

test('storageSourceLabel / resolutionLabel 映射 API 字段', () => {
    expect(storageSourceLabel('parquet_v2')).toBe('Parquet v2');
    expect(storageSourceLabel('parquet_v1')).toBe('Parquet v1（热窗口）');
    expect(storageSourceLabel(undefined)).toBe('Parquet v1（热窗口）');
    expect(resolutionLabel(60)).toBe('1m');
    expect(resolutionLabel(300)).toBe('5m');
    expect(resolutionLabel(3600)).toBe('1h');
    expect(resolutionLabel(0)).toBe('-');
});

test('采集元信息展示 storage_source / resolution / mixed_resolution', async () => {
    profiles.flamegraph.mockResolvedValue({ code: 0, data: {
        nodes: [{ name: 'main', value: 10, self: 10 }], empty: false, total: 10,
        storage_source: 'parquet_v2', resolution_seconds: 300, mixed_resolution: true,
    } });
    profiles.topn.mockResolvedValue({ code: 0, data: { items: [], empty: true } });
    profiles.labelValues.mockResolvedValue({ code: 0, data: { values: [], available: false } });
    continuous.timeline.mockResolvedValue({ code: 0, data: null });

    const session = {
        sid: 'cps-store',
        name: 'Store',
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
    await act(async () => { await Promise.resolve(); });

    expect(container.textContent).toContain('存储来源');
    expect(container.textContent).toContain('Parquet v2');
    expect(container.textContent).toContain('分辨率');
    expect(container.textContent).toContain('5m');
    expect(container.textContent).toContain('混合分辨率');
    expect(container.textContent).toContain('是');

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

test('数据库快照面板：哨兵触发列表只展示文案，不做可点击链接', async () => {
    sentinelRules.events.mockResolvedValueOnce({
        code: 0,
        data: {
            events: [
                { rule_sid: 'sr-1', metric: 'lock_wait', status: 'fired_no_action', observed_value: 8, floor_value: 3, evaluated_at: '2026-08-24T13:05:00Z' },
                { rule_sid: 'sr-2', metric: 'digest', status: 'fired_no_action', observed_value: 600000, floor_value: 50000, evaluated_at: '2026-08-24T14:32:00Z' },
                { rule_sid: 'sr-1', metric: 'lock_wait', status: 'skipped_cooldown', observed_value: 4, floor_value: 3, evaluated_at: '2026-08-24T13:10:00Z' },
            ],
        },
    });
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<DBSnapshotPanel data={{ empty: true }} loading={false} targetIP="10.0.0.9" timeWindow={{ from: '2026-08-24T12:00:00Z', to: '2026-08-24T15:00:00Z' }} />);
    });
    await act(async () => { await Promise.resolve(); });

    expect(sentinelRules.events).toHaveBeenCalledWith({ target_ip: '10.0.0.9', signal: 'db_snapshot', from: '2026-08-24T12:00:00Z', to: '2026-08-24T15:00:00Z' });
    // 只展示 fired_no_action，skipped_cooldown 不应出现
    expect(container.textContent).toContain('近期哨兵触发');
    expect(container.textContent).toContain('锁等待');
    expect(container.textContent).toContain('慢SQL环比');
    expect(container.textContent).toContain('等待 8 s（阈值 3 s）');
    expect((container.textContent.match(/已记录异常，当前无自动诊断/g) || []).length).toBe(2);
    // 不能做成可点击链接（child_tid 永远为空，点进去会是不存在的任务）
    expect(container.querySelectorAll('a, button[onclick]').length).toBe(0);

    act(() => root.unmount());
    container.remove();
});

test('数据库快照面板：没有哨兵触发记录时不展示该区块', async () => {
    sentinelRules.events.mockResolvedValueOnce({ code: 0, data: { events: [] } });
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<DBSnapshotPanel data={{ empty: true }} loading={false} targetIP="10.0.0.9" timeWindow={{ from: '2026-08-24T12:00:00Z', to: '2026-08-24T15:00:00Z' }} />);
    });
    await act(async () => { await Promise.resolve(); });

    expect(container.textContent).not.toContain('近期哨兵触发');

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
