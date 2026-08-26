// ============================================================
// components/ScheduleTimeline.js — 单个周期任务的时间轴工作区
// ============================================================
// 由 ScheduleDetailPage 使用，输入 sid（周期任务 SID），渲染：
//   - 时间范围查询：全部窗口 / 快捷区间 / 自定义区间
//   - 状态 / task_kind / 结果筛选
//   - 时间轴图 + 趋势统计
//   - 窗口列表：查看、取消、设为基线 / 与基线对比（内嵌 Diff 面板）
// 视觉与持续采集列表、周期任务列表统一：白色轻边框卡片、浅灰表头、
// 圆角状态标签、蓝色文字操作、红色危险操作、横向滚动保留。
// ============================================================

import React, { useCallback, useEffect, useState } from 'react';
import { Link } from 'react-router-dom';
import { tasks } from '../api';
import TimelineChart, { statusColor } from './TimelineChart';
import TaskCancelButton from './TaskCancelButton';
import InlineDiffPanel from './InlineDiffPanel';
import Pagination from './Pagination';
import { browserTimeZoneLabel, formatDateTime, localDateTimeToISO } from '../utils/time';

const S = {
    card: { minWidth: 0, maxWidth: '100%', background: '#fff', borderRadius: 8, padding: 20, marginBottom: 16, border: '1px solid #e5e7eb', boxShadow: '0 1px 2px rgba(16,24,40,.04)' },
    btn: { background: '#315efb', color: '#fff', border: 'none', padding: '7px 14px', borderRadius: 6, cursor: 'pointer', fontSize: 13, fontWeight: 700 },
    btnSm: { background: '#f8fafc', color: '#475467', border: '1px solid #d0d7de', padding: '5px 10px', borderRadius: 6, cursor: 'pointer', fontSize: 12, fontWeight: 700, textDecoration: 'none' },
    input: { padding: '7px 10px', border: '1px solid #d0d7de', borderRadius: 6, fontSize: 13, boxSizing: 'border-box' },
    rangeBtn: (active) => ({
        background: active ? '#315efb' : '#f8fafc', color: active ? '#fff' : '#475467',
        border: active ? '1px solid #315efb' : '1px solid #d0d7de',
        padding: '6px 14px', borderRadius: 6, cursor: 'pointer', fontSize: 13, fontWeight: 700,
    }),
    loading: { textAlign: 'center', padding: 40, color: '#667085' },
    label: { display: 'block', marginBottom: 8, fontWeight: 700, fontSize: 14, color: '#475467' },
    hint: { fontSize: 12, color: '#667085', marginTop: 4 },
    tableWrap: { width: '100%', minWidth: 0, maxWidth: '100%', overflowX: 'auto', overflowY: 'hidden' },
    table: { width: '100%', borderCollapse: 'collapse', minWidth: 680 },
    th: { textAlign: 'left', padding: '10px 12px', borderBottom: '1px solid #d0d5dd', color: '#475467', background: '#f8fafc', fontSize: 12, whiteSpace: 'nowrap' },
    td: { padding: '11px 12px', borderBottom: '1px solid #edf0f3', color: '#344054', fontSize: 13, verticalAlign: 'top' },
    rowName: { color: '#101828', fontWeight: 700, marginBottom: 3 },
    badge: { display: 'inline-flex', borderRadius: 999, padding: '3px 8px', fontSize: 12, fontWeight: 700, whiteSpace: 'nowrap' },
    link: { color: '#315efb', fontWeight: 700, textDecoration: 'none', marginRight: 10, whiteSpace: 'nowrap' },
    baselineBtn: { color: '#315efb', background: 'transparent', border: 0, padding: 0, fontWeight: 700, cursor: 'pointer', fontSize: 13, marginRight: 10, whiteSpace: 'nowrap' },
    baselineActiveBtn: { color: '#315efb', background: '#eef2ff', border: '1px solid #c7d2fe', borderRadius: 6, padding: '3px 9px', fontWeight: 700, cursor: 'pointer', fontSize: 12, whiteSpace: 'nowrap' },
    error: { background: '#fff3f3', border: '1px solid #ffcdd2', color: '#b42318', borderRadius: 8, padding: 12, marginBottom: 16 },
    empty: { textAlign: 'center', color: '#667085', padding: 38, border: '1px dashed #d0d5dd', borderRadius: 8 },
};

const ST = { 0: '待处理', 1: '执行中', 2: '已完成', 3: '失败', 4: '上传中', 5: '已取消' };
const isActiveTask = (status) => status === 0 || status === 1 || status === 4;
// 时间轴窗口列表每页条数
const PAGE_SIZE = 20;

