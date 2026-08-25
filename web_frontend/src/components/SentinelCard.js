// ============================================================
// components/SentinelCard.js — 持续采集详情页的"当前哨兵"卡片
// ============================================================
// 按 docs/sentinel-rule-frontend-design.md §3：哨兵规则依附于它监控的
// 持续采集会话，不单独起一个管理页面。这里只做"看这个会话身上挂了哪些
// 哨兵、加一个、删一个"，判异事件的时间轴呈现见 ContinuousProfilingPanel。
// ============================================================

import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { sentinelRules } from '../api';
import { DB_SENTINEL_METRICS, SENTINEL_SIGNALS, signalLabel } from '../utils/continuous';

// db_snapshot 判异方式的展示文案，跟 DB_SENTINEL_METRICS 的取值一一对应。
const DB_METRIC_LABEL = { lock_wait: '锁等待', digest: '慢SQL环比' };

// 哨兵规则一行的展示文案：histogram 类信号（p50/p95/p99，微秒存储，前端按ms展示）
// 和 db_snapshot 信号（锁等待用秒、慢SQL环比用倍数）单位完全不同，不能共用同一套
// "floor_value / 1000 当ms" 的换算，否则数据库那两行会显示出错误的单位。
function ruleSummary(rule) {
    if (rule.signal === 'db_snapshot') {
        if (rule.metric === 'lock_wait') return `数据库 · 锁等待 > ${rule.floor_value} s`;
        if (rule.metric === 'digest') return `数据库 · 慢SQL环比 > ${rule.k_factor} 倍（下限 ${rule.floor_value / 1000} ms）`;
        return `数据库 · ${rule.metric}`;
    }
    return `${signalLabel(rule.signal)} · ${rule.metric} > ${rule.floor_value / 1000} ms`;
}

const S = {
    head: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 10, marginBottom: 10 },
    title: { margin: 0, fontSize: 15 },
    link: { color: '#315efb', fontWeight: 700, textDecoration: 'none', background: 'none', border: 0, padding: 0, cursor: 'pointer', fontSize: 13 },
    empty: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: 14, border: '1px dashed #d0d5dd', borderRadius: 8, color: '#667085', fontSize: 13 },
    row: { display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', gap: 12, padding: '12px 14px', border: '1px solid #e5e7eb', borderRadius: 8, background: '#f9fafb' },
    rowTitle: { fontWeight: 700, color: '#101828', marginBottom: 4 },
    subtle: { color: '#667085', fontSize: 12 },
    badge: { display: 'inline-flex', borderRadius: 999, padding: '3px 8px', fontSize: 12, fontWeight: 700, background: '#eaf6ea', color: '#1c7d3f', marginTop: 6 },
    danger: { color: '#b42318', background: 'none', border: 0, padding: 0, cursor: 'pointer', fontWeight: 700, fontSize: 12 },
    error: { color: '#b42318', background: '#fff6f5', border: '1px solid #fda29b', borderRadius: 6, padding: 10, fontSize: 13, marginTop: 10 },
    form: { marginTop: 10, padding: 14, border: '1px solid #c7d2fe', background: '#eef2ff', borderRadius: 8, display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12 },
    label: { display: 'block', color: '#344054', fontSize: 12, fontWeight: 700, marginBottom: 5 },
    input: { width: '100%', height: 34, padding: '6px 9px', boxSizing: 'border-box', border: '1px solid #d0d5dd', borderRadius: 6, fontSize: 13 },
    formActions: { gridColumn: '1/-1', display: 'flex', justifyContent: 'flex-end', gap: 8, marginTop: 4 },
    cancelBtn: { background: '#fff', color: '#475467', border: '1px solid #d0d5dd', borderRadius: 6, padding: '6px 12px', fontWeight: 700, cursor: 'pointer', fontSize: 12 },
    submitBtn: disabled => ({ background: disabled ? '#e5e7eb' : '#315efb', color: disabled ? '#98a2b3' : '#fff', border: 0, borderRadius: 6, padding: '6px 12px', fontWeight: 700, cursor: disabled ? 'not-allowed' : 'pointer', fontSize: 12 }),
};

