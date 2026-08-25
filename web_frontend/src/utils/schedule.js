// ============================================================
// utils/schedule.js — 周期计划文案字典与周期标签
// ============================================================
// 周期计划两种模式：
//   - 间隔型（新建默认）：interval_seconds（秒），展示"每 X 分钟采集一次"；
//   - 旧 cron 型（兼容）：cron_expr，回退到 cronHumanLabel 友好描述。
// 统一在这里维护中文名称、用途、状态解释，避免各页面自行拼接技术名。
// ============================================================

import { cronHumanLabel } from './cron';

// intervalHumanLabel 把间隔秒数转成「每 X 分钟 / 每小时」中文描述。
export function intervalHumanLabel(seconds) {
    const value = Number(seconds);
    if (!Number.isFinite(value) || value <= 0) return '';
    if (value % 3600 === 0) {
        const hours = value / 3600;
        return hours === 1 ? '每小时' : `每 ${hours} 小时`;
    }
    if (value % 60 === 0) {
        const minutes = value / 60;
        return minutes === 1 ? '每 1 分钟' : `每 ${minutes} 分钟`;
    }
    return value === 1 ? '每 1 秒' : `每 ${value} 秒`;
}

// scheduleUsesInterval 判断计划是否为间隔型（新任务默认）。
export function scheduleUsesInterval(sch) {
    return Number(sch?.interval_seconds) > 0;
}

// schedulePeriodLabel 计划执行周期的自然语言描述：间隔型显示"每 X 分钟"，
// 旧 cron 型显示 cron 友好描述。原始 cron 仅在 title/tooltip 中作为诊断信息。
export function schedulePeriodLabel(sch) {
    if (scheduleUsesInterval(sch)) {
        return intervalHumanLabel(sch.interval_seconds) || '周期计划';
    }
    return cronHumanLabel(sch.cron_expr) || '周期计划';
}

// schedulePeriodTitle 计划执行周期的悬停说明：解释"这个设置意味着什么"
// 并提供原始值（间隔秒数 / 原始 cron）作为诊断信息。
export function schedulePeriodTitle(sch) {
    if (scheduleUsesInterval(sch)) {
        const label = intervalHumanLabel(sch.interval_seconds) || '';
        return `${label}自动触发一次深度采样（间隔 ${sch.interval_seconds} 秒）`;
    }
    const raw = String(sch?.cron_expr || '').trim();
    return raw ? `旧版 Cron 表达式（${raw}），兼容保留` : '周期计划';
}

// scheduleStatusText 计划启停状态的中文解释（含可执行建议）。
export function scheduleStatusText(sch) {
    if (sch?.enabled === true) {
        return '计划运行中：到点自动创建深度采样窗口，可在详情页随时停用。';
    }
    return '计划已停用：不会再自动创建采集窗口，可在详情页重新启用。';
}

// scheduleWindowLabel 窗口时长（采样时长）的自然语言描述。
export function scheduleWindowLabel(sch) {
    const value = Number(sch?.duration);
    if (!Number.isFinite(value) || value <= 0) return '';
    return `每个窗口持续 ${value} 秒`;
}

// COLLECTOR_FIELD_LABELS 采集器字段的中文名称与用途说明（悬停/文案用）。
// key 为 TaskKind schema 的字段名，value 提供"技术名 + 普通用户解释"。
export const FIELD_LABELS = {
    duration: { label: '采样时长', hint: '每个采集窗口持续多久（秒）' },
    frequency: { label: '采样频率', hint: '每秒采集多少次（Hz）' },
    interval_seconds: { label: '采样间隔', hint: '每隔多久自动采集一次（秒）' },
    start_at: { label: '开始时间', hint: '计划首次运行的时间；之前的时间会立即生效' },
    window_seconds: { label: '窗口周期', hint: '相邻两次采样的间隔，保证窗口不重叠（秒）' },
    callgraph: { label: '调用栈模式', hint: 'fp = 帧指针，dwarf = DWARF 调试信息' },
    event: { label: '采样事件', hint: 'perf 采样事件，如 cpu-clock / cycles' },
    target_pid: { label: '目标进程 PID', hint: '指定要采集的进程' },
};
