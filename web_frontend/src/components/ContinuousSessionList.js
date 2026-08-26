import React, { useCallback, useEffect, useRef, useState } from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import { continuous } from '../api';
import { continuousStateColor, continuousStateLabel, decodeJSONField, formatRelativeTime, selectorIdentity, selectorModeLabel, signalLabel } from '../utils/continuous';
import InfoTooltip from './InfoTooltip';

const S = {
    card: { minWidth: 0, maxWidth: '100%', background: '#fff', borderRadius: 8, padding: 20, border: '1px solid #e5e7eb', boxShadow: '0 1px 2px rgba(16,24,40,.04)' },
    head: { display: 'flex', justifyContent: 'space-between', gap: 12, alignItems: 'center', flexWrap: 'wrap', marginBottom: 14 },
    title: { margin: 0, fontSize: 18, color: '#101828' },
    subtle: { color: '#667085', fontSize: 12 },
    toolbar: { display: 'flex', justifyContent: 'space-between', gap: 10, flexWrap: 'wrap', marginBottom: 14 },
    filters: { display: 'flex', gap: 8, flexWrap: 'wrap', flex: '1 1 560px' },
    input: { minWidth: 220, flex: '1 1 240px', height: 36, padding: '7px 10px', boxSizing: 'border-box', border: '1px solid #d0d5dd', borderRadius: 6, fontSize: 13 },
    select: { height: 36, padding: '7px 10px', border: '1px solid #d0d5dd', borderRadius: 6, background: '#fff', fontSize: 13 },
    button: { height: 36, padding: '0 12px', color: '#315efb', background: '#fff', border: '1px solid #c7d2fe', borderRadius: 6, fontWeight: 700, cursor: 'pointer' },
    dangerButton: { height: 36, padding: '0 12px', color: '#b42318', background: '#fff', border: '1px solid #fda29b', borderRadius: 6, fontWeight: 700, cursor: 'pointer' },
    tableWrap: { width: '100%', minWidth: 0, maxWidth: '100%', overflowX: 'auto', overflowY: 'hidden' },
    table: { width: '100%', borderCollapse: 'collapse', minWidth: 920 },
    th: { textAlign: 'left', padding: '10px 12px', borderBottom: '1px solid #d0d5dd', color: '#475467', background: '#f8fafc', fontSize: 12, whiteSpace: 'nowrap' },
    td: { padding: '11px 12px', borderBottom: '1px solid #edf0f3', color: '#344054', fontSize: 13, verticalAlign: 'top' },
    name: { color: '#101828', fontWeight: 700, marginBottom: 3 },
    mono: { maxWidth: 300, color: '#475467', fontSize: 12, fontFamily: 'ui-monospace,SFMono-Regular,Menlo,monospace', wordBreak: 'break-all' },
    badge: { display: 'inline-flex', borderRadius: 999, padding: '3px 8px', fontSize: 12, fontWeight: 700, whiteSpace: 'nowrap' },
    chips: { display: 'flex', flexWrap: 'wrap', gap: 4, maxWidth: 220 },
    chip: { display: 'inline-flex', background: '#f2f4f7', color: '#475467', borderRadius: 999, padding: '2px 6px', fontSize: 11, fontWeight: 700 },
    link: { color: '#315efb', fontWeight: 700, textDecoration: 'none', marginRight: 10, whiteSpace: 'nowrap' },
    stop: { color: '#b42318', background: 'transparent', border: 0, padding: 0, fontWeight: 700, cursor: 'pointer', whiteSpace: 'nowrap' },
    warn: { marginBottom: 14, color: '#b54708', background: '#fffaeb', border: '1px solid #fedf89', borderRadius: 6, padding: 10, fontSize: 13 },
    error: { marginBottom: 14, color: '#b42318', background: '#fff6f5', border: '1px solid #fda29b', borderRadius: 6, padding: 10, fontSize: 13 },
    empty: { textAlign: 'center', color: '#667085', padding: 38, border: '1px dashed #d0d5dd', borderRadius: 8 },
    pagination: { display: 'flex', justifyContent: 'flex-end', alignItems: 'center', gap: 10, marginTop: 14 },
    pageButton: { height: 32, padding: '0 10px', color: '#315efb', background: '#fff', border: '1px solid #c7d2fe', borderRadius: 6, fontWeight: 700, cursor: 'pointer' },
    pageButtonDisabled: { color: '#98a2b3', background: '#f8fafc', border: '1px solid #e5e7eb', cursor: 'not-allowed' },
    jumpInput: { width: 52, height: 32, padding: '0 8px', border: '1px solid #d0d5dd', borderRadius: 6, background: '#fff', fontSize: 13, textAlign: 'center', boxSizing: 'border-box' },
};