export default function ScheduleTimeline({ sid }) {
    const [points, setPoints] = useState([]);
    const [loading, setLoading] = useState(false);
    const [error, setError] = useState('');
    const [baselineTid, setBaselineTid] = useState('');
    const [queryMode, setQueryMode] = useState('list'); // 'list' 全部窗口 | 'range' 时间区间
    const [fromInput, setFromInput] = useState('');
    const [toInput, setToInput] = useState('');
    const [showCustom, setShowCustom] = useState(false);
    const [activeRange, setActiveRange] = useState(0);   // -1 自定义；0 未选中；>0 最近 N 小时
    const [rangeFrom, setRangeFrom] = useState('');
    const [rangeTo, setRangeTo] = useState('');
    const [statusFilter, setStatusFilter] = useState('');
    const [kindFilter, setKindFilter] = useState('');
    const [hasResultFilter, setHasResultFilter] = useState('');
    const [trends, setTrends] = useState(null);
    const [diffCompareTid, setDiffCompareTid] = useState('');
    const [page, setPage] = useState(1); // 时间轴窗口列表当前页

    const timelineFilters = useCallback(() => ({
        status: statusFilter || undefined,
        task_kind: kindFilter || undefined,
        has_result: hasResultFilter || undefined,
    }), [statusFilter, kindFilter, hasResultFilter]);

    const loadTimeline = useCallback(async (targetSid, { resetMode = true, silent = false } = {}) => {
        if (!targetSid) return;
        if (resetMode) { setQueryMode('list'); setActiveRange(0); }
        // 后台轮询静默刷新：不翻 loading，避免整个时间轴（图 + 窗口表）每轮询塌缩成"加载中"再重建
        if (!silent) { setLoading(true); setPage(1); }
        setError('');
        try {
            const r = await tasks.timeline(targetSid, timelineFilters());
            if (r.code === 0) { setPoints(r.data?.points || []); setTrends(r.data?.trends || null); }
            else if (!silent) setError(r.message || '查询失败');
        } catch (e) { if (!silent) setError('请求失败: ' + (e.message || '')); }
        finally { if (!silent) setLoading(false); }
    }, [timelineFilters]);

    // SID 变化（在详情页间切换）时重置并加载
    useEffect(() => {
        setBaselineTid('');
        setDiffCompareTid('');
        setStatusFilter('');
        setKindFilter('');
        setHasResultFilter('');
        setPoints([]);
        if (sid) loadTimeline(sid);
    }, [sid]);

    // 区间查询：返回 [from, to] 内触发的全部采集窗口
    const loadRange = useCallback(async (fromISO, toISO, silent = false) => {
        if (!sid) return;
        setQueryMode('range');
        setRangeFrom(fromISO); setRangeTo(toISO);
        if (!silent) { setLoading(true); setPage(1); }
        setError('');
        try {
            const r = await tasks.timeline(sid, { from: fromISO, to: toISO, ...timelineFilters() });
            if (r.code === 0) { setPoints(r.data?.points || []); setTrends(r.data?.trends || null); }
            else if (!silent) setError(r.message || '查询失败');
        } catch (e) { if (!silent) setError('请求失败: ' + (e.message || '')); }
        finally { if (!silent) setLoading(false); }
    }, [sid, timelineFilters]);

    const loadQuickRange = useCallback((hours) => {
        const to = new Date();
        const from = new Date(to.getTime() - hours * 3600 * 1000);
        setActiveRange(hours);
        setShowCustom(false);
        loadRange(from.toISOString(), to.toISOString());
    }, [loadRange]);

    const loadCustomRange = useCallback(() => {
        if (!fromInput || !toInput) { setError('请选择自定义开始时间和结束时间后再查询'); return; }
        const fromISO = localDateTimeToISO(fromInput);
        const toISO = localDateTimeToISO(toInput);
        if (!fromISO || !toISO) { setError('自定义时间格式无效'); return; }
        if (new Date(toISO) < new Date(fromISO)) { setError('结束时间不能早于开始时间'); return; }
        setActiveRange(-1);
        loadRange(fromISO, toISO);
    }, [fromInput, toInput, loadRange]);

    const reloadCurrent = useCallback(() => {
        // 用户显式触发（应用筛选 / 停止后刷新）：非静默，展示 loading
        if (queryMode === 'range') { if (rangeFrom && rangeTo) loadRange(rangeFrom, rangeTo); }
        else loadTimeline(sid, { resetMode: false });
    }, [queryMode, rangeFrom, rangeTo, loadRange, loadTimeline, sid]);

    // 自动轮询：有运行中窗口时沿用当前查询模式静默刷新（不塌缩内容）
    useEffect(() => {
        const hasRunning = points.some(p => isActiveTask(p.status));
        if (!hasRunning || !sid) return undefined;
        const iv = setInterval(() => {
            if (queryMode === 'range') { if (rangeFrom && rangeTo) loadRange(rangeFrom, rangeTo, true); }
            else loadTimeline(sid, { resetMode: false, silent: true });
        }, 5000);
        return () => clearInterval(iv);
    }, [points, sid, queryMode, rangeFrom, rangeTo, loadTimeline, loadRange]);

    // 窗口数据变化后钳制页码（如静默刷新后总窗口数减少、当前页越界时回到最后一页）
    useEffect(() => {
        const totalPages = Math.max(1, Math.ceil(points.length / PAGE_SIZE));
        if (page > totalPages) setPage(totalPages);
    }, [points, page]);

    // 按时间倒序展示（最新窗口排最上面），序号沿用原始时间顺序（最早=1），再按页切分
    const displayPoints = points.map((p, i) => ({ ...p, _seq: i + 1 })).reverse();
    const totalPages = Math.max(1, Math.ceil(displayPoints.length / PAGE_SIZE));
    const pagePoints = displayPoints.slice((page - 1) * PAGE_SIZE, page * PAGE_SIZE);

    return (
        <>
            {/* 时间范围查询 */}
            <section style={S.card}>
                <h3 style={{ marginTop: 0, marginBottom: 4 }}>🔍 时间范围查询</h3>
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
                    <button style={S.btnSm} onClick={() => loadTimeline(sid)}>查看全部窗口</button>
                </div>

                {showCustom && (
                    <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap', marginBottom: 12 }}>
                        <input type="datetime-local" style={{ ...S.input, width: 220 }} value={fromInput} onChange={e => setFromInput(e.target.value)} aria-label="自定义开始时间" />
                        <span style={{ color: '#667085' }}>→</span>
                        <input type="datetime-local" style={{ ...S.input, width: 220 }} value={toInput} onChange={e => setToInput(e.target.value)} aria-label="自定义结束时间" />
                        <button style={S.btn} onClick={loadCustomRange} disabled={!fromInput || !toInput}>查询区间</button>
                    </div>
                )}

                <div style={{ borderTop: '1px solid #edf0f3', paddingTop: 12, display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                    <span style={{ ...S.label, marginBottom: 0 }}>筛选</span>
                    <select style={{ ...S.input, width: 130 }} value={statusFilter} onChange={e => setStatusFilter(e.target.value)} aria-label="窗口状态筛选">
                        <option value="">全部状态</option>
                        <option value="0">待处理</option>
                        <option value="1">执行中</option>
                        <option value="2">已完成</option>
                        <option value="3">失败</option>
                        <option value="4">上传中</option>
                        <option value="5">已取消</option>
                    </select>
                    <input style={{ ...S.input, width: 180 }} value={kindFilter} onChange={e => setKindFilter(e.target.value)} placeholder="task_kind" aria-label="采集器筛选" />
                    <select style={{ ...S.input, width: 130 }} value={hasResultFilter} onChange={e => setHasResultFilter(e.target.value)} aria-label="结果筛选">
                        <option value="">全部结果</option>
                        <option value="true">有结果</option>
                        <option value="false">无结果</option>
                    </select>
                    <button style={S.btnSm} onClick={reloadCurrent}>应用筛选</button>
                </div>
            </section>

            {/* 时间轴 */}
            {loading && <div style={S.loading}>⏳ 加载时间轴...</div>}
            {error && <div style={{ ...S.loading, color: '#b42318' }}>❌ {error}</div>}

            {!loading && points.length > 0 && (
                <section style={S.card}>
                    <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', flexWrap: 'wrap', gap: 8 }}>
                        <h3 style={{ margin: '0 0 4px' }}>
                            {queryMode === 'range' ? `区间结果 (${points.length} 个窗口)`
                                : `历史采集 (${points.length} 个窗口)`} — {sid}
                        </h3>
                        <span style={S.hint}>滚轮缩放 · 拖动平移 · 点击色块查看详情</span>
                    </div>

                    <TimelineChart points={points} />

                    {trends && (
                        <div style={{ display: 'flex', gap: 10, flexWrap: 'wrap', margin: '12px 0 4px' }}>
                            <span style={S.btnSm}>窗口 {trends.total || 0}</span>
                            <span style={S.btnSm}>成功 {trends.success || 0}</span>
                            <span style={S.btnSm}>失败 {trends.failed || 0}</span>
                            <span style={S.btnSm}>已取消 {trends.canceled || 0}</span>
                            <span style={S.btnSm}>进行中 {trends.running || 0}</span>
                            <span style={S.btnSm}>有结果 {trends.has_result || 0}</span>
                        </div>
                    )}

                    {/* 按时间倒序展示：最新窗口排最上面；序号沿用原始时间顺序（最早=1） */}
                    <div className="table-scroll" style={{ ...S.tableWrap, marginTop: 20 }}>
                        <table style={S.table}>
                            <thead>
                                <tr>
                                    <th style={S.th}>窗口</th>
                                    <th style={S.th}>状态</th>
                                    <th style={S.th}>采样参数</th>
                                    <th style={S.th}>窗口时间</th>
                                    <th style={S.th}>操作</th>
                                </tr>
                            </thead>
                            <tbody>
                                {pagePoints.map(p => (
                                    <React.Fragment key={p.tid}>
                                        <tr>
                                            <td style={S.td}>
                                                <div style={S.rowName}>{p._seq}. {p.name || p.tid}</div>
                                                <span style={{ color: '#667085', fontSize: 12 }}>{p.tid}</span>
                                            </td>
                                            <td style={S.td}>
                                                <span style={{ ...S.badge, background: statusColor(p.status), color: '#fff' }}>{ST[p.status] || '未知'}</span>
                                            </td>
                                            <td style={S.td}>
                                                {p.frequency_hz ? `${p.frequency_hz}Hz` : '-'}
                                                {p.duration_seconds ? ` · ${p.duration_seconds}s` : ''}
                                            </td>
                                            <td style={S.td}>
                                                窗口 {formatDateTime(p.window_start || p.create_time)}
                                                {' → '}
                                                {p.window_end ? formatDateTime(p.window_end) : (p.end_time ? formatDateTime(p.end_time) : '进行中')}
                                                {p.scheduled_at && <div style={{ color: '#667085', fontSize: 11, marginTop: 4 }}>触发 {formatDateTime(p.scheduled_at)}</div>}
                                            </td>
                                            <td style={S.td}>
                                                <Link style={S.link} to={p.result_url || `/task/result?tid=${p.tid}`}>查看</Link>
                                                <TaskCancelButton
                                                    tid={p.tid}
                                                    status={p.status}
                                                    canManage={p.can_manage}
                                                    onCancelled={reloadCurrent}
                                                    style={{ marginRight: 10 }}
                                                />
                                                {p.has_result && (baselineTid === p.tid ? (
                                                    <button style={S.baselineActiveBtn} onClick={() => { setBaselineTid(''); setDiffCompareTid(''); }}>
                                                        ★ 基线（点击取消）
                                                    </button>
                                                ) : baselineTid ? (
                                                    <button style={S.baselineActiveBtn}
                                                        onClick={() => setDiffCompareTid(v => (v === p.tid ? '' : p.tid))}>
                                                        {diffCompareTid === p.tid ? '收起对比' : '与基线对比'}
                                                    </button>
                                                ) : (
                                                    <button style={S.baselineBtn} onClick={() => setBaselineTid(p.tid)}>
                                                        设为基线
                                                    </button>
                                                ))}
                                            </td>
                                        </tr>
                                        {diffCompareTid === p.tid && baselineTid && baselineTid !== p.tid && (
                                            <tr>
                                                <td colSpan={5} style={{ padding: '0 0 16px', borderBottom: '1px solid #edf0f3' }}>
                                                    <InlineDiffPanel
                                                        baselineTid={baselineTid}
                                                        compareTid={p.tid}
                                                        onClose={() => setDiffCompareTid('')}
                                                    />
                                                </td>
                                            </tr>
                                        )}
                                    </React.Fragment>
                                ))}
                            </tbody>
                        </table>
                    </div>

                    <Pagination page={page} totalPages={totalPages} onPageChange={setPage} />
                </section>
            )}

            {!loading && sid && points.length === 0 && !error && (
                <div style={S.empty}>
                    {queryMode === 'range' ? '该时间区间内没有采集窗口'
                        : '该周期任务暂无采集窗口记录'}
                </div>
            )}
        </>
    );
}
