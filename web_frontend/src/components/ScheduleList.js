// ============================================================
// components/ScheduleList.js — 可复用周期任务（周期性深度采样计划）列表
// ============================================================
// 主机周期 Tab 与 /timeline 旧兼容入口共用同一套全局周期任务列表。
// 列表显示：名称与短 SID / 目标与采集器 / 启停状态 / 执行周期 / 最近运行 /
// 下次运行 / 创建者 / 操作（查看、启停、删除）。
// 点击名称或"查看"进入独立时间轴详情页（由 detailPrefix 决定路由前缀）。
// compact 模式用于概览卡片：只显示前 limit 条，隐藏搜索/筛选/分页。
// ============================================================

import React, { useCallback, useEffect, useRef, useState } from 'react';
import { Link } from 'react-router-dom';
import { schedules } from '../api';
import Pagination from './Pagination';
import { collectorLabelFromTask } from '../utils/collectors';
import { schedulePeriodLabel, schedulePeriodTitle, scheduleStatusText } from '../utils/schedule';
import { formatDateTime } from '../utils/time';
import InfoTooltip from './InfoTooltip';

const S = {
    card: { minWidth: 0, maxWidth: '100%', background: '#fff', borderRadius: 8, padding: 20, border: '1px solid #e5e7eb', boxShadow: '0 1px 2px rgba(16,24,40,.04)' },
    head: { display: 'flex', justifyContent: 'space-between', gap: 12, alignItems: 'center', flexWrap: 'wrap', marginBottom: 14 },
    title: { margin: 0, fontSize: 18, color: '#101828' },
    subtle: { color: '#667085', fontSize: 12 },
    toolbar: { display: 'flex', gap: 8, flexWrap: 'wrap', alignItems: 'center', marginBottom: 14 },
    input: { minWidth: 220, flex: '1 1 240px', height: 36, padding: '7px 10px', boxSizing: 'border-box', border: '1px solid #d0d5dd', borderRadius: 6, fontSize: 13 },
    select: { height: 36, padding: '7px 10px', border: '1px solid #d0d5dd', borderRadius: 6, background: '#fff', fontSize: 13 },
    tableWrap: { width: '100%', minWidth: 0, maxWidth: '100%', overflowX: 'auto', overflowY: 'hidden' },
    table: { width: '100%', borderCollapse: 'collapse', minWidth: 920 },
    th: { textAlign: 'left', padding: '10px 12px', borderBottom: '1px solid #d0d5dd', color: '#475467', background: '#f8fafc', fontSize: 12, whiteSpace: 'nowrap' },
    td: { padding: '11px 12px', borderBottom: '1px solid #edf0f3', color: '#344054', fontSize: 13, verticalAlign: 'top' },
    name: { color: '#101828', fontWeight: 700, marginBottom: 3 },
    mono: { maxWidth: 300, color: '#475467', fontSize: 12, fontFamily: 'ui-monospace,SFMono-Regular,Menlo,monospace', wordBreak: 'break-all' },
    badge: { display: 'inline-flex', borderRadius: 999, padding: '3px 8px', fontSize: 12, fontWeight: 700, whiteSpace: 'nowrap' },
    link: { color: '#315efb', fontWeight: 700, textDecoration: 'none', marginRight: 10, whiteSpace: 'nowrap' },
    dangerLink: { color: '#b42318', background: 'transparent', border: 0, padding: 0, fontWeight: 700, cursor: 'pointer', whiteSpace: 'nowrap' },
    cron: { display: 'inline-flex', background: '#f2f4f7', color: '#475467', borderRadius: 6, padding: '2px 7px', fontSize: 12, fontFamily: 'ui-monospace,SFMono-Regular,Menlo,monospace' },
    empty: { textAlign: 'center', color: '#667085', padding: 38, border: '1px dashed #d0d5dd', borderRadius: 8 },
    error: { marginBottom: 14, color: '#b42318', background: '#fff6f5', border: '1px solid #fda29b', borderRadius: 6, padding: 10, fontSize: 13 },
};