const PAGE_SIZE = 20;

export default function ContinuousSessionList({ target, refreshToken = 0 }) {
    const [searchParams, setSearchParams] = useSearchParams();
    const [sessions, setSessions] = useState([]);
    const [total, setTotal] = useState(0);
    // 页码初始值从 URL 的 cpage 读取，刷新后仍停留在原页。
    const [page, setPage] = useState(() => {
        const raw = Number(searchParams.get('cpage'));
        return Number.isInteger(raw) && raw >= 1 ? raw : 1;
    });
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState('');
    const [keyword, setKeyword] = useState('');
    const [status, setStatus] = useState('');
    const [scope, setScope] = useState('');
    // 默认展示整台主机上的全部 Session；需要聚焦个人任务时可手动切换。
    const [ownerFilter, setOwnerFilter] = useState('all');
	const [showTestSessions, setShowTestSessions] = useState(() => searchParams.get('ctest') === '1');
    const [stopping, setStopping] = useState('');
    const [cleaning, setCleaning] = useState(false);
    const [jumpInput, setJumpInput] = useState('');
    const requestSequence = useRef(0);

    const load = useCallback(async (silent = false) => {
        const requestID = ++requestSequence.current;
        if (!silent) setLoading(true);
        setError('');
        try {
            const response = await continuous.sessions({
                target_ip: target.ip,
                page,
                page_size: PAGE_SIZE,
                owner_filter: ownerFilter,
				test_filter: showTestSessions ? 'all' : 'exclude',
                keyword: keyword.trim() || undefined,
                observed_state: status || undefined,
                scope: scope || undefined,
            });
            if (requestID !== requestSequence.current) return;
            if (response.code !== 0) throw new Error(response.message || '加载持续采集任务失败');
            const nextSessions = response.data?.sessions || [];
            const nextTotal = Number(response.data?.total ?? nextSessions.length);
            const lastPage = Math.max(1, Math.ceil(nextTotal / PAGE_SIZE));
            if (page > lastPage) {
                setPage(lastPage);
                return;
            }
            setSessions(nextSessions);
            setTotal(nextTotal);
        } catch (err) {
            if (requestID === requestSequence.current) setError(err?.message || '加载持续采集任务失败');
        } finally {
            if (!silent && requestID === requestSequence.current) setLoading(false);
        }
	}, [target.ip, ownerFilter, showTestSessions, keyword, status, scope, page]);

    useEffect(() => { load(); }, [load, refreshToken]);
    useEffect(() => {
        const timer = window.setInterval(() => load(true), 5000);
        return () => window.clearInterval(timer);
    }, [load]);

	// 页码和测试任务开关同步到 URL，刷新或重新进入页面时保持当前视图。
    useEffect(() => {
        const params = new URLSearchParams(searchParams);
		let changed = false;
		if (params.get('cpage') !== String(page)) { params.set('cpage', String(page)); changed = true; }
		if (showTestSessions) {
			if (params.get('ctest') !== '1') { params.set('ctest', '1'); changed = true; }
		} else if (params.has('ctest')) {
			params.delete('ctest'); changed = true;
		}
		if (changed) setSearchParams(params, { replace: true });
	}, [page, showTestSessions, searchParams, setSearchParams]);

    const stop = async session => {
        if (!window.confirm(`停止持续采集“${session.name}”？停止后不会自动恢复。`)) return;
        setStopping(session.sid);
        setError('');
        try {
            const response = await continuous.stopSession(session.sid);
            if (response.code !== 0) throw new Error(response.message || '停止失败');
            await load(true);
        } catch (err) {
            setError(err?.response?.data?.message || err?.message || '停止失败');
        } finally {
            setStopping('');
        }
    };

    const cleanableSessions = sessions.filter(isCleanableTestSession);
    const cleanupTestSessions = async () => {
        if (cleanableSessions.length === 0 || cleaning) return;
        const names = cleanableSessions.map(session => session.name).join('、');
        if (!window.confirm(`将清理本页 ${cleanableSessions.length} 个已停止、无样本测试任务：${names}。继续吗？`)) return;
        setCleaning(true);
        setError('');
        try {
            for (const session of cleanableSessions) {
                const response = await continuous.deleteSession(session.sid);
                if (response.code !== 0) throw new Error(response.message || `清理 ${session.name} 失败`);
            }
            await load();
        } catch (err) {
            setError(err?.response?.data?.message || err?.message || '清理测试任务失败');
        } finally {
            setCleaning(false);
        }
    };

    const cleanupOne = async session => {
        if (cleaning || !isCleanableTestSession(session)) return;
        if (!window.confirm(`确认清理测试任务“${session.name}”？仅删除已停止且无样本任务。`)) return;
        setCleaning(true);
        setError('');
        try {
            const response = await continuous.deleteSession(session.sid);
            if (response.code !== 0) throw new Error(response.message || '清理测试任务失败');
            await load();
        } catch (err) {
            setError(err?.response?.data?.message || err?.message || '清理测试任务失败');
        } finally {
            setCleaning(false);
        }
    };

    const degradedCount = sessions.filter(session => session.continuity_mode === 'degraded' && session.desired_state === 'running').length;
    const pageCount = Math.max(1, Math.ceil(total / PAGE_SIZE));

    const jumpTo = () => {
        if (!String(jumpInput).trim()) { setJumpInput(''); return; }
        const raw = Number(jumpInput);
        if (!Number.isInteger(raw)) { setJumpInput(''); return; }
        setPage(Math.min(Math.max(1, raw), pageCount));
        setJumpInput('');
    };
    return <section style={S.card}>
        <div style={S.head}><div><h3 style={S.title}>持续采集</h3><span style={S.subtle}>任务按用户期望持续运行；等待进程和 Agent 离线不会自动终止任务</span></div><span style={S.subtle}>共 {total} 条{ownerFilter === 'mine' ? '（仅我创建的）' : '（全部创建者）'}</span></div>
        {degradedCount > 0 && <div style={S.warn}>本页有 {degradedCount} 个活动任务正在降级运行。任务仍严格限制采集范围，但滚动采集窗口可能存在短暂空档。</div>}
        {error && <div style={S.error}>{error}</div>}
        <div className="continuous-list-toolbar" style={S.toolbar}>
            <div className="continuous-list-filters" style={S.filters}>
                <input style={S.input} value={keyword} onChange={event => { setKeyword(event.target.value); setPage(1); }} placeholder="搜索名称 / exe" />
                <select style={S.select} value={status} onChange={event => { setStatus(event.target.value); setPage(1); }}><option value="">全部状态</option><option value="running">运行中</option><option value="waiting">等待进程</option><option value="degraded">降级运行</option><option value="pending">待启动</option><option value="stopping">停止中</option><option value="stopped">已停止</option><option value="offline">Agent 离线</option><option value="error">异常</option></select>
                <select style={S.select} value={scope} onChange={event => { setScope(event.target.value); setPage(1); }}><option value="">全部范围</option><option value="host">整机</option><option value="process">进程</option></select>
                <span style={{ display: 'inline-flex', alignItems: 'center' }}>
                    <select aria-label="持续采集归属筛选" style={S.select} value={ownerFilter} onChange={event => { setOwnerFilter(event.target.value); setPage(1); }}><option value="all">全部创建者</option><option value="mine">我创建的</option></select>
                    <InfoTooltip label="查看持续采集归属筛选说明">默认显示这台主机上全部创建者的任务和完整分页；需要聚焦个人任务时可切换到“我创建的”。</InfoTooltip>
                </span>
            </div>
            <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
				<button aria-pressed={showTestSessions} style={S.button} onClick={() => { setShowTestSessions(value => !value); setPage(1); }}>
					{showTestSessions ? '隐藏测试任务' : '显示测试任务'}
				</button>
                {cleanableSessions.length > 0 && <span style={{ display: 'inline-flex', alignItems: 'center' }}>
                    <button style={S.dangerButton} onClick={cleanupTestSessions} disabled={cleaning}>{cleaning ? '清理中...' : `清理测试残留（${cleanableSessions.length}）`}</button>
                    <InfoTooltip label="查看清理测试残留说明">仅清理本页中名称明确带有测试标记、已经停止或异常、且没有任何样本和采集记录的空任务。后端会再次校验，不会删除有采集数据的任务。</InfoTooltip>
                </span>}
                <button style={S.button} onClick={() => load()} disabled={loading}>{loading ? '刷新中' : '刷新'}</button>
            </div>
        </div>
        {loading ? <div style={S.empty}>正在加载持续采集任务...</div> : sessions.length === 0 ? <div style={S.empty}>{total ? '当前页暂无持续采集任务' : '暂无匹配的持续采集任务。'}</div> : <div className="table-scroll" style={S.tableWrap}><table style={S.table}>
            <thead><tr><th style={S.th}>名称</th><th style={S.th}>范围与目标</th><th style={S.th} title="会话当前所处阶段：运行中 / 等待进程 / 已停止 / 离线等">状态</th><th style={S.th} title="这个持续采集会话采集了哪些数据（CPU / 块 IO / 调度延迟等）">信号</th><th style={S.th} title="距上一次成功把数据上传到服务器的时间；如果很久没更新，说明采集或上传可能有问题">最近上传</th><th style={S.th} title="会话已运行/已存在的时间，以及创建它的账号">持续时间 / 创建人</th><th style={S.th}>操作</th></tr></thead>
            <tbody>{sessions.map(session => {
                const state = session.observed_state || 'pending';
                const [background, color] = continuousStateColor(state);
                const signals = decodeJSONField(session.signals, ['cpu_profile']);
                const active = decodeJSONField(session.active_processes, []);
                const running = session.desired_state === 'running';
                const identity = selectorIdentity(session);
                const waitingReason = state === 'waiting' ? (session.degradation_reason || '等待匹配进程') : '';
                return <tr key={session.sid}>
                    <td style={S.td}><div style={S.name}>{session.name}</div><span style={S.subtle}>{shortSID(session.sid)}</span></td>
                    <td style={S.td}>{normalizedScope(session) === 'process' ? <><strong>进程 · {selectorModeLabel(identity.mode)} · {active.length} 个活动实例</strong><div style={S.mono} title={identity.detail}>{identity.exe || identity.detail}</div>{identity.mode === 'pid_instance' && identity.detail && <div style={S.subtle}>{identity.detail}</div>}{waitingReason && <div style={{ ...S.subtle, color: '#b54708', marginTop: 4 }} title={waitingReason}>{waitingReason}</div>}</> : <strong>整机</strong>}<div style={S.subtle}>样本 {formatCount(session.sample_count)}</div></td>
                    <td style={S.td}><span style={{ ...S.badge, background, color }}>{continuousStateLabel(state)}</span>{session.continuity_mode === 'degraded' && <div style={{ ...S.subtle, color: '#b54708', marginTop: 4 }}>降级连续性</div>}</td>
                    <td style={S.td}><div style={S.chips}>{signals.map(signal => <span key={signal} style={S.chip}>{signalLabel(signal)}</span>)}</div></td>
                    <td style={S.td}>{formatRelativeTime(session.last_upload_at)}</td>
                    <td style={S.td}>{duration(session.started_at, session.stopped_at)}<div style={S.subtle}>{session.user_name || 'system'}</div></td>
                    <td style={S.td}><Link style={S.link} to={`/hosts/${encodeURIComponent(target.id)}/continuous/${encodeURIComponent(session.sid)}`}>查看</Link>{running && session.can_manage && <button style={S.stop} disabled={stopping === session.sid} onClick={() => stop(session)}>{stopping === session.sid ? '停止中' : '停止'}</button>}{isCleanableTestSession(session) && <button style={S.stop} disabled={cleaning} onClick={() => cleanupOne(session)}>清理</button>}</td>
                </tr>;
            })}</tbody>
        </table></div>}
        {!loading && total > 0 && <div style={S.pagination}>
            <button aria-label="持续采集第一页" style={{ ...S.pageButton, ...(page <= 1 ? S.pageButtonDisabled : {}) }} disabled={page <= 1} onClick={() => setPage(1)}>首页</button>
            <button aria-label="持续采集上一页" style={{ ...S.pageButton, ...(page <= 1 ? S.pageButtonDisabled : {}) }} disabled={page <= 1} onClick={() => setPage(value => Math.max(1, value - 1))}>上一页</button>
            <span style={S.subtle}>第 {page} / {pageCount} 页</span>
            <button aria-label="持续采集下一页" style={{ ...S.pageButton, ...(page >= pageCount ? S.pageButtonDisabled : {}) }} disabled={page >= pageCount} onClick={() => setPage(value => Math.min(pageCount, value + 1))}>下一页</button>
            <button aria-label="持续采集最后一页" style={{ ...S.pageButton, ...(page >= pageCount ? S.pageButtonDisabled : {}) }} disabled={page >= pageCount} onClick={() => setPage(pageCount)}>末页</button>
            <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
                <input aria-label="持续采集跳转页码" style={S.jumpInput} type="number" min={1} max={pageCount} value={jumpInput} placeholder="页码" onChange={event => setJumpInput(event.target.value)} onKeyDown={event => { if (event.key === 'Enter') jumpTo(); }} />
                <button aria-label="持续采集跳转" style={S.pageButton} onClick={jumpTo}>跳转</button>
            </span>
        </div>}
    </section>;
}

