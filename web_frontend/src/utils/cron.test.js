// ============================================================
// utils/cron.test.js — cron 表达式中文友好描述测试
// ============================================================

import { cronHumanLabel } from './cron';

test('每分钟类', () => {
    expect(cronHumanLabel('*/1 * * * *')).toBe('每 1 分钟');
    expect(cronHumanLabel('* * * * *')).toBe('每 1 分钟');
    expect(cronHumanLabel('*/5 * * * *')).toBe('每 5 分钟');
    expect(cronHumanLabel('*/30 * * * *')).toBe('每 30 分钟');
});

test('每小时类', () => {
    expect(cronHumanLabel('0 * * * *')).toBe('每小时');
    expect(cronHumanLabel('0 */1 * * *')).toBe('每小时');
    expect(cronHumanLabel('0 */6 * * *')).toBe('每 6 小时');
});

test('每天固定时刻', () => {
    expect(cronHumanLabel('30 * * * *')).toBe('每小时的 30 分');
    expect(cronHumanLabel('0 8 * * *')).toBe('每天 08:00');
    expect(cronHumanLabel('15 9 * * *')).toBe('每天 09:15');
});

test('每周/工作日', () => {
    expect(cronHumanLabel('0 8 * * 1')).toBe('每周一 08:00');
    expect(cronHumanLabel('0 8 * * 0')).toBe('每周日 08:00');
    expect(cronHumanLabel('0 8 * * 1-5')).toBe('工作日 08:00');
});

test('无法翻译时原样返回', () => {
    expect(cronHumanLabel('0 9 * * 0,6')).toBe('0 9 * * 0,6');
    expect(cronHumanLabel('0 0 1 1 *')).toBe('0 0 1 1 *');
    expect(cronHumanLabel('')).toBe('');
    expect(cronHumanLabel(undefined)).toBe('');
    // 非 5 字段
    expect(cronHumanLabel('@daily')).toBe('@daily');
});
