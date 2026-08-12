export const PROFILE_SAMPLE_PERIOD_MS = 1000 / 19;

export function normalizeProfileUnit(unit) {
    return String(unit || '').trim().toLowerCase();
}

export function profileUnitLabel(unit) {
    const normalized = normalizeProfileUnit(unit);
    if (normalized === 'nanoseconds' || normalized === 'ns') return 'CPU 占用时长';
    if (normalized === 'samples' || normalized === 'sample') return 'samples';
    if (normalized === 'count' || normalized === 'counts') return 'count';
    if (normalized === 'bytes' || normalized === 'byte') return 'bytes';
    return unit || 'unknown unit';
}

export function isCPUTimeUnit(unit) {
    const normalized = normalizeProfileUnit(unit);
    return normalized === 'nanoseconds' || normalized === 'ns';
}

export function metricColumnLabel(unit, fallback = 'Value') {
    if (isCPUTimeUnit(unit)) return fallback;
    if (profileUnitLabel(unit) === 'bytes') return 'Bytes';
    if (profileUnitLabel(unit) === 'samples') return 'Samples';
    if (profileUnitLabel(unit) === 'count') return 'Count';
    return fallback;
}

export function formatMetricValue(value, unit) {
    const normalized = normalizeProfileUnit(unit);
    const num = Number(value);
    if (!Number.isFinite(num)) return fallbackZero(normalized);
    const safe = Math.max(0, num);
    if (normalized === 'nanoseconds' || normalized === 'ns') return formatNanoseconds(safe);
    if (normalized === 'bytes' || normalized === 'byte') return formatBytes(safe);
    if (normalized === 'samples' || normalized === 'sample') return `${formatCompactNumber(safe)} samples`;
    if (normalized === 'count' || normalized === 'counts') return formatCompactNumber(safe);
    if (!normalized) return formatCompactNumber(safe);
    return `${formatCompactNumber(safe)} ${unit}`;
}

export function formatPercentFromNode(d) {
    const widthRatio = Number(d?.x1) - Number(d?.x0);
    if (Number.isFinite(widthRatio)) return `${(widthRatio * 100).toFixed(2)}%`;
    const value = Number(d?.value ?? d?.data?.value);
    const total = Number(d?.parent?.value ?? d?.parent?.data?.value);
    if (Number.isFinite(value) && Number.isFinite(total) && total > 0) {
        return `${((value / total) * 100).toFixed(2)}%`;
    }
    return '0.00%';
}

export function formatFlamegraphLabel(d, unit) {
    const name = d?.data?.name || d?.name || 'unknown';
    const value = d?.value ?? d?.data?.value ?? 0;
    if (isCPUTimeUnit(unit)) {
        return `${name} (${formatPercentFromNode(d)}, 占用 ${formatMetricValue(value, unit)})`;
    }
    return `${name} (${formatPercentFromNode(d)}, ${formatMetricValue(value, unit)})`;
}

export function formatProfileTotal(total, unit) {
    return formatMetricValue(total, unit);
}

export function formatRawMetric(value, unit) {
    const raw = Number(value);
    if (!Number.isFinite(raw)) return `0 ${unit || ''}`.trim();
    return `${raw} ${unit || ''}`.trim();
}

function fallbackZero(unit) {
    if (unit === 'nanoseconds' || unit === 'ns') return '0 毫秒';
    if (unit === 'bytes' || unit === 'byte') return '0 B';
    if (unit === 'samples' || unit === 'sample') return '0 samples';
    return '0';
}

function formatNanoseconds(value) {
    if (value >= 1e9) return `${trimFixed(value / 1e9, 2)} 秒`;
    if (value >= 1e6) return `${trimFixed(value / 1e6, 1)} 毫秒`;
    if (value >= 1e3) return `${trimFixed(value / 1e3, 1)} 微秒`;
    return `${trimFixed(value, 0)} 纳秒`;
}

function formatBytes(value) {
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let size = value;
    let idx = 0;
    while (size >= 1024 && idx < units.length - 1) {
        size /= 1024;
        idx += 1;
    }
    return `${trimFixed(size, idx === 0 ? 0 : 1)} ${units[idx]}`;
}

function formatCompactNumber(value) {
    if (value >= 1e9) return `${trimFixed(value / 1e9, 2)}B`;
    if (value >= 1e6) return `${trimFixed(value / 1e6, 2)}M`;
    if (value >= 1e3) return `${trimFixed(value / 1e3, 1)}K`;
    return trimFixed(value, value % 1 === 0 ? 0 : 1);
}

function trimFixed(value, digits) {
    return Number(value).toFixed(digits).replace(/\.0+$|(\.\d*[1-9])0+$/, '$1');
}
