import {
    clampPercent,
    formatCapacity,
    usageColor,
    formatCollectedAt,
    hostMetricAvailable,
} from './hostMetrics';

describe('clampPercent', () => {
    test('passes through in-range values', () => {
        expect(clampPercent(0)).toBe(0);
        expect(clampPercent(50)).toBe(50);
        expect(clampPercent(100)).toBe(100);
        expect(clampPercent(72.8)).toBe(72.8);
    });

    test('clamps out-of-range to 0-100', () => {
        expect(clampPercent(-5)).toBe(0);
        expect(clampPercent(120)).toBe(100);
        expect(clampPercent(150.5)).toBe(100);
    });

    test('handles missing/invalid input defensively', () => {
        expect(clampPercent(undefined)).toBe(0);
        expect(clampPercent(null)).toBe(0);
        expect(clampPercent(NaN)).toBe(0);
        expect(clampPercent('abc')).toBe(0);
    });
});

describe('formatCapacity', () => {
    test('formats B / KiB / MiB / GiB / TiB', () => {
        expect(formatCapacity(0)).toBe('0 B');
        expect(formatCapacity(500)).toBe('500 B');
        expect(formatCapacity(1024)).toBe('1.0 KiB');
        expect(formatCapacity(5 * 1024 * 1024)).toBe('5.0 MiB');
        expect(formatCapacity(1400000000)).toBe('1.3 GiB');
        expect(formatCapacity(30700000000)).toBe('28.6 GiB');
        expect(formatCapacity(4 * 1024 * 1024 * 1024 * 1024)).toBe('4.0 TiB');
    });

    test('returns -- for invalid or negative', () => {
        expect(formatCapacity(undefined)).toBe('--');
        expect(formatCapacity(null)).toBe('--');
        expect(formatCapacity(NaN)).toBe('--');
        expect(formatCapacity(-1)).toBe('--');
    });
});

describe('usageColor', () => {
    test('uses brand blue below 70%', () => {
        expect(usageColor(0)).toBe('#315efb');
        expect(usageColor(69)).toBe('#315efb');
        expect(usageColor(69.9)).toBe('#315efb');
    });

    test('uses warning orange at 70-89%', () => {
        expect(usageColor(70)).toBe('#f59e0b');
        expect(usageColor(72.8)).toBe('#f59e0b');
        expect(usageColor(89)).toBe('#f59e0b');
        expect(usageColor(89.9)).toBe('#f59e0b');
    });

    test('uses danger red at 90% and above', () => {
        expect(usageColor(90)).toBe('#dc2626');
        expect(usageColor(95)).toBe('#dc2626');
        expect(usageColor(100)).toBe('#dc2626');
    });

    test('clamps invalid input to 0 (blue)', () => {
        expect(usageColor(undefined)).toBe('#315efb');
        expect(usageColor(-10)).toBe('#315efb');
        expect(usageColor(250)).toBe('#dc2626');
    });
});

describe('formatCollectedAt', () => {
    test('formats RFC3339 timestamp', () => {
        const formatted = formatCollectedAt('2026-08-22T01:02:03Z');
        expect(formatted).not.toBe('--');
        expect(formatted).toContain(':');
    });

    test('returns -- for empty or invalid', () => {
        expect(formatCollectedAt('')).toBe('--');
        expect(formatCollectedAt(undefined)).toBe('--');
        expect(formatCollectedAt('not-a-date')).toBe('--');
    });
});

describe('hostMetricAvailable', () => {
    test('respects per-dimension availability flag', () => {
        const host = { cpu_available: true, memory_available: false };
        expect(hostMetricAvailable(host, 'cpu')).toBe(true);
        expect(hostMetricAvailable(host, 'memory')).toBe(false);
        expect(hostMetricAvailable(host, 'disk')).toBe(false);
    });

    test('missing host or flag means unavailable', () => {
        expect(hostMetricAvailable(null, 'cpu')).toBe(false);
        expect(hostMetricAvailable(undefined, 'cpu')).toBe(false);
        expect(hostMetricAvailable({}, 'cpu')).toBe(false);
    });
});
