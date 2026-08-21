import React from 'react';
import { createRoot } from 'react-dom/client';
import { act } from 'react-dom/test-utils';

jest.mock('./InteractiveFlamegraph', () => ({
    __esModule: true,
    default: () => null,
    countProfileNodes: () => 0,
}));

import {
    DiagnosticDetails,
    diagnosticText,
    formatDiagnosticJSON,
    makeSequentialDiffWindows,
    makeTimeWindow,
    coverageSegments,
    instanceFilters,
	processInstanceOptions,
    rangeOptionsForRetention,
    runtimeLabel,
    validateCustomTimeWindow,
} from './ContinuousProfilingPanel';

global.IS_REACT_ACT_ENVIRONMENT = true;

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

test('diff quick windows are adjacent, equal, and filtered by total retention', () => {
    const windows = makeSequentialDiffWindows('15m', new Date('2026-08-19T12:00:00Z'));
    expect(windows.baseWindow.to).toBe(windows.compareWindow.from);
    expect(new Date(windows.baseWindow.to) - new Date(windows.baseWindow.from))
        .toBe(new Date(windows.compareWindow.to) - new Date(windows.compareWindow.from));

    expect(rangeOptionsForRetention(24).map(([value]) => value)).toContain('24h');
    expect(rangeOptionsForRetention(24, true).map(([value]) => value)).toContain('12h');
    expect(rangeOptionsForRetention(24, true).map(([value]) => value)).not.toContain('24h');
});