export default function ScheduleList({ targetIp, detailPrefix, compact = false, limit = 5, bare = false, onChanged }) {
    const [list, setList] = useState([]);
    const [total, setTotal] = useState(0);
    const [page, setPage] = useState(1);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState('');
    const [keyword, setKeyword] = useState('');
    const [enabled, setEnabled] = useState('');
    // 完整列表和主机概览都默认展示全部创建者的计划。
    const [ownerFilter, setOwnerFilter] = useState('all');
    const [busySid, setBusySid] = useState('');
    const requestSequence = useRef(0);

    const pageSize = compact ? Math.max(1, limit) : 10;
    const totalPages = Math.max(1, Math.ceil(total / pageSize));

    const load = useCallback(async (silent = false) => {
        const requestID = ++requestSequence.current;
        if (!silent) setLoading(true);
        setError('');
        try {
            const response = await schedules.list({
                target_ip: targetIp || undefined,
                page,
                page_size: pageSize,
                keyword: keyword.trim() || undefined,
                enabled: enabled || undefined,
                owner_filter: ownerFilter,
            });
            if (requestID !== requestSequence.current) return;
            if (response.code !== 0) throw new Error(response.message || '加载周期任务失败');
            setList(response.data?.schedules || []);
            setTotal(Number(response.data?.total ?? (response.data?.schedules || []).length));
        } catch (err) {
            if (requestID === requestSequence.current) setError(err?.message || '加载周期任务失败');
        } finally {
            if (!silent && requestID === requestSequence.current) setLoading(false);
        }
    }, [targetIp, page, pageSize, keyword, enabled, ownerFilter]);

    useEffect(() => { load(); }, [load]);
    useEffect(() => {
        if (compact) return undefined;
        const timer = window.setInterval(() => load(true), 10000);
        return () => window.clearInterval(timer);
    }, [load, compact]);

    const toggle = async (sch) => {
        setBusySid(sch.sid);
        setError('');
        try {
            const response = await schedules.toggle(sch.sid);
            if (response.code !== 0) throw new Error(response.message || '切换状态失败');
            onChanged && onChanged();
            await load(true);
        } catch (err) {
            setError(err?.message || '切换状态失败');
        } finally {
            setBusySid('');
        }
    };

    const remove = async (sch) => {
        if (!window.confirm(`确定删除周期任务「${sch.name}」？相关历史采集窗口保留。`)) return;
        setBusySid(sch.sid);
        setError('');
        try {
            const response = await schedules.delete(sch.sid);
            if (response.code !== 0) throw new Error(response.message || '删除失败');
            onChanged && onChanged();
            await load(true);
        } catch (err) {
            setError(err?.message || '删除失败');
        } finally {
            setBusySid('');
        }
    };

    const detailURL = (sid) => `${detailPrefix}/${encodeURIComponent(sid)}`;

    const content = (
        <>
            {error && <div style={S.error}>{error}</div>}

            {!compact && (
                <div style={S.toolbar}>
                    <input
                        style={S.input}
                        value={keyword}
                        onChange={e => { setKeyword(e.target.value); setPage(1); }}
                        placeholder="搜索名称 / SID / 目标 IP"
                        aria-label="周期任务搜索"
                    />
                    <select style={S.select} value={enabled} onChange={e => { setEnabled(e.target.value); setPage(1); }} aria-label="周期任务启停筛选">
                        <option value="">全部状态</option>
                        <option value="true">启用</option>
                        <option value="false">停用</option>
                    </select>
                    <select style={S.select} value={ownerFilter} onChange={e => { setOwnerFilter(e.target.value); setPage(1); }} aria-label="周期任务归属筛选">
                        <option value="all">全部创建者</option>
                        <option value="mine">我创建的</option>
                    </select>
                </div>
            )}

            {loading && list.length === 0 ? (
                <div style={S.empty}>正在加载周期任务...</div>
            ) : list.length === 0 ? (
                <div style={S.empty}>
                    <div style={{ fontWeight: 700, color: '#475467', marginBottom: 6 }}>暂无周期任务</div>
                    <div style={S.subtle}>可在“新建单次采样”中勾选周期性深度采样来创建周期计划</div>
                </div>
            ) : (
                <div className="table-scroll" style={S.tableWrap}>
                    <table style={S.table}>
                        <thead>
                            <tr>
                                <th style={S.th}>名称 / SID</th>
                                <th style={S.th}>目标与采集器</th>
                                <th style={S.th}>状态</th>
                                <th style={S.th}>执行周期</th>
                                <th style={S.th}>最近运行</th>
                                <th style={S.th}>下次运行</th>
                                <th style={S.th}>创建者</th>
                                <th style={S.th}>操作</th>
                            </tr>
                        </thead>
                        <tbody>
                            {list.map(sch => {
                                const running = sch.enabled === true;
                                return (
                                    <tr key={sch.sid}>
                                        <td style={S.td}>
                                            <Link to={detailURL(sch.sid)} style={S.name}>{sch.name}</Link>
                                            <div style={S.mono}>{shortSID(sch.sid)}</div>
                                        </td>
                                        <td style={S.td}>
                                            <strong>{sch.target_ip || '-'}</strong>
                                            <div style={S.subtle}>{collectorLabelFromTask({ task_kind: sch.task_kind, type: sch.task_type, profiler_type: sch.profiler_type, request_params: sch.request_params })}</div>
                                        </td>
                                        <td style={S.td}>
                                            <span style={{ ...S.badge, background: running ? '#16a34a' : '#64748b', color: '#fff' }} title={scheduleStatusText(sch)}>
                                                {running ? '启用' : '停用'}
                                                <InfoTooltip>{scheduleStatusText(sch)}</InfoTooltip>
                                            </span>
                                        </td>
                                        <td style={S.td}>
                                            <span style={S.cron} title={schedulePeriodTitle(sch)}>{schedulePeriodLabel(sch)}<InfoTooltip>{schedulePeriodTitle(sch)}</InfoTooltip></span>
                                        </td>
                                        <td style={S.td}>{formatDateTime(sch.last_run_at) || '-'}</td>
                                        <td style={S.td}>{running ? (formatDateTime(sch.next_run_at) || '-') : '已停用'}</td>
                                        <td style={S.td}>{sch.user_name || '系统'}</td>
                                        <td style={S.td}>
                                            <Link style={S.link} to={detailURL(sch.sid)}>查看</Link>
                                            {sch.can_manage && (
                                                <>
                                                    <button
                                                        style={{ ...S.link, background: 'transparent', border: 0, padding: 0, cursor: 'pointer' }}
                                                        disabled={busySid === sch.sid}
                                                        onClick={() => toggle(sch)}
                                                    >
                                                        {running ? '停用' : '启用'}
                                                    </button>
                                                    <button
                                                        style={S.dangerLink}
                                                        disabled={busySid === sch.sid}
                                                        onClick={() => remove(sch)}
                                                    >
                                                        删除
                                                    </button>
                                                </>
                                            )}
                                        </td>
                                    </tr>
                                );
                            })}
                        </tbody>
                    </table>
                </div>
            )}

            {!compact && total > pageSize && (
                <div style={{ display: 'flex', justifyContent: 'flex-end', alignItems: 'center', gap: 10, marginTop: 14 }}>
                    <Pagination page={page} totalPages={totalPages} onPageChange={setPage} />
                </div>
            )}
        </>
    );

    if (bare) return content;

    return (
        <section style={S.card}>
            <div style={S.head}>
                <div>
                    <h3 style={S.title}>周期任务</h3>
                    <span style={S.subtle}>由周期计划自动触发的深度采样窗口，点击计划进入独立时间轴</span>
                </div>
                <span style={S.subtle}>共 {total} 条</span>
            </div>
            {content}
        </section>
    );
}

function shortSID(value) {
    return String(value || '').length > 18 ? `${value.slice(0, 10)}...${value.slice(-4)}` : value;
}
