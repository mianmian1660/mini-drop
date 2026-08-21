// ============================================================
// pages/TimelinePage.js — Periodic Deep Sampling 时间轴
// 自动加载所有定时任务，点击即可查看历史采集窗口
// 支持三种查询：全部窗口 / 时间区间（快捷或自定义）/ 按时刻回溯
// ============================================================

import React, { useState, useEffect, useCallback } from 'react';
import { Link } from 'react-router-dom';
import { tasks, schedules } from '../api';
import TimelineChart, { statusColor } from '../components/TimelineChart';
import { browserTimeZoneLabel, formatDateTime, localDateTimeToISO } from '../utils/time';

const S = {
    container: { width: '100%', maxWidth: 1200, minWidth: 0, margin: '0 auto', padding: 20, fontFamily: 'Arial, sans-serif' },
    card: { minWidth: 0, maxWidth: '100%', background: '#fff', borderRadius: 8, padding: 24, marginBottom: 16, boxShadow: '0 2px 8px rgba(0,0,0,0.1)' },
    btn: { background: '#4a6cf7', color: '#fff', border: 'none', padding: '8px 16px', borderRadius: 6, cursor: 'pointer', fontSize: 13 },
    btnSm: { background: '#e0e0e0', color: '#333', border: 'none', padding: '4px 10px', borderRadius: 4, cursor: 'pointer', fontSize: 12, marginRight: 6 },
    input: { padding: '7px 10px', border: '1px solid #ddd', borderRadius: 6, fontSize: 13, marginBottom: 12, boxSizing: 'border-box' },
    rangeBtn: (active) => ({
        background: active ? '#4a6cf7' : '#f0f2f8', color: active ? '#fff' : '#555',
        border: active ? '1px solid #4a6cf7' : '1px solid #dde1ec',
        padding: '6px 14px', borderRadius: 6, cursor: 'pointer', fontSize: 13, marginRight: 8,
    }),
    timeline: { position: 'relative', paddingLeft: 30, borderLeft: '3px solid #4a6cf7', marginLeft: 10 },
    point: (st, eff) => ({
        position: 'relative', marginBottom: 20, padding: '10px 16px',
        background: st === 2 ? '#e8f5e9' : st === 3 ? '#ffebee' : st === 4 ? '#f3e8ff' : st === 1 ? '#e3f2fd' : '#f5f5f5',
        borderRadius: 6, border: eff ? '2px solid #4a6cf7' : '1px solid #eee',
    }),
    dot: (st) => ({
        position: 'absolute', left: -39, top: 12, width: 14, height: 14, borderRadius: '50%',
        background: st === 2 ? '#4caf50' : st === 3 ? '#f44336' : st === 4 ? '#7c3aed' : st === 1 ? '#2196f3' : '#ccc',
        border: '2px solid #fff', boxShadow: '0 0 0 2px #4a6cf7',
    }),
    loading: { textAlign: 'center', padding: 60, color: '#999' },
    label: { display: 'block', marginBottom: 8, fontWeight: 'bold', fontSize: 14, color: '#555' },
    hint: { fontSize: 12, color: '#888', marginTop: 4 },
    schRow: (active) => ({
        padding: '12px 16px', marginBottom: 8, borderRadius: 6, cursor: 'pointer',
        background: active ? '#e8f0ff' : '#fafafa', border: active ? '1px solid #4a6cf7' : '1px solid #e0e0e0',
        display: 'flex', justifyContent: 'space-between', alignItems: 'center',
    }),
};

const ST = { 0: '待处理', 1: '执行中', 2: '已完成', 3: '失败', 4: '上传中' };
const isActiveTask = (status) => status === 0 || status === 1 || status === 4;

