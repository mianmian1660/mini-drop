export function parseTime(value) {
    if (!value) return null;
    const date = value instanceof Date ? value : new Date(value);
    return Number.isNaN(date.getTime()) ? null : date;
}

export function formatDateTime(value) {
    const date = parseTime(value);
    if (!date) return value ? String(value) : '';
    return date.toLocaleString();
}

export function formatTimeShort(value) {
    const date = parseTime(value);
    if (!date) return '';
    return new Intl.DateTimeFormat(undefined, {
        hour: '2-digit',
        minute: '2-digit',
        hour12: false,
    }).format(date);
}

export function browserTimeZoneLabel() {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || '浏览器本地时区';
}

export function localDateTimeToISO(value) {
    const date = parseTime(value);
    return date ? date.toISOString() : '';
}
