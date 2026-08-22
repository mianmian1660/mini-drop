// ============================================================
// storageStatus.js — 服务端存储压力状态展示的纯工具函数
// ============================================================
// 与后端 GET /api/v1/storage/status 返回的 data 字段配套：
//   { path, total_bytes, available_bytes, used_bytes,
//     level: normal|warning|critical|emergency|unknown,
//     collection_allowed, checked_at }
//
// 全部为纯函数，便于单元测试。展示语义：
//   - normal / 无数据：不显示任何提示
//   - warning：浅黄色单行提示（只告警）
//   - critical：浅红色提示，请尽快清理
//   - emergency / unknown：浅红色 + "新采集已暂停"
// ============================================================

import { formatCapacity } from './hostMetrics';

// 接口失效时保留上一次成功状态的最长时间（毫秒），超时后隐藏。
export const STORAGE_ALERT_STALE_MS = 90 * 1000;

// 把后端 storage.status() 的 data 计算成页面告警对象：
//   null（不显示）或 { level, tone: 'warning'|'danger', paused, message }
// 任何缺失/异常输入都按"不显示"处理，避免页面出现错误大横幅。
export function computeStorageAlert(status) {
    if (!status || typeof status !== 'object') return null;
    const level = status.level;
    if (!level || level === 'normal') return null;

    const avail = status.available_bytes;
    const remaining =
        typeof avail === 'number' && Number.isFinite(avail) && avail >= 0 ? formatCapacity(avail) : null;

    switch (level) {
        case 'warning':
            return {
                level,
                tone: 'warning',
                paused: false,
                message: remaining ? `服务端存储空间偏低，剩余 ${remaining}` : '服务端存储空间偏低',
            };
        case 'critical':
            return {
                level,
                tone: 'danger',
                paused: false,
                message: remaining
                    ? `服务端存储空间严重不足，请尽快清理（剩余 ${remaining}）`
                    : '服务端存储空间严重不足，请尽快清理',
            };
        case 'emergency':
            return {
                level,
                tone: 'danger',
                paused: true,
                message: remaining
                    ? `服务端存储空间已耗尽，新采集已暂停（剩余 ${remaining}）`
                    : '服务端存储空间已耗尽，新采集已暂停',
            };
        case 'unknown':
            return {
                level,
                tone: 'danger',
                paused: true,
                message: '服务端存储状态未知，新采集已暂停',
            };
        default:
            return null;
    }
}

// 解析"上一次成功状态 + 当前时间"：超过 staleMs 未刷新成功则返回 null（隐藏）。
// 用于接口临时失败时只保留最近一次成功状态，最多 90 秒。
export function resolveStorageAlert(lastSuccess, nowMs, staleMs = STORAGE_ALERT_STALE_MS) {
    if (!lastSuccess) return null;
    const fetchedAt = lastSuccess.fetchedAt;
    if (typeof fetchedAt !== 'number' || !Number.isFinite(fetchedAt)) return null;
    if (nowMs - fetchedAt > staleMs) return null;
    return lastSuccess.alert;
}