export default function TimelinePage() {
    const [schList, setSchList] = useState([]);
    const [masterTid, setMasterTid] = useState('');
    const [points, setPoints] = useState([]);
    const [loading, setLoading] = useState(false);
    const [schLoading, setSchLoading] = useState(true);
    const [error, setError] = useState('');
    const [atInput, setAtInput] = useState('');       // datetime-local 输入值
    const [spanInput, setSpanInput] = useState('30m'); // 回溯时刻前后取多长区间
    const [baselineTid, setBaselineTid] = useState(''); // 已选作对比基线的窗口
    const [queryMode, setQueryMode] = useState('list'); // 'list' 全部窗口 | 'at' 按时刻回溯 | 'range' 时间区间
    const [fromInput, setFromInput] = useState('');   // 自定义区间起点（datetime-local）
    const [toInput, setToInput] = useState('');       // 自定义区间终点（datetime-local）
    const [showCustom, setShowCustom] = useState(false);  // 自定义区间输入是否展开
    const [activeRange, setActiveRange] = useState(0);    // 选中的快捷区间小时数；-1 表示自定义；0 表示未选中
    // 已生效区间保存为两个字符串而非对象，避免轮询 effect 因引用变化被反复重建
    const [rangeFrom, setRangeFrom] = useState('');
    const [rangeTo, setRangeTo] = useState('');
    const [statusFilter, setStatusFilter] = useState('');
    const [kindFilter, setKindFilter] = useState('');
    const [hasResultFilter, setHasResultFilter] = useState('');
    const [trends, setTrends] = useState(null);

    const timelineFilters = useCallback(() => ({
        status: statusFilter || undefined,
        task_kind: kindFilter || undefined,
        has_result: hasResultFilter || undefined,
    }), [statusFilter, kindFilter, hasResultFilter]);

    // 加载定时任务列表
    useEffect(() => {
        schedules.list().then(r => {
            if (r.code === 0) setSchList(r.data?.schedules || []);
        }).catch(() => { }).finally(() => setSchLoading(false));
    }, []);

    const loadTimeline = useCallback(async (sid) => {
        if (!sid) return;
        setMasterTid(sid);
        setQueryMode('list');
        setActiveRange(0);
        setLoading(true); setError('');
        try {
            const r = await tasks.timeline(sid, timelineFilters());
            if (r.code === 0) { setPoints(r.data?.points || []); setTrends(r.data?.trends || null); }
            else setError(r.message || '查询失败');
        } catch (e) { setError('请求失败: ' + (e.message || '')); }
        finally { setLoading(false); }
    }, [timelineFilters]);

    // 按时刻回溯：返回 [at-span, at+span] 内的全部窗口，其中当时生效的一个带 is_effective
    const loadAt = useCallback(async () => {
        if (!masterTid || !atInput) return;
        setQueryMode('at');
        setActiveRange(0);
        setLoading(true); setError('');
        try {
            const atISO = new Date(atInput).toISOString();
            const r = await tasks.timeline(masterTid, { at: atISO, span: spanInput, ...timelineFilters() });
            if (r.code === 0) { setPoints(r.data?.points || []); setTrends(r.data?.trends || null); }
            else setError(r.message || '查询失败');
        } catch (e) { setError('请求失败: ' + (e.message || '')); }
        finally { setLoading(false); }
    }, [masterTid, atInput, spanInput, timelineFilters]);

    // 区间查询：返回 [from, to] 内触发的全部采集窗口
    const loadRange = useCallback(async (fromISO, toISO) => {
        if (!masterTid) return;
        setQueryMode('range');
        setRangeFrom(fromISO); setRangeTo(toISO);
        setLoading(true); setError('');
        try {
            const r = await tasks.timeline(masterTid, { from: fromISO, to: toISO, ...timelineFilters() });
            if (r.code === 0) { setPoints(r.data?.points || []); setTrends(r.data?.trends || null); }
            else setError(r.message || '查询失败');
        } catch (e) { setError('请求失败: ' + (e.message || '')); }
        finally { setLoading(false); }
    }, [masterTid, timelineFilters]);

    // 快捷区间：最近 N 小时（区间在点击时固定，不随轮询滑动）
    const loadQuickRange = useCallback((hours) => {
        const to = new Date();
        const from = new Date(to.getTime() - hours * 3600 * 1000);
        setActiveRange(hours);
        setShowCustom(false);
        loadRange(from.toISOString(), to.toISOString());
    }, [loadRange]);

    // 自定义区间：读取两个 datetime-local 输入（按浏览器本地时区解释）
    const loadCustomRange = useCallback(() => {
        if (!fromInput || !toInput) {
            setError('请选择自定义开始时间和结束时间后再查询');
            return;
        }
        const fromISO = localDateTimeToISO(fromInput);
        const toISO = localDateTimeToISO(toInput);
        if (!fromISO || !toISO) {
            setError('自定义时间格式无效');
            return;
        }
        if (new Date(toISO) < new Date(fromISO)) {
            setError('结束时间不能早于开始时间');
            return;
        }
        setActiveRange(-1);
        loadRange(fromISO, toISO);
    }, [fromInput, toInput, loadRange]);

    // 自动轮询：沿用当前查询模式（全部窗口 / 时间区间 / 按时刻回溯）重新拉取
    useEffect(() => {
        const hasRunning = points.some(p => isActiveTask(p.status));
        if (!hasRunning || !masterTid) return;
        const iv = setInterval(() => {
            if (queryMode === 'at') loadAt();
            else if (queryMode === 'range') { if (rangeFrom && rangeTo) loadRange(rangeFrom, rangeTo); }
            else loadTimeline(masterTid);
        }, 5000);
        return () => clearInterval(iv);
    }, [points, masterTid, queryMode, rangeFrom, rangeTo, loadTimeline, loadAt, loadRange]);

    const refreshSchedules = () => {
        setSchLoading(true);
        schedules.list().then(r => {
            if (r.code === 0) setSchList(r.data?.schedules || []);
        }).catch(() => { }).finally(() => setSchLoading(false));
    };

    const toggleSchedule = async (sid, e) => {
        e.stopPropagation();  // 阻止触发行点击
        try {
            await schedules.toggle(sid);
            refreshSchedules();
        } catch (err) { alert('操作失败: ' + err.message); }
    };

    const deleteSchedule = async (sid, e) => {
        e.stopPropagation();
        if (!window.confirm('确定删除此定时任务？相关采集记录保留。')) return;
        try {
            await schedules.delete(sid);
            refreshSchedules();
            if (masterTid === sid) { setMasterTid(''); setPoints([]); }
        } catch (err) { alert('删除失败: ' + err.message); }
    };

    return (
        <div style={S.container}>
            <h2>周期性深度采样时间轴</h2>
            <div style={S.card}>
                <h3 style={{ marginTop: 0 }}>请优先在主机性能中心查看时间轴</h3>
                <p style={S.hint}>
                    时间轴已经迁入具体主机页面，会按当前主机过滤周期性深度采样窗口。这里保留为兼容入口。
                </p>
                <Link to="/" style={{ ...S.btnSm, textDecoration: 'none' }}>返回主机列表</Link>
            </div>

                {/* 周期性深度采样计划列表 */}
            <div style={S.card}>
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 12 }}>
                    <h3 style={{ margin: 0 }}>周期性深度采样计划</h3>
                    <button style={S.btnSm} onClick={refreshSchedules}>🔄 刷新</button>
                </div>
                {schLoading ? <p style={S.loading}>加载中...</p>
                    : schList.length === 0 ? (
                        <div style={{ textAlign: 'center', padding: 20, color: '#999' }}>
                            <p>暂无周期性深度采样计划</p>
                            <p style={S.hint}>在主页新建任务时勾选“周期性深度采样”即可创建</p>
                        </div>
                    ) : (
                        schList.map(sch => (
                            <div key={sch.sid} style={S.schRow(masterTid === sch.sid)}
                                onClick={() => loadTimeline(sch.sid)}>
                                <div style={{ flex: 1 }}>
                                    <strong>{sch.name}</strong>
                                    <span style={{ marginLeft: 10, fontSize: 12, color: '#888' }}>{sch.cron_expr}</span>
                                    <span style={{ marginLeft: 10, fontSize: 12, color: '#667085' }}>{sch.user_name || '系统'}</span>
                                    <span style={{
                                        marginLeft: 10, padding: '2px 6px', borderRadius: 8, fontSize: 11,
                                        background: sch.enabled ? '#e8f5e9' : '#f5f5f5', color: sch.enabled ? '#4caf50' : '#999'
                                    }}>
                                        {sch.enabled ? '启用' : '停用'}
                                    </span>
                                </div>
                                <div style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
                                    <span style={{ fontSize: 11, color: '#888', marginRight: 8 }}>
                                        SID: {sch.sid}
                                    </span>
                                    {sch.can_manage && <button style={{ ...S.btnSm, background: sch.enabled ? '#ff9800' : '#4caf50', color: '#fff' }}
                                        onClick={(e) => toggleSchedule(sch.sid, e)}>
                                        {sch.enabled ? '⏸ 停止' : '▶ 启用'}
                                    </button>}
                                    {sch.can_manage && <button style={{ ...S.btnSm, background: '#f44336', color: '#fff' }}
                                        onClick={(e) => deleteSchedule(sch.sid, e)}>
                                        🗑 删除
                                    </button>}
                                </div>
                            </div>
                        ))
                    )}
            </div>

            {/* 时间范围查询 */}
            {masterTid && (
                <div style={S.card}>
                    <h3 style={{ marginTop: 0 }}>🔍 时间范围查询</h3>
                    <p style={S.hint}>
                        选择一段时间，查看该区间内触发的全部采集窗口；时间按浏览器本地时区显示：{browserTimeZoneLabel()}。
                    </p>
                    <div style={{ display: 'flex', alignItems: 'center', flexWrap: 'wrap', marginBottom: 12 }}>
                        <button style={S.rangeBtn(activeRange === 1)} onClick={() => loadQuickRange(1)}>最近 1 小时</button>
                        <button style={S.rangeBtn(activeRange === 24)} onClick={() => loadQuickRange(24)}>最近 24 小时</button>
                        <button style={S.rangeBtn(activeRange === 168)} onClick={() => loadQuickRange(168)}>最近 7 天</button>
                        <button style={S.rangeBtn(showCustom || activeRange === -1)} onClick={() => setShowCustom(v => !v)}>
                            自定义区间 {showCustom ? '▲' : '▼'}
                        </button>
                        <button style={S.btnSm} onClick={() => loadTimeline(masterTid)}>查看全部窗口</button>
                    </div>

                    {showCustom && (
                        <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap', marginBottom: 12 }}>
                            <input
                                type="datetime-local"
                                style={{ ...S.input, width: 220, marginBottom: 0 }}
                                value={fromInput}
                                onChange={e => setFromInput(e.target.value)}
                            />
                            <span style={{ color: '#888' }}>→</span>
                            <input
                                type="datetime-local"
                                style={{ ...S.input, width: 220, marginBottom: 0 }}
                                value={toInput}
                                onChange={e => setToInput(e.target.value)}
                            />
                            <button style={S.btn} onClick={loadCustomRange} disabled={!fromInput || !toInput}>查询区间</button>
                        </div>
                    )}

                    <div style={{ borderTop: '1px solid #eee', paddingTop: 12, display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                        <span style={{ ...S.label, marginBottom: 0 }}>筛选</span>
                        <select style={{ ...S.input, width: 130, marginBottom: 0 }} value={statusFilter} onChange={e => setStatusFilter(e.target.value)}>
                            <option value="">全部状态</option>
                            <option value="0">待处理</option>
                            <option value="1">执行中</option>
                            <option value="2">已完成</option>
                            <option value="3">失败</option>
                            <option value="4">上传中</option>
                            <option value="5">已取消</option>
                        </select>
                        <input
                            style={{ ...S.input, width: 180, marginBottom: 0 }}
                            value={kindFilter}
                            onChange={e => setKindFilter(e.target.value)}
                            placeholder="task_kind"
                        />
                        <select style={{ ...S.input, width: 130, marginBottom: 0 }} value={hasResultFilter} onChange={e => setHasResultFilter(e.target.value)}>
                            <option value="">全部结果</option>
                            <option value="true">有结果</option>
                            <option value="false">无结果</option>
                        </select>
                        <button style={S.btnSm} onClick={() => {
                            if (queryMode === 'at') loadAt();
                            else if (queryMode === 'range' && rangeFrom && rangeTo) loadRange(rangeFrom, rangeTo);
                            else loadTimeline(masterTid);
                        }}>应用筛选</button>
                    </div>

                    <div style={{ borderTop: '1px solid #eee', paddingTop: 12, display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                        <span style={{ ...S.label, marginBottom: 0 }}>🕐 按时刻回溯</span>
                        <input
                            type="datetime-local"
                            style={{ ...S.input, width: 220, marginBottom: 0 }}
                            value={atInput}
                            onChange={e => setAtInput(e.target.value)}
                        />
                        <select
                            style={{ ...S.input, marginBottom: 0 }}
                            value={spanInput}
                            onChange={e => setSpanInput(e.target.value)}
                        >
                            <option value="10m">前后 ±10 分钟</option>
                            <option value="30m">前后 ±30 分钟</option>
                            <option value="2h">前后 ±2 小时</option>
                        </select>
                        <button style={S.btn} onClick={loadAt} disabled={!atInput}>回溯到该时刻</button>
                        <div style={{ ...S.hint, flexBasis: '100%' }}>
                            返回该时刻前后区间内的全部采集窗口；当时正在生效的那个会加粗边框并标注「当时生效」。
                        </div>
                    </div>
                </div>
            )}

            {/* 时间轴 */}
            {loading && <div style={S.loading}>⏳ 加载时间轴...</div>}
            {error && <div style={{ ...S.loading, color: '#f44' }}>❌ {error}</div>}

            {!loading && points.length > 0 && (
                <div style={S.card}>
                    <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', flexWrap: 'wrap', gap: 8 }}>
                        <h3 style={{ margin: '0 0 4px' }}>
                            {queryMode === 'at' ? `回溯结果 (${points.length} 个窗口)`
                                : queryMode === 'range' ? `区间结果 (${points.length} 个窗口)`
                                    : `历史采集 (${points.length} 个窗口)`} — {masterTid}
                        </h3>
                        <span style={S.hint}>滚轮缩放 · 拖动平移 · 点击色块查看详情</span>
                    </div>

                    <TimelineChart points={points} />

                    {trends && (
                        <div style={{ display: 'flex', gap: 10, flexWrap: 'wrap', margin: '12px 0 4px' }}>
                            <span style={S.btnSm}>窗口 {trends.total || 0}</span>
                            <span style={S.btnSm}>成功 {trends.success || 0}</span>
                            <span style={S.btnSm}>失败 {trends.failed || 0}</span>
                            <span style={S.btnSm}>进行中 {trends.running || 0}</span>
                            <span style={S.btnSm}>有结果 {trends.has_result || 0}</span>
                        </div>
                    )}

                    <div style={{ ...S.timeline, marginTop: 20 }}>
                        {points.map((p, i) => (
                            <div key={p.tid} style={S.point(p.status, p.is_effective)}>
                                <div style={S.dot(p.status)} />
                                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                                    <div>
                                        <strong>{i + 1}. {p.name || p.tid}</strong>
                                        <span style={{
                                            marginLeft: 10, padding: '2px 8px', borderRadius: 10, fontSize: 11, fontWeight: 'bold',
                                            background: statusColor(p.status),
                                            color: '#fff'
                                        }}>{ST[p.status] || '未知'}</span>
                                        {p.is_effective && <span style={{
                                            marginLeft: 6, padding: '2px 8px', borderRadius: 10, fontSize: 11, fontWeight: 'bold',
                                            background: '#4a6cf7', color: '#fff'
                                        }}>当时生效</span>}
                                        {p.task_kind && <span style={{ marginLeft: 6, fontSize: 11, color: '#666' }}>{p.task_kind}</span>}
                                        {p.has_result && <span style={{ marginLeft: 6, fontSize: 11, color: '#4caf50' }}>✅ 有结果</span>}
                                    </div>
                                    <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
                                        {/* 只有分析完成的窗口才有 top.json，没有产物的窗口不给对比入口 */}
                                        {p.has_result && (baselineTid === p.tid ? (
                                            <button style={{ ...S.btnSm, background: '#4a6cf7', color: '#fff', marginRight: 0 }}
                                                onClick={() => setBaselineTid('')}>
                                                ★ 基线（点击取消）
                                            </button>
                                        ) : baselineTid ? (
                                            <Link to={`/task/diff?baseline=${baselineTid}&compare=${p.tid}`}
                                                style={{ ...S.btnSm, marginRight: 0, textDecoration: 'none', background: '#e8f0ff', color: '#4a6cf7' }}>
                                                与基线对比
                                            </Link>
                                        ) : (
                                            <button style={{ ...S.btnSm, marginRight: 0 }}
                                                onClick={() => setBaselineTid(p.tid)}>
                                                设为基线
                                            </button>
                                        ))}
                                        <Link to={p.result_url || `/task/result?tid=${p.tid}`}
                                            style={{ color: '#4a6cf7', fontSize: 13, fontWeight: 'bold', textDecoration: 'none' }}>
                                            查看详情 →
                                        </Link>
                                    </div>
                                </div>
                                <div style={{ fontSize: 11, color: '#999', marginTop: 4 }}>
                                    窗口 {formatDateTime(p.window_start || p.create_time)}
                                    {' → '}
                                    {p.window_end
                                        ? formatDateTime(p.window_end)
                                        : (p.end_time ? formatDateTime(p.end_time) : '进行中')}
                                    {p.frequency_hz ? ` · ${p.frequency_hz}Hz` : ''}
                                    {p.duration_seconds ? ` · ${p.duration_seconds}s` : ''}
                                    {p.scheduled_at ? ` · 触发 ${formatDateTime(p.scheduled_at)}` : ''}
                                </div>
                            </div>
                        ))}
                    </div>
                </div>
            )}

            {!loading && masterTid && points.length === 0 && !error && (
                <div style={{ ...S.loading, color: '#888' }}>
                    {queryMode === 'range' ? '该时间区间内没有采集窗口'
                        : queryMode === 'at' ? '该时刻之前没有已触发的采集窗口'
                            : '该定时任务暂无子任务记录'}
                </div>
            )}
        </div>
    );
}
