// ============================================================
// utils/schedule.test.js — 周期计划文案与周期标签测试
// ============================================================

import { intervalHumanLabel, schedulePeriodLabel, schedulePeriodTitle, scheduleStatusText, scheduleUsesInterval } from './schedule';

test('intervalHumanLabel 把间隔秒数转成中文描述', () => {
    expect(intervalHumanLabel(60)).toBe('每 1 分钟');
    expect(intervalHumanLabel(300)).toBe('每 5 分钟');
    expect(intervalHumanLabel(600)).toBe('每 10 分钟');
    expect(intervalHumanLabel(1800)).toBe('每 30 分钟');
    expect(intervalHumanLabel(3600)).toBe('每小时');
    expect(intervalHumanLabel(7200)).toBe('每 2 小时');
    expect(intervalHumanLabel(45)).toBe('每 45 秒');
    expect(intervalHumanLabel(0)).toBe('');
    expect(intervalHumanLabel(undefined)).toBe('');
});

test('scheduleUsesInterval 判断间隔型计划', () => {
    expect(scheduleUsesInterval({ interval_seconds: 300 })).toBe(true);
    expect(scheduleUsesInterval({ interval_seconds: 0 })).toBe(false);
    expect(scheduleUsesInterval({ cron_expr: '*/5 * * * *' })).toBe(false);
});

test('schedulePeriodLabel 间隔型显示每 X 分钟，旧 cron 显示友好描述', () => {
    expect(schedulePeriodLabel({ interval_seconds: 300 })).toBe('每 5 分钟');
    expect(schedulePeriodLabel({ cron_expr: '*/5 * * * *' })).toBe('每 5 分钟');
    expect(schedulePeriodLabel({ cron_expr: '0 8 * * *' })).toBe('每天 08:00');
    expect(schedulePeriodLabel({})).toBe('周期计划');
});

test('schedulePeriodTitle 提供原始值与含义说明', () => {
    const interval = schedulePeriodTitle({ interval_seconds: 300 });
    expect(interval).toContain('每 5 分钟自动触发一次深度采样');
    expect(interval).toContain('间隔 300 秒');

    const cron = schedulePeriodTitle({ cron_expr: '*/5 * * * *' });
    expect(cron).toContain('旧版 Cron 表达式');
    expect(cron).toContain('*/5 * * * *');
});

test('scheduleStatusText 提供启停状态的可执行解释', () => {
    expect(scheduleStatusText({ enabled: true })).toContain('计划运行中');
    expect(scheduleStatusText({ enabled: false })).toContain('计划已停用');
});