function normalizedScope(session) { return session.scope === 'process' ? 'process' : 'host'; }
function shortSID(value) { return String(value || '').length > 18 ? `${value.slice(0, 10)}...${value.slice(-4)}` : value; }
function formatCount(value) {
    const count = Number(value) || 0;
    if (count >= 1000000) return `${(count / 1000000).toFixed(1)}M`;
    if (count >= 1000) return `${(count / 1000).toFixed(1)}K`;
    return String(Math.round(count));
}
function isCleanableTestSession(session) {
    const state = session?.observed_state || '';
    const desired = session?.desired_state || '';
    const name = String(session?.name || '').toLowerCase();
    const marker = ['boundary-', 'multilang-', 'test', 'smoke', '测试'].some(value => name.includes(value));
    return marker && desired === 'stopped' && ['stopped', 'error'].includes(state) && Number(session?.sample_count || 0) === 0 && session?.can_manage;
}
function duration(start, end) {
    const from = new Date(start).getTime();
    const to = end ? new Date(end).getTime() : Date.now();
    if (!Number.isFinite(from) || !Number.isFinite(to)) return '-';
    const seconds = Math.max(0, Math.floor((to - from) / 1000));
    if (seconds < 3600) return `${Math.floor(seconds / 60)} 分钟`;
    if (seconds < 86400) return `${(seconds / 3600).toFixed(1)} 小时`;
    return `${(seconds / 86400).toFixed(1)} 天`;
}