export default function SentinelCard({ targetIP, signals = [], hasDBTargets = false }) {
    const eligibleKey = signals.filter(signal => SENTINEL_SIGNALS.includes(signal)).join(',');
    const eligibleSignals = useMemo(() => (eligibleKey ? eligibleKey.split(',') : []), [eligibleKey]);
    const hasAnyEligible = eligibleSignals.length > 0 || hasDBTargets;
    const [rules, setRules] = useState([]);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState('');
    const [deletingSid, setDeletingSid] = useState('');
    const [adding, setAdding] = useState(false);

    const load = useCallback(async () => {
        if (!targetIP) return;
        setLoading(true);
        setError('');
        try {
            const response = await sentinelRules.list({ target_ip: targetIP });
            if (response.code !== 0) throw new Error(response.message || '加载哨兵规则失败');
            const all = response.data?.rules || [];
            setRules(all.filter(rule => eligibleSignals.includes(rule.signal) || (hasDBTargets && rule.signal === 'db_snapshot')));
        } catch (err) {
            setError(err?.message || '加载哨兵规则失败');
        } finally {
            setLoading(false);
        }
    }, [targetIP, eligibleSignals, hasDBTargets]);

    useEffect(() => { load(); }, [load]);

    const remove = async (rule) => {
        if (!window.confirm(`停止「${rule.name}」的哨兵监控？停止后不再自动触发深度诊断。`)) return;
        setDeletingSid(rule.sid);
        setError('');
        try {
            const response = await sentinelRules.delete(rule.sid);
            if (response.code !== 0) throw new Error(response.message || '删除失败');
            await load();
        } catch (err) {
            setError(err?.response?.data?.message || err?.message || '删除失败');
        } finally {
            setDeletingSid('');
        }
    };

    return <div>
        <div style={S.head}><h3 style={S.title}>当前哨兵</h3>{hasAnyEligible && !adding && <button style={S.link} onClick={() => setAdding(true)}>+ 添加哨兵</button>}</div>
        {error && <div style={S.error}>{error}</div>}
        {loading ? <div style={S.subtle}>正在加载...</div>
            : !hasAnyEligible ? <div style={S.subtle}>当前会话没有可用于哨兵判异的信号（仅调度延迟 / IO 延迟 / 数据库支持）。</div>
                : rules.length === 0 && !adding ? <div style={S.empty}><span>尚未设置哨兵，异常波动不会自动触发深度诊断</span><button style={S.link} onClick={() => setAdding(true)}>+ 添加哨兵</button></div>
                    : <div style={{ display: 'grid', gap: 8 }}>{rules.map(rule => (
                        <div key={rule.sid} style={S.row}>
                            <div>
                                <div style={S.rowTitle}>{ruleSummary(rule)}</div>
                                <span style={S.subtle}>冷却期 {Math.round(rule.cooldown_seconds / 60)} 分钟</span>
                                <div><span style={S.badge}>已启用</span></div>
                            </div>
                            {rule.can_manage && <button style={S.danger} disabled={deletingSid === rule.sid} onClick={() => remove(rule)}>{deletingSid === rule.sid ? '删除中...' : '删除'}</button>}
                        </div>
                    ))}</div>}
        {adding && <AddSentinelForm targetIP={targetIP} signals={eligibleSignals} hasDBTargets={hasDBTargets} onCancel={() => setAdding(false)} onCreated={() => { setAdding(false); load(); }} />}
    </div>;
}

