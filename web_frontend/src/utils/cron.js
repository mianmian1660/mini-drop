// ============================================================
// utils/cron.js — cron 表达式转中文友好描述（仅展示，兼容旧任务）
// ============================================================
// 把标准 5 字段 cron（分 时 日 月 周）翻译成「每分钟」「每 5 分钟」
// 「每天 08:00」「每周一 08:00」「工作日 08:00」等易读文本。
// 无法翻译时原样返回表达式。新任务已改用"间隔 + 开始时间"
//（utils/schedule.js 的 intervalHumanLabel），cron 仅用于旧计划兼容展示。
// ============================================================

const DOW_CN = ['日', '一', '二', '三', '四', '五', '六', '日'];
const pad = (v) => String(v).padStart(2, '0');
const isNum = (s) => /^\d+$/.test(String(s));

export function cronHumanLabel(expr) {
    const raw = String(expr || '').trim();
    const parts = raw.split(/\s+/);
    if (parts.length !== 5) return raw;
    const [min, hour, dom, mon, dow] = parts;
    const daily = dom === '*' && mon === '*' && dow === '*';

    // 每分钟 / 每 N 分钟：*/n * * * *
    if (daily && hour === '*' && min.startsWith('*/')) {
        const n = Number(min.slice(2));
        if (Number.isFinite(n) && n >= 1) return n === 1 ? '每分钟' : `每 ${n} 分钟`;
    }
    if (daily && hour === '*' && min === '*') return '每分钟';

    // 每小时 / 每 N 小时：0 */n * * *
    if (daily && min === '0' && hour.startsWith('*/')) {
        const n = Number(hour.slice(2));
        if (Number.isFinite(n) && n >= 1) return n === 1 ? '每小时' : `每 ${n} 小时`;
    }
    if (daily && min === '0' && hour === '*') return '每小时';

    // 每天固定时刻：m h * * *
    if (daily && isNum(min) && isNum(hour)) return `每天 ${pad(hour)}:${pad(min)}`;

    // 每天固定小时：0 h * * *；每小时的 m 分
    if (daily && min === '0' && isNum(hour)) return `每天 ${pad(hour)} 点`;
    if (daily && isNum(min) && hour === '*') return `每小时的 ${pad(min)} 分`;

    // 每周固定：m h * * w
    if (dom === '*' && mon === '*' && isNum(min) && isNum(hour) && isNum(dow)) {
        return `每周${DOW_CN[Number(dow) % 7]} ${pad(hour)}:${pad(min)}`;
    }
    // 工作日：m h * * 1-5
    if (dom === '*' && mon === '*' && dow === '1-5' && isNum(min) && isNum(hour)) {
        return `工作日 ${pad(hour)}:${pad(min)}`;
    }

    // 无法友好描述 → 原样返回
    return raw;
}
