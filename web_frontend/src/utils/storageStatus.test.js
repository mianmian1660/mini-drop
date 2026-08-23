import {
    computeStorageAlert,
    resolveStorageAlert,
    STORAGE_ALERT_STALE_MS,
} from './storageStatus';

describe('computeStorageAlert', () => {
    test('normal level hides the banner', () => {
        expect(computeStorageAlert({ level: 'normal', available_bytes: 20 * 1024 * 1024 * 1024 })).toBeNull();
    });

    test('missing / invalid input hides the banner', () => {
        expect(computeStorageAlert(null)).toBeNull();
        expect(computeStorageAlert(undefined)).toBeNull();
        expect(computeStorageAlert({})).toBeNull();
        expect(computeStorageAlert({ level: 'normal' })).toBeNull();
        expect(computeStorageAlert({ level: 'something-else' })).toBeNull();
        expect(computeStorageAlert({ level: '' })).toBeNull();
    });

    test('warning shows light-yellow single line with remaining capacity', () => {
        const alert = computeStorageAlert({ level: 'warning', available_bytes: 6 * 1024 * 1024 * 1024 });
        expect(alert).toEqual({
            level: 'warning',
            tone: 'warning',
            paused: false,
            message: expect.stringContaining('服务端存储空间偏低'),
        });
        expect(alert.message).toContain('6.0 GiB');
    });

    test('warning without capacity still shows the message', () => {
        const alert = computeStorageAlert({ level: 'warning' });
        expect(alert).not.toBeNull();
        expect(alert.message).toBe('服务端存储空间偏低');
    });

    test('critical shows red prompt to clean up soon', () => {
        const alert = computeStorageAlert({ level: 'critical', available_bytes: 2 * 1024 * 1024 * 1024 });
        expect(alert.tone).toBe('danger');
        expect(alert.paused).toBe(false);
        expect(alert.message).toContain('请尽快清理');
        expect(alert.message).toContain('2.0 GiB');
    });

    test('emergency shows paused hint with remaining capacity', () => {
        const alert = computeStorageAlert({ level: 'emergency', available_bytes: 512 * 1024 * 1024 });
        expect(alert.tone).toBe('danger');
        expect(alert.paused).toBe(true);
        expect(alert.message).toContain('新采集已暂停');
        expect(alert.message).toContain('512 MiB');
    });

    test('unknown shows paused hint without fabricated capacity', () => {
        const alert = computeStorageAlert({ level: 'unknown', available_bytes: 0 });
        expect(alert.tone).toBe('danger');
        expect(alert.paused).toBe(true);
        expect(alert.message).toBe('服务端存储状态未知，新采集已暂停');
    });

    test('invalid capacity falls back to message without capacity', () => {
        const alert = computeStorageAlert({ level: 'emergency', available_bytes: 'oops' });
        expect(alert.message).toBe('服务端存储空间已耗尽，新采集已暂停');
        const negative = computeStorageAlert({ level: 'critical', available_bytes: -5 });
        expect(negative.message).toBe('服务端存储空间严重不足，请尽快清理');
    });
});

describe('resolveStorageAlert', () => {
    const alert = { level: 'warning', tone: 'warning', paused: false, message: 'x' };

    test('no previous success returns null', () => {
        expect(resolveStorageAlert(null, Date.now())).toBeNull();
        expect(resolveStorageAlert(undefined, Date.now())).toBeNull();
    });

    test('fresh success keeps the alert', () => {
        const last = { alert, fetchedAt: Date.now() - 10 * 1000 };
        expect(resolveStorageAlert(last, Date.now())).toBe(alert);
    });

    test('exactly at stale boundary keeps the alert', () => {
        const last = { alert, fetchedAt: Date.now() - STORAGE_ALERT_STALE_MS };
        expect(resolveStorageAlert(last, Date.now())).toBe(alert);
    });

    test('beyond stale window hides the alert', () => {
        const last = { alert, fetchedAt: Date.now() - (STORAGE_ALERT_STALE_MS + 1) };
        expect(resolveStorageAlert(last, Date.now())).toBeNull();
    });

    test('malformed fetchedAt hides the alert', () => {
        expect(resolveStorageAlert({ alert, fetchedAt: 'now' }, Date.now())).toBeNull();
        expect(resolveStorageAlert({ alert, fetchedAt: undefined }, Date.now())).toBeNull();
    });
});
