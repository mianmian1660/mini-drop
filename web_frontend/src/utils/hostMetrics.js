// ============================================================
// hostMetrics.js — 整机资源（host 对象）展示的纯工具函数
// ============================================================
// 与后端 /api/v1/agent/detail、/api/v1/agent/stat 返回的
// stat.host 对象配套。全部为纯函数，便于单元测试。
// ============================================================

// 百分比做 0-100 防御性限制：非法值/缺失一律按 0 处理，
// 避免后端返回异常值导致进度条溢出或负数。
export function clampPercent(value) {
    const num = Number(value);
    if (!Number.isFinite(num)) return 0;
    return Math.min(100, Math.max(0, num));
}

// 容量自动格式化：B / KiB / MiB / GiB / TiB。
// 无效或负数返回 '--'（与"无数据"语义一致）。
export function formatCapacity(bytes) {
    if (bytes === null || bytes === undefined) return '--';
    const num = Number(bytes);
    if (!Number.isFinite(num) || num < 0) return '--';
    if (num === 0) return '0 B';
    if (num < 1024) return `${num} B`;
    const units = ['KiB', 'MiB', 'GiB', 'TiB'];
    let value = num;
    let unitIndex = -1;
    do {
        value /= 1024;
        unitIndex++;
    } while (value >= 1024 && unitIndex < units.length - 1);
    const digits = value >= 100 ? 0 : 1;
    return `${value.toFixed(digits)} ${units[unitIndex]}`;
}

// 进度条颜色：低于 70% 用品牌蓝，70-89% 用警告橙，90% 及以上用危险红。
export function usageColor(percent) {
    const pct = clampPercent(percent);
    if (pct >= 90) return '#dc2626'; // 危险红
    if (pct >= 70) return '#f59e0b'; // 警告橙
    return '#315efb';                // 品牌蓝
}

// 把 RFC3339 采集时间格式化为"HH:MM:SS"，无效/缺失返回 '--'
export function formatCollectedAt(rfc3339) {
    if (!rfc3339) return '--';
    const date = new Date(rfc3339);
    if (Number.isNaN(date.getTime())) return '--';
    return date.toLocaleTimeString();
}

// 判断某个 host 维度是否可用（后端 *_available 标志；缺失视为不可用）
export function hostMetricAvailable(host, field) {
    return Boolean(host && host[`${field}_available`] === true);
}