function AddSentinelForm({ targetIP, signals, hasDBTargets = false, onCancel, onCreated }) {
    const [signal, setSignal] = useState(signals[0] || (hasDBTargets ? 'db_snapshot' : ''));
    const isDB = signal === 'db_snapshot';
    // dbMetric 只在 isDB 时有意义，两个 metric 各自的输入字段和单位完全不同
    // （见 SentinelCard.js 顶部 ruleSummary 的注释），所以状态也分开维护，不复用
    // histogram 分支的 floor（避免切换信号时把上一次输入的值带过去造成误解）。
    const [dbMetric, setDbMetric] = useState(DB_SENTINEL_METRICS[0]);
    const [floor, setFloor] = useState('5'); // histogram 类：ms
    const [lockSeconds, setLockSeconds] = useState('3'); // db lock_wait：秒
    const [digestFloorMs, setDigestFloorMs] = useState('50'); // db digest：ms（下限）
    const [kFactor, setKFactor] = useState('5'); // db digest：环比倍数
    const [cooldownMin, setCooldownMin] = useState(15);
    const [submitting, setSubmitting] = useState(false);
    const [error, setError] = useState('');

    const valid = signal && Number(cooldownMin) > 0 && (
        !isDB ? Number(floor) > 0
            : dbMetric === 'lock_wait' ? Number(lockSeconds) > 0
                : Number(digestFloorMs) > 0 && Number(kFactor) > 0
    );

    const submit = async () => {
        if (!valid || submitting) return;
        setSubmitting(true);
        setError('');
        try {
            const payload = !isDB
                ? {
                    name: `${signalLabel(signal)} 哨兵`, target_ip: targetIP, signal, metric: 'p99',
                    // 后端 floor_value 单位是微秒(us)，前端输入框单位是毫秒(ms)，提交时 ×1000 换算。
                    floor_value: Number(floor) * 1000,
                }
                : dbMetric === 'lock_wait'
                    ? {
                        // 后端 wait_seconds 单位就是秒，不能沿用 histogram 那套 ×1000 换算。
                        name: `${DB_METRIC_LABEL.lock_wait} 哨兵`, target_ip: targetIP, signal, metric: 'lock_wait',
                        floor_value: Number(lockSeconds),
                    }
                    : {
                        // digest 的 total_latency_us 后端按微秒存储，前端输入 ms，同样 ×1000。
                        name: `${DB_METRIC_LABEL.digest} 哨兵`, target_ip: targetIP, signal, metric: 'digest',
                        floor_value: Number(digestFloorMs) * 1000, k_factor: Number(kFactor),
                    };
            const response = await sentinelRules.create({ ...payload, cooldown_seconds: Math.round(Number(cooldownMin) * 60) });
            if (response.code !== 0) throw new Error(response.message || '创建哨兵规则失败');
            onCreated();
        } catch (err) {
            setError(err?.response?.data?.message || err?.message || '创建哨兵规则失败');
        } finally {
            setSubmitting(false);
        }
    };

    return <div style={S.form}>
        <label style={isDB ? { gridColumn: '1/-1' } : undefined}><span style={S.label}>监控信号</span>
            <select style={S.input} value={signal} onChange={event => setSignal(event.target.value)}>
                {signals.map(item => <option key={item} value={item}>{signalLabel(item)} · p99</option>)}
                {hasDBTargets && <option value="db_snapshot">数据库</option>}
            </select>
        </label>
        {isDB && <label style={{ gridColumn: '1/-1' }}><span style={S.label}>判异方式</span>
            <select style={S.input} value={dbMetric} onChange={event => setDbMetric(event.target.value)}>
                {DB_SENTINEL_METRICS.map(item => <option key={item} value={item}>{DB_METRIC_LABEL[item]}</option>)}
            </select>
        </label>}
        {!isDB && <label><span style={S.label}>告警阈值 ms</span><input style={S.input} type="number" min={0.1} step={0.5} value={floor} onChange={event => setFloor(event.target.value)} /></label>}
        {isDB && dbMetric === 'lock_wait' && <label><span style={S.label}>等待阈值（秒）</span><input style={S.input} type="number" min={0.5} step={0.5} value={lockSeconds} onChange={event => setLockSeconds(event.target.value)} /></label>}
        {isDB && dbMetric === 'digest' && <>
            <label><span style={S.label}>最小耗时下限（ms）</span><input style={S.input} type="number" min={1} value={digestFloorMs} onChange={event => setDigestFloorMs(event.target.value)} /></label>
            <label><span style={S.label}>环比倍数</span><input style={S.input} type="number" min={1} step={0.5} value={kFactor} onChange={event => setKFactor(event.target.value)} /></label>
        </>}
        <label><span style={S.label}>冷却期（分钟）</span><input style={S.input} type="number" min={1} value={cooldownMin} onChange={event => setCooldownMin(event.target.value)} /></label>
        {error && <div style={{ ...S.error, gridColumn: '1/-1', marginTop: 0 }}>{error}</div>}
        <div style={S.formActions}>
            <button style={S.cancelBtn} onClick={onCancel} disabled={submitting}>取消</button>
            <button style={S.submitBtn(!valid || submitting)} onClick={submit} disabled={!valid || submitting}>{submitting ? '创建中...' : '创建哨兵'}</button>
        </div>
    </div>;
}
