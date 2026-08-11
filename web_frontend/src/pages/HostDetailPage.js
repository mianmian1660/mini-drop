import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Link, useParams, useSearchParams } from 'react-router-dom';
import { select } from 'd3-selection';
import { flamegraph as createFlamegraph } from 'd3-flame-graph';
import 'd3-flame-graph/dist/d3-flamegraph.css';
import { agents, profiles, schedules, tasks } from '../api';
import CreateTaskModal from '../components/CreateTaskModal';
import Pagination from '../components/Pagination';
import TimelineChart, { statusColor } from '../components/TimelineChart';
import { capabilityLabel, collectorLabelFromTask, parseStringList } from '../utils/collectors';

const S = {
    container: { maxWidth: 1280, margin: '0 auto', padding: 24, fontFamily: 'Arial, sans-serif', color: '#202124' },
    pageHead: { display: 'flex', justifyContent: 'space-between', gap: 16, alignItems: 'flex-end', marginBottom: 18 },
    eyebrow: { margin: '0 0 6px 0', color: '#667085', fontSize: 13 },
    title: { margin: 0, fontSize: 28, lineHeight: 1.2 },
    actions: { display: 'flex', gap: 10, flexWrap: 'wrap', justifyContent: 'flex-end' },
    card: { background: '#fff', borderRadius: 8, padding: 20, marginBottom: 16, border: '1px solid #e5e7eb', boxShadow: '0 1px 3px rgba(16,24,40,0.08)' },
    contextGrid: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(150px, 1fr))', gap: 10 },
    contextItem: { background: '#f8fafc', border: '1px solid #edf0f3', borderRadius: 6, padding: 12 },
    contextLabel: { color: '#667085', fontSize: 12, marginBottom: 5 },
    contextValue: { color: '#111827', fontSize: 14, fontWeight: 700, wordBreak: 'break-word' },
    tabBar: { display: 'flex', gap: 8, flexWrap: 'wrap', marginBottom: 16 },
    tab: (active) => ({ padding: '9px 14px', borderRadius: 6, border: active ? '1px solid #315efb' : '1px solid #d0d7de', background: active ? '#eef2ff' : '#fff', color: active ? '#315efb' : '#475467', cursor: 'pointer', fontSize: 14, fontWeight: 700 }),
    grid: { display: 'grid', gridTemplateColumns: 'minmax(280px, 0.8fr) minmax(420px, 1.2fr)', gap: 16 },
    sectionHead: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 12, marginBottom: 12 },
    sectionTitle: { margin: 0, fontSize: 18 },
    btn: { background: '#315efb', color: '#fff', border: 'none', padding: '9px 14px', borderRadius: 6, cursor: 'pointer', fontSize: 13, fontWeight: 700 },
    btnDisabled: { background: '#e5e7eb', color: '#667085', border: 'none', padding: '9px 14px', borderRadius: 6, cursor: 'not-allowed', fontSize: 13, fontWeight: 700 },
    btnSm: { background: '#f8fafc', color: '#475467', border: '1px solid #d0d7de', padding: '6px 10px', borderRadius: 6, cursor: 'pointer', fontSize: 12, textDecoration: 'none' },
    btnSecondary: { background: '#fff', color: '#315efb', border: '1px solid #c7d2fe', padding: '8px 12px', borderRadius: 6, cursor: 'pointer', fontSize: 13, fontWeight: 700, textDecoration: 'none' },
    badge: { display: 'inline-flex', alignItems: 'center', padding: '3px 9px', borderRadius: 999, fontSize: 12, fontWeight: 700 },
    metricGrid: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(130px, 1fr))', gap: 10 },
    metric: { background: '#f8fafc', border: '1px solid #edf0f3', borderRadius: 6, padding: 12 },
    metricLabel: { color: '#667085', fontSize: 12, marginBottom: 6 },
    metricValue: { fontSize: 18, fontWeight: 700, wordBreak: 'break-word' },
    capWrap: { display: 'flex', gap: 6, flexWrap: 'wrap', marginTop: 12 },
    capPill: { display: 'inline-flex', alignItems: 'center', padding: '2px 7px', borderRadius: 999, background: '#eef2ff', color: '#315efb', fontSize: 11, fontWeight: 700 },
    auditList: { display: 'grid', gap: 10 },
    auditItem: { display: 'grid', gridTemplateColumns: '150px 90px minmax(0, 1fr)', gap: 12, padding: '10px 12px', background: '#fbfcfe', border: '1px solid #edf0f3', borderRadius: 6, fontSize: 13 },
    table: { width: '100%', borderCollapse: 'collapse' },
    th: { textAlign: 'left', padding: '11px 12px', borderBottom: '1px solid #d0d7de', color: '#475467', fontSize: 12, background: '#f8fafc' },
    td: { padding: '11px 12px', borderBottom: '1px solid #edf0f3', fontSize: 13, verticalAlign: 'top' },
    toolbar: { display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap', marginBottom: 12 },
    input: { padding: '8px 10px', border: '1px solid #d0d7de', borderRadius: 6, fontSize: 13, boxSizing: 'border-box' },
    select: { padding: '8px 10px', border: '1px solid #d0d7de', borderRadius: 6, background: '#fff', fontSize: 13 },
    empty: { textAlign: 'center', padding: 34, color: '#667085', background: '#fbfcfe', border: '1px dashed #d0d7de', borderRadius: 8 },
    loading: { textAlign: 'center', padding: 40, color: '#999' },
    error: { background: '#fff3f3', border: '1px solid #ffcdd2', color: '#b42318', borderRadius: 8, padding: 12, marginBottom: 16 },
    subtle: { color: '#667085', fontSize: 12 },
    warn: { color: '#b42318', background: '#fff6f5', border: '1px solid #fda29b', borderRadius: 6, padding: '9px 12px', fontSize: 13, margin: '12px 0 0' },
    success: { color: '#067647', background: '#ecfdf3', border: '1px solid #abefc6', borderRadius: 6, padding: '9px 12px', fontSize: 13, margin: '12px 0 0' },
    info: { color: '#175cd3', background: '#eff6ff', border: '1px solid #bfdbfe', borderRadius: 6, padding: '9px 12px', fontSize: 13, margin: '12px 0 0' },
    statusBar: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(210px, 1fr))', gap: 10, margin: '12px 0' },
    statusChip: { display: 'flex', flexDirection: 'column', gap: 4, padding: 12, borderRadius: 6, border: '1px solid #e5e7eb', background: '#fbfcfe' },
    statusChipLabel: { color: '#667085', fontSize: 12, fontWeight: 700 },
    statusChipValue: { fontSize: 14, fontWeight: 700 },
    linkStrong: { color: '#315efb', fontWeight: 700, textDecoration: 'none' },
    flameGraphBox: { minHeight: 280, overflowX: 'auto', border: '1px solid #edf0f3', borderRadius: 6, background: '#fbfcfe', padding: 8 },
    fallbackTitle: { margin: '12px 0 8px', color: '#475467', fontSize: 12, fontWeight: 700 },
    flameWrap: { display: 'grid', gap: 8 },
    flameRow: { display: 'grid', gridTemplateColumns: 'minmax(140px, 220px) minmax(0, 1fr) 86px', gap: 10, alignItems: 'center', fontSize: 13 },
    barTrack: { height: 26, background: '#eef2f7', borderRadius: 6, overflow: 'hidden', border: '1px solid #e5e7eb' },
    bar: { height: '100%' },
};

const tabs = [
    { id: 'overview', label: '概览' },
    { id: 'tasks', label: '任务列表' },
    { id: 'timeline', label: '时间轴' },
    { id: 'profiling', label: '持续 profiling' },
    { id: 'logs', label: 'Agent 日志' },
];
const statusColors = { 0: '#ffc107', 1: '#2196f3', 2: '#4caf50', 3: '#f44336', 4: '#7c3aed' };
const statusNames = { 0: '待处理', 1: '执行中', 2: '已完成', 3: '失败', 4: '上传中' };
const ST = { 0: '待处理', 1: '执行中', 2: '已完成', 3: '失败', 4: '上传中' };

export default function HostDetailPage() {
    const { targetId: rawTargetId } = useParams();
    const [searchParams, setSearchParams] = useSearchParams();
    const targetId = decodeURIComponent(rawTargetId || '');
    const [targets, setTargets] = useState([]);
    const [agentDetail, setAgentDetail] = useState(null);
    const [hostTasks, setHostTasks] = useState([]);
    const [hostSchedules, setHostSchedules] = useState([]);
    const [profileSummary, setProfileSummary] = useState(null);
    const [loading, setLoading] = useState(true);
    const [detailLoading, setDetailLoading] = useState(false);
    const [error, setError] = useState('');
    const [showCreate, setShowCreate] = useState(false);

    const activeTab = tabs.some(t => t.id === searchParams.get('tab')) ? searchParams.get('tab') : 'overview';
    const target = useMemo(() => targets.find(t => t.id === targetId) || null, [targets, targetId]);
    const dropOnline = target?.drop_agent_status === 'online';

    const loadTargets = useCallback(async () => {
        const res = await profiles.targets();
        if (res.code !== 0) throw new Error(res.message || '加载可观测对象失败');
        setTargets(res.data?.targets || []);
    }, []);

    const loadAgentDetail = useCallback(async (ip, options = {}) => {
        if (!ip) return;
        if (!options.silent) setDetailLoading(true);
        try {
            const res = await agents.detail(ip);
            setAgentDetail(res.code === 0 ? res.data || null : null);
        } catch (err) {
            console.error('加载主机 Agent 详情失败:', err);
            setAgentDetail(null);
        } finally {
            if (!options.silent) setDetailLoading(false);
        }
    }, []);

    const loadHostTasks = useCallback(async (ip) => {
        if (!ip) return;
        const res = await tasks.list({ page: 1, pageSize: 5, keyword: ip });
        if (res.code === 0) setHostTasks(res.data?.tasks || []);
    }, []);

    const loadHostSchedules = useCallback(async (ip) => {
        if (!ip) return;
        const res = await schedules.list();
        if (res.code === 0) {
            setHostSchedules((res.data?.schedules || []).filter(s => s.target_ip === ip));
        }
    }, []);

    const loadProfileSummary = useCallback(async (target) => {
        if (!target) return;
        const to = new Date();
        const from = new Date(to.getTime() - 30 * 60 * 1000);
        try {
            const res = await profiles.topn({
                target_id: target.id,
                host: target.ip,
                service: target.service_name || 'hotmethod',
                from: from.toISOString(),
                to: to.toISOString(),
                profile_type: 'cpu',
            });
            if (res.code === 0) setProfileSummary(res.data || null);
        } catch (err) {
            setProfileSummary({ empty: true, message: err?.message || '持续 profiling 查询失败' });
        }
    }, []);

    useEffect(() => {
        setLoading(true);
        setError('');
        loadTargets()
            .catch(err => setError(err?.message || '加载主机信息失败'))
            .finally(() => setLoading(false));
    }, [loadTargets]);

    useEffect(() => {
        if (!target?.ip) return;
        loadAgentDetail(target.ip);
        loadHostTasks(target.ip);
        loadHostSchedules(target.ip);
        loadProfileSummary(target);
    }, [target, loadAgentDetail, loadHostTasks, loadHostSchedules, loadProfileSummary]);

    useEffect(() => {
        if (!target?.ip) return undefined;
        const timer = setInterval(() => {
            loadAgentDetail(target.ip, { silent: true });
            loadHostTasks(target.ip);
            loadHostSchedules(target.ip);
        }, 10000);
        return () => clearInterval(timer);
    }, [target, loadAgentDetail, loadHostTasks, loadHostSchedules]);

    const setTab = (tab) => {
        setSearchParams({ tab });
    };

    const handleTaskCreated = () => {
        setShowCreate(false);
        if (target?.ip) {
            loadHostTasks(target.ip);
            loadHostSchedules(target.ip);
            loadAgentDetail(target.ip, { silent: true });
        }
    };

    if (loading) return <div style={S.container}><p style={S.loading}>加载主机详情...</p></div>;
    if (error) return <MessagePage message={error} />;
    if (!target) return <MessagePage message="未找到当前主机或无权限访问。" />;

    const agent = agentDetail?.agent || {};
    const stat = agentDetail?.stat || {};
    const audits = agentDetail?.audits || [];
    const capabilities = parseStringList(agent.capabilities);

    return (
        <div style={S.container}>
            <div style={S.pageHead}>
                <div>
                    <p style={S.eyebrow}>Performance Center</p>
                    <h2 style={S.title}>{target.hostname || target.ip}</h2>
                </div>
                <div style={S.actions}>
                    <Link to="/" style={S.btnSecondary}>返回主机列表</Link>
                    <button style={dropOnline ? S.btn : S.btnDisabled} onClick={() => dropOnline && setShowCreate(true)} disabled={!dropOnline}>
                        新建采样
                    </button>
                </div>
            </div>

            <HostContext target={target} activeTab={activeTab} />
            {!dropOnline && <div style={S.warn}>drop_agent 当前不可用，按需采样暂不可创建；持续 profiling 仍可进入查看。</div>}

            <div style={S.tabBar}>
                {tabs.map(tab => <button key={tab.id} style={S.tab(activeTab === tab.id)} onClick={() => setTab(tab.id)}>{tab.label}</button>)}
            </div>

            {showCreate && (
                <CreateTaskModal
                    initialTargetIP={target.ip}
                    lockTargetIP
                    scheduleSuccessLink={`/hosts/${encodeURIComponent(target.id)}?tab=timeline`}
                    onClose={() => setShowCreate(false)}
                    onSuccess={handleTaskCreated}
                />
            )}

            {activeTab === 'overview' && (
                <OverviewPanel
                    target={target}
                    agent={agent}
                    stat={stat}
                    detailLoading={detailLoading}
                    capabilities={capabilities}
                    tasks={hostTasks}
                    schedules={hostSchedules}
                    profileSummary={profileSummary}
                    onRefresh={() => {
                        loadAgentDetail(target.ip);
                        loadHostTasks(target.ip);
                        loadHostSchedules(target.ip);
                        loadProfileSummary(target);
                    }}
                    onTab={setTab}
                />
            )}
            {activeTab === 'tasks' && <HostTasksPanel target={target} />}
            {activeTab === 'timeline' && <HostTimelinePanel target={target} />}
            {activeTab === 'profiling' && <HostProfilingPanel target={target} />}
            {activeTab === 'logs' && <AgentLogsPanel audits={audits} detailLoading={detailLoading} onRefresh={() => loadAgentDetail(target.ip)} />}
        </div>
    );
}

function HostContext({ target, activeTab }) {
    return (
        <section style={S.card}>
            <div style={S.contextGrid}>
                <ContextItem label="host" value={target.hostname || '-'} />
                <ContextItem label="ip" value={target.ip || '-'} />
                <ContextItem label="service" value={target.service_name || '-'} />
                <ContextItem label="environment" value={target.environment || '-'} />
                <ContextItem label="drop_agent" value={<StatusPill value={target.drop_agent_status} />} />
                <ContextItem label="parca_agent" value={<StatusPill value={target.parca_agent_status} />} />
                <ContextItem label="time range" value={activeTab === 'profiling' ? 'Tab 内选择' : '当前主机上下文'} />
                <ContextItem label="labels" value={labelSummary(target.labels)} />
            </div>
        </section>
    );
}

function OverviewPanel({ target, agent, stat, detailLoading, capabilities, tasks: taskItems, schedules: scheduleItems, profileSummary, onRefresh, onTab }) {
    const profilingMsg = profileSummary?.empty ? (profileSummary.message || '无持续 profiling 数据') : `${profileSummary?.items?.length || 0} 个热点函数`;
    return (
        <>
            <section style={S.card}>
                <div style={S.sectionHead}>
                    <h3 style={S.sectionTitle}>性能状态概览</h3>
                    <button style={S.btnSecondary} onClick={onRefresh} disabled={detailLoading}>{detailLoading ? '刷新中' : '刷新'}</button>
                </div>
                <div style={S.metricGrid}>
                    <Metric label="Agent 在线" value={agent.online ? 'ONLINE' : target.drop_agent_status || 'unknown'} />
                    <Metric label="资源数据源" value={stat.source === 'grpc' ? '实时 gRPC' : '数据库快照'} />
                    <Metric label="CPU" value={`${formatMetric(stat.cpu_percent, 1)}%`} />
                    <Metric label="内存" value={formatMemory(stat.memory_kb)} />
                    <Metric label="定时窗口" value={scheduleItems.length} />
                    <Metric label="持续 profiling" value={profilingMsg} />
                </div>
                <div style={S.capWrap}>
                    {capabilities.length === 0 ? <span style={S.subtle}>未声明采集能力</span> : capabilities.map(cap => <span key={cap} style={S.capPill}>{capabilityLabel(cap)}</span>)}
                </div>
            </section>

            <div style={S.grid}>
                <section style={S.card}>
                    <div style={S.sectionHead}>
                        <h3 style={S.sectionTitle}>最近任务</h3>
                        <button style={S.btnSm} onClick={() => onTab('tasks')}>查看任务列表</button>
                    </div>
                    <MiniTaskList tasks={taskItems} />
                </section>
                <section style={S.card}>
                    <div style={S.sectionHead}>
                        <h3 style={S.sectionTitle}>最近定时窗口</h3>
                        <button style={S.btnSm} onClick={() => onTab('timeline')}>查看时间轴</button>
                    </div>
                    {scheduleItems.length === 0 ? <div style={S.empty}>该主机暂无持续采集计划</div> : (
                        <div style={S.auditList}>
                            {scheduleItems.slice(0, 5).map(s => (
                                <div key={s.sid} style={S.auditItem}>
                                    <span>{s.cron_expr}</span>
                                    <StatusPill value={s.enabled ? 'online' : 'offline'} />
                                    <span>{s.name}</span>
                                </div>
                            ))}
                        </div>
                    )}
                </section>
            </div>

            <section style={S.card}>
                <div style={S.sectionHead}>
                    <h3 style={S.sectionTitle}>持续 profiling 摘要</h3>
                    <button style={S.btnSm} onClick={() => onTab('profiling')}>进入持续 profiling</button>
                </div>
                {profileSummary?.empty ? <div style={S.empty}>{profileSummary.message || '暂无持续 profiling 数据'}</div> : <TopNTable data={profileSummary} compact />}
            </section>
        </>
    );
}

function HostTasksPanel({ target }) {
    const [taskList, setTaskList] = useState([]);
    const [total, setTotal] = useState(0);
    const [page, setPage] = useState(1);
    const [statusFilter, setStatusFilter] = useState('');
    const [keyword, setKeyword] = useState('');
    const [loading, setLoading] = useState(true);
    const pageSize = 10;
    const totalPages = Math.max(1, Math.ceil(total / pageSize));

    const loadTasks = useCallback(async () => {
        setLoading(true);
        try {
            const res = await tasks.list({ page, pageSize, keyword: target.ip, status: statusFilter || undefined });
            if (res.code === 0) {
                const list = res.data?.tasks || [];
                const q = keyword.trim().toLowerCase();
                setTaskList(q ? list.filter(t => [t.tid, t.name, collectorLabelFromTask(t)].some(v => String(v || '').toLowerCase().includes(q))) : list);
                setTotal(res.data?.total || 0);
            }
        } finally {
            setLoading(false);
        }
    }, [target.ip, page, pageSize, statusFilter, keyword]);

    useEffect(() => { loadTasks(); }, [loadTasks]);
    useEffect(() => {
        const timer = setInterval(loadTasks, 10000);
        return () => clearInterval(timer);
    }, [loadTasks]);

    return (
        <section style={S.card}>
            <div style={S.sectionHead}>
                <h3 style={S.sectionTitle}>该主机任务列表</h3>
                <span style={S.subtle}>target_ip = {target.ip}</span>
            </div>
            <div style={S.toolbar}>
                <input style={{ ...S.input, minWidth: 220 }} value={keyword} onChange={e => { setKeyword(e.target.value); setPage(1); }} placeholder="在该主机任务内搜索名称 / ID / 采集器" />
                <select style={S.select} value={statusFilter} onChange={e => { setStatusFilter(e.target.value); setPage(1); }}>
                    <option value="">全部状态</option>
                    <option value="0">待处理</option>
                    <option value="1">执行中</option>
                    <option value="4">上传中</option>
                    <option value="2">已完成</option>
                    <option value="3">失败</option>
                </select>
            </div>
            {loading && taskList.length === 0 ? <div style={S.loading}>加载任务...</div> : <TaskTable tasks={taskList} />}
            <Pagination page={page} totalPages={totalPages} onPageChange={setPage} />
        </section>
    );
}

function HostTimelinePanel({ target }) {
    const [scheduleList, setScheduleList] = useState([]);
    const [masterTid, setMasterTid] = useState('');
    const [points, setPoints] = useState([]);
    const [loading, setLoading] = useState(false);
    const [error, setError] = useState('');
    const [statusFilter, setStatusFilter] = useState('');
    const [kindFilter, setKindFilter] = useState('');
    const [hasResultFilter, setHasResultFilter] = useState('');
    const [range, setRange] = useState('all');
    const [baselineTid, setBaselineTid] = useState('');

    const timelineFilters = useCallback(() => ({
        status: statusFilter || undefined,
        task_kind: kindFilter || undefined,
        has_result: hasResultFilter || undefined,
    }), [statusFilter, kindFilter, hasResultFilter]);

    const loadSchedules = useCallback(async () => {
        const res = await schedules.list();
        if (res.code === 0) {
            const list = (res.data?.schedules || []).filter(s => s.target_ip === target.ip);
            setScheduleList(list);
            if (!masterTid && list.length > 0) setMasterTid(list[0].sid);
        }
    }, [target.ip, masterTid]);

    const loadTimeline = useCallback(async (sid = masterTid) => {
        if (!sid) return;
        setMasterTid(sid);
        setLoading(true);
        setError('');
        try {
            const opts = { ...timelineFilters() };
            if (range !== 'all') {
                const hours = Number(range);
                const to = new Date();
                const from = new Date(to.getTime() - hours * 3600 * 1000);
                opts.from = from.toISOString();
                opts.to = to.toISOString();
            }
            const res = await tasks.timeline(sid, opts);
            if (res.code === 0) setPoints(res.data?.points || []);
            else setError(res.message || '查询时间轴失败');
        } catch (err) {
            setError(err?.message || '查询时间轴失败');
        } finally {
            setLoading(false);
        }
    }, [masterTid, range, timelineFilters]);

    useEffect(() => { loadSchedules(); }, [loadSchedules]);
    useEffect(() => { if (masterTid) loadTimeline(masterTid); }, [masterTid, range, statusFilter, kindFilter, hasResultFilter, loadTimeline]);

    return (
        <>
            <section style={S.card}>
                <div style={S.sectionHead}>
                    <h3 style={S.sectionTitle}>该主机时间轴</h3>
                    <span style={S.subtle}>只显示 target_ip = {target.ip} 的定时采集</span>
                </div>
                {scheduleList.length === 0 ? <div style={S.empty}>该主机暂无持续采集计划。点击“新建采样”并勾选持续采集后会出现在这里。</div> : (
                    <>
                        <div style={S.toolbar}>
                            <select style={S.select} value={masterTid} onChange={e => setMasterTid(e.target.value)}>
                                {scheduleList.map(s => <option key={s.sid} value={s.sid}>{s.name} · {s.cron_expr}</option>)}
                            </select>
                            <select style={S.select} value={range} onChange={e => setRange(e.target.value)}>
                                <option value="all">全部窗口</option>
                                <option value="1">最近 1 小时</option>
                                <option value="24">最近 24 小时</option>
                                <option value="168">最近 7 天</option>
                            </select>
                            <select style={S.select} value={statusFilter} onChange={e => setStatusFilter(e.target.value)}>
                                <option value="">全部状态</option>
                                <option value="2">已完成</option>
                                <option value="3">失败</option>
                                <option value="1">执行中</option>
                            </select>
                            <input style={S.input} value={kindFilter} onChange={e => setKindFilter(e.target.value)} placeholder="task_kind" />
                            <select style={S.select} value={hasResultFilter} onChange={e => setHasResultFilter(e.target.value)}>
                                <option value="">全部结果</option>
                                <option value="true">有结果</option>
                                <option value="false">无结果</option>
                            </select>
                            <button style={S.btnSm} onClick={() => loadTimeline(masterTid)}>刷新</button>
                        </div>
                        {error && <div style={S.error}>{error}</div>}
                        {loading ? <div style={S.loading}>加载时间轴...</div> : points.length === 0 ? <div style={S.empty}>当前筛选条件下没有采集窗口</div> : (
                            <TimelineResult points={points} baselineTid={baselineTid} setBaselineTid={setBaselineTid} />
                        )}
                    </>
                )}
            </section>
        </>
    );
}

function TimelineResult({ points, baselineTid, setBaselineTid }) {
    return (
        <div>
            <TimelineChart points={points} />
            <div style={{ marginTop: 16 }}>
                {points.map((p, i) => (
                    <div key={p.tid} style={{ ...S.card, boxShadow: 'none', padding: 14, marginBottom: 10 }}>
                        <div style={{ display: 'flex', justifyContent: 'space-between', gap: 12, alignItems: 'center' }}>
                            <div>
                                <strong>{i + 1}. {p.name || p.tid}</strong>
                                <span style={{ ...S.badge, marginLeft: 8, background: statusColor(p.status), color: '#fff' }}>{ST[p.status] || '未知'}</span>
                                {p.task_kind && <span style={{ marginLeft: 8, fontSize: 12, color: '#667085' }}>{p.task_kind}</span>}
                                {p.has_result && <span style={{ marginLeft: 8, fontSize: 12, color: '#16a34a' }}>有结果</span>}
                            </div>
                            <div style={{ display: 'flex', gap: 8 }}>
                                {p.has_result && (baselineTid === p.tid ? (
                                    <button style={S.btnSm} onClick={() => setBaselineTid('')}>基线</button>
                                ) : baselineTid ? (
                                    <Link to={`/task/diff?baseline=${baselineTid}&compare=${p.tid}`} style={S.btnSm}>与基线对比</Link>
                                ) : (
                                    <button style={S.btnSm} onClick={() => setBaselineTid(p.tid)}>设为基线</button>
                                ))}
                                <Link to={p.result_url || `/task/result?tid=${p.tid}`} style={S.btnSm}>查看详情</Link>
                            </div>
                        </div>
                        <div style={S.subtle}>
                            窗口 {formatTime(p.window_start || p.create_time)} → {formatTime(p.window_end || p.end_time) || '进行中'}
                        </div>
                    </div>
                ))}
            </div>
        </div>
    );
}

function HostProfilingPanel({ target }) {
    const [range, setRange] = useState('30m');
    const [profileType, setProfileType] = useState('cpu');
    const [flamegraph, setFlamegraph] = useState(null);
    const [topn, setTopn] = useState(null);
    const [querying, setQuerying] = useState(false);
    const [error, setError] = useState('');
    const timeWindow = useMemo(() => makeTimeWindow(range), [range]);
    const labelSelector = labelSelectorForTarget(target);
    const parcaUIURL = target.parca_ui_url || 'http://localhost:7070';
    const pprofScrapeStatus = target.pprof_scrape_status || 'unknown';
    const parcaAgentStatusText = target.parca_agent_status === 'online'
        ? 'online'
        : 'WSL/eBPF 不可用或未接入';
    const pprofScrapeTargets = Array.isArray(target.pprof_scrape_targets) && target.pprof_scrape_targets.length > 0
        ? target.pprof_scrape_targets.join(' / ')
        : 'parca:7070 / pprof_demo:6060';
    const nativeStatus = querying && !flamegraph && !topn
        ? '查询中'
        : (flamegraph?.empty || topn?.empty) ? '待接入 gRPC/Connect' : 'ready';
    const pprofStatusText = pprofScrapeStatus === 'available' ? 'available' : pprofScrapeStatus;
    const pprofMessage = pprofScrapeStatus === 'available'
        ? 'Parca 已在 scrape 标准 Go pprof，可在外部 UI 查询 {job="pprof_demo"}。'
        : (target.pprof_scrape_message || 'Parca/pprof scrape 当前不可确认。');

    const refresh = useCallback(async () => {
        setQuerying(true);
        setError('');
        const params = {
            target_id: target.id,
            host: target.ip,
            service: target.service_name || 'hotmethod',
            from: timeWindow.from,
            to: timeWindow.to,
            profile_type: profileType,
            labels: JSON.stringify(target.labels || {}),
        };
        try {
            const [fgRes, topRes] = await Promise.all([profiles.flamegraph(params), profiles.topn(params)]);
            if (fgRes.code === 0) setFlamegraph(fgRes.data);
            if (topRes.code === 0) setTopn(topRes.data);
            if (fgRes.code !== 0) setError(fgRes.message || '加载火焰图失败');
            if (topRes.code !== 0) setError(topRes.message || '加载 TopN 失败');
        } catch (err) {
            setFlamegraph(null);
            setTopn(null);
            setError(err?.message || '持续 profiling 查询失败');
        } finally {
            setQuerying(false);
        }
    }, [target, timeWindow, profileType]);

    useEffect(() => { refresh(); }, [refresh]);

    return (
        <>
            <section style={S.card}>
                <div style={S.sectionHead}>
                    <div>
                        <h3 style={S.sectionTitle}>持续 profiling</h3>
                        <div style={{ ...S.subtle, marginTop: 4 }}>Mini-Drop 原生视图为主；Parca 作为外部数据源入口。</div>
                    </div>
                    <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', justifyContent: 'flex-end' }}>
                        <a href={parcaUIURL} target="_blank" rel="noreferrer" style={S.btnSecondary}>打开 Parca UI</a>
                        <button style={S.btnSecondary} onClick={refresh} disabled={querying}>{querying ? '查询中' : '刷新'}</button>
                    </div>
                </div>
                <div style={S.statusBar}>
                    <ProfileStatusChip label="pprof scrape" value={pprofStatusText} tone={pprofScrapeStatus === 'available' ? 'success' : 'info'} />
                    <ProfileStatusChip label="eBPF agent" value={parcaAgentStatusText} tone={target.parca_agent_status === 'online' ? 'success' : 'muted'} />
                    <ProfileStatusChip label="Mini-Drop 原生查询" value={nativeStatus} tone={nativeStatus === 'ready' ? 'success' : 'info'} />
                </div>
                <div style={pprofScrapeStatus === 'available' ? S.success : S.info}>
                    {pprofMessage}
                </div>
                <div style={S.toolbar}>
                    <select style={S.select} value={range} onChange={e => setRange(e.target.value)}>
                        <option value="15m">最近 15 分钟</option>
                        <option value="30m">最近 30 分钟</option>
                        <option value="1h">最近 1 小时</option>
                        <option value="6h">最近 6 小时</option>
                    </select>
                    <select style={S.select} value={profileType} onChange={e => setProfileType(e.target.value)}>
                        <option value="cpu">CPU</option>
                        <option value="memory">Memory</option>
                    </select>
                    <input style={{ ...S.input, minWidth: 360 }} value={labelSelector} readOnly />
                </div>
                <div style={S.contextGrid}>
                    <ContextItem label="time range" value={`${formatTime(timeWindow.from)} - ${formatTime(timeWindow.to)}`} />
                    <ContextItem label="label selector" value={labelSelector} />
                    <ContextItem label="scrape targets" value={pprofScrapeTargets} />
                    <ContextItem label="推荐查询" value='{job="pprof_demo"} 或 {job="parca"}' />
                </div>
            </section>
            {error && <div style={S.error}>{error}</div>}
            <div style={S.grid}>
                <section style={S.card}>
                    <div style={S.sectionHead}>
                        <h3 style={S.sectionTitle}>火焰图</h3>
                        <span style={S.subtle}>{flamegraph?.source || 'mini-drop'} · {flamegraph?.unit || 'samples'}</span>
                    </div>
                    <FlamegraphView data={flamegraph} loading={querying} parcaUIURL={parcaUIURL} />
                </section>
                <section style={S.card}>
                    <div style={S.sectionHead}>
                        <h3 style={S.sectionTitle}>TopN Self/Total</h3>
                        <span style={S.subtle}>{topn?.items?.length || 0} functions</span>
                    </div>
                    <TopNTable data={topn} loading={querying} parcaUIURL={parcaUIURL} />
                </section>
            </div>
        </>
    );
}

function AgentLogsPanel({ audits, detailLoading, onRefresh }) {
    return (
        <section style={S.card}>
            <div style={S.sectionHead}>
                <h3 style={S.sectionTitle}>该主机 Agent 日志</h3>
                <button style={S.btnSecondary} onClick={onRefresh} disabled={detailLoading}>{detailLoading ? '刷新中' : '刷新'}</button>
            </div>
            {audits.length === 0 ? <div style={S.empty}>暂无该主机审计日志</div> : (
                <div style={S.auditList}>
                    {audits.slice(0, 20).map(item => (
                        <div key={item.id} style={S.auditItem}>
                            <span>{formatTime(item.created_at)}</span>
                            <span style={{ ...S.badge, background: auditColor(item.event), color: '#fff', justifyContent: 'center' }}>{auditName(item.event)}</span>
                            <span>{item.reason || '-'}</span>
                        </div>
                    ))}
                </div>
            )}
        </section>
    );
}

function MiniTaskList({ tasks: taskItems }) {
    if (taskItems.length === 0) return <div style={S.empty}>该主机暂无按需任务</div>;
    return <TaskTable tasks={taskItems.slice(0, 5)} compact />;
}

function TaskTable({ tasks: taskItems, compact = false }) {
    if (taskItems.length === 0) return <div style={S.empty}>没有匹配的任务</div>;
    return (
        <table style={S.table}>
            <thead>
                <tr>
                    <th style={S.th}>任务ID</th>
                    <th style={S.th}>名称</th>
                    {!compact && <th style={S.th}>采集器</th>}
                    <th style={S.th}>状态</th>
                    <th style={S.th}>创建时间</th>
                    <th style={S.th}>操作</th>
                </tr>
            </thead>
            <tbody>
                {taskItems.map(task => (
                    <tr key={task.tid}>
                        <td style={{ ...S.td, color: '#667085', fontSize: 12 }}>{task.tid}</td>
                        <td style={S.td}>{task.name}</td>
                        {!compact && <td style={S.td}>{collectorLabelFromTask(task)}</td>}
                        <td style={S.td}><span style={{ ...S.badge, background: statusColors[task.status] || '#999', color: '#fff' }}>{statusNames[task.status] || '未知'}</span></td>
                        <td style={S.td}>{formatTime(task.create_time)}</td>
                        <td style={S.td}><Link to={`/task/result?tid=${task.tid}`} style={{ color: '#315efb', fontWeight: 700 }}>查看</Link></td>
                    </tr>
                ))}
            </tbody>
        </table>
    );
}

function FlamegraphView({ data, loading, parcaUIURL }) {
    if (loading && !data) return <div style={S.empty}>正在查询持续 profiling...</div>;
    if (!data || data.empty || !Array.isArray(data.nodes) || data.nodes.length === 0) {
        return <ProfileEmptyState parcaUIURL={parcaUIURL} />;
    }
    return <InteractiveFlamegraph data={data} />;
}

function InteractiveFlamegraph({ data }) {
    const graphRef = useRef(null);
    const [renderError, setRenderError] = useState('');

    useEffect(() => {
        if (!graphRef.current) return undefined;
        const root = miniDropToD3Flamegraph(data);
        const selection = select(graphRef.current);
        selection.selectAll('*').remove();
        setRenderError('');
        try {
            const chart = createFlamegraph()
                .width(Math.max(graphRef.current.clientWidth || 760, 760))
                .cellHeight(18)
                .transitionDuration(150)
                .minFrameSize(3)
                .sort(true)
                .title('');
            selection.datum(root).call(chart);
        } catch (err) {
            setRenderError(err?.message || '火焰图渲染失败');
        }
        return () => {
            selection.selectAll('*').remove();
        };
    }, [data]);

    return (
        <>
            <div ref={graphRef} style={S.flameGraphBox} />
            {renderError && <div style={S.warn}>火焰图渲染失败，已显示下方摘要：{renderError}</div>}
            <div style={S.fallbackTitle}>热点摘要</div>
            <FlamegraphRows data={data} />
        </>
    );
}

function FlamegraphRows({ data }) {
    const rows = flattenNodes(data.nodes).slice(0, 24);
    const max = Math.max(...rows.map(row => row.value), 1);
    return (
        <div style={S.flameWrap}>
            {rows.map((row, index) => (
                <div key={`${row.name}-${index}`} style={{ ...S.flameRow, paddingLeft: Math.min(row.depth * 18, 90) }}>
                    <span title={row.name}>{truncate(row.name, 34)}</span>
                    <div style={S.barTrack}><div style={{ ...S.bar, width: `${Math.max(4, (row.value / max) * 100)}%`, background: barColor(row.depth) }} /></div>
                    <strong>{formatMetric(row.value, 1)}</strong>
                </div>
            ))}
        </div>
    );
}

function miniDropToD3Flamegraph(data) {
    return {
        name: 'root',
        value: Number(data?.total || sumNodeValues(data?.nodes || [])) || 1,
        children: (data?.nodes || []).map(toD3Node),
    };
}

function toD3Node(node) {
    return {
        name: node.name || 'unknown',
        value: Number(node.value || 0),
        children: (node.children || []).map(toD3Node),
    };
}

function sumNodeValues(nodes) {
    return nodes.reduce((sum, node) => sum + Number(node.value || 0), 0);
}

function TopNTable({ data, loading, compact = false, parcaUIURL }) {
    if (loading && !data) return <div style={S.empty}>正在查询 TopN...</div>;
    const items = data?.items || [];
    if (data?.empty || items.length === 0) {
        return <ProfileEmptyState parcaUIURL={parcaUIURL} compact={compact} />;
    }
    return (
        <table style={S.table}>
            <thead>
                <tr><th style={S.th}>函数</th><th style={S.th}>Total</th><th style={S.th}>Self</th></tr>
            </thead>
            <tbody>
                {items.slice(0, compact ? 5 : 14).map((item, index) => (
                    <tr key={`${item.name}-${index}`}>
                        <td style={S.td} title={item.name}>{truncate(item.name, compact ? 44 : 36)}</td>
                        <td style={S.td}>{formatMetric(item.value, 1)} {item.unit || data.unit || ''}</td>
                        <td style={S.td}>{formatMetric(item.self, 1)}</td>
                    </tr>
                ))}
            </tbody>
        </table>
    );
}

function ProfileEmptyState({ parcaUIURL, compact = false }) {
    return (
        <div style={S.empty}>
            <div style={{ fontWeight: 700, color: '#475467', marginBottom: 6 }}>
                Parca 已可采 pprof，Mini-Drop 原生转换待接入
            </div>
            {!compact && (
                <div style={{ ...S.subtle, marginBottom: 12 }}>
                    当前 WSL 环境下可先在 Parca UI 查询 <strong>{'{job="pprof_demo"}'}</strong>。
                </div>
            )}
            {parcaUIURL && (
                <a href={parcaUIURL} target="_blank" rel="noreferrer" style={S.btnSecondary}>打开 Parca UI</a>
            )}
        </div>
    );
}

function ProfileStatusChip({ label, value, tone = 'info' }) {
    const colors = {
        success: { border: '#abefc6', background: '#ecfdf3', color: '#067647' },
        info: { border: '#bfdbfe', background: '#eff6ff', color: '#175cd3' },
        muted: { border: '#e5e7eb', background: '#f8fafc', color: '#475467' },
    };
    const color = colors[tone] || colors.info;
    return (
        <div style={{ ...S.statusChip, borderColor: color.border, background: color.background }}>
            <span style={S.statusChipLabel}>{label}</span>
            <span style={{ ...S.statusChipValue, color: color.color }}>{value || '-'}</span>
        </div>
    );
}

function MessagePage({ message }) {
    return <div style={S.container}><div style={S.error}>{message}</div><Link to="/" style={S.btnSecondary}>返回主机列表</Link></div>;
}

function ContextItem({ label, value }) {
    return <div style={S.contextItem}><div style={S.contextLabel}>{label}</div><div style={S.contextValue}>{value}</div></div>;
}

function Metric({ label, value }) {
    return <div style={S.metric}><div style={S.metricLabel}>{label}</div><div style={S.metricValue}>{value}</div></div>;
}

function StatusPill({ value }) {
    const status = String(value || 'unknown');
    const color = status === 'online' ? '#16a34a' : status === 'offline' ? '#dc2626' : status === 'unconfigured' ? '#64748b' : '#7c3aed';
    return <span style={{ ...S.badge, background: color, color: '#fff' }}>{status}</span>;
}

function makeTimeWindow(range) {
    const to = new Date();
    const minutes = { '15m': 15, '30m': 30, '1h': 60, '6h': 360 }[range] || 30;
    const from = new Date(to.getTime() - minutes * 60 * 1000);
    return { from: from.toISOString(), to: to.toISOString() };
}

function flattenNodes(nodes, depth = 0) {
    return nodes.flatMap(node => [{ ...node, depth }, ...flattenNodes(node.children || [], depth + 1)]);
}

function labelSummary(labels) {
    const entries = Object.entries(labels || {});
    if (entries.length === 0) return 'node/job/env/instance 待接入';
    return entries.slice(0, 4).map(([k, v]) => `${k}=${v}`).join(', ');
}

function labelSelectorForTarget(target) {
    const labels = { ...(target.labels || {}) };
    if (!labels.node && target.hostname) labels.node = target.hostname;
    if (!labels.instance && target.ip) labels.instance = target.ip;
    if (!labels.job && target.service_name) labels.job = target.service_name;
    if (!labels.env && target.environment) labels.env = target.environment;
    return `{${Object.entries(labels).map(([k, v]) => `${k}="${v}"`).join(', ')}}`;
}

function formatTime(value) {
    if (!value) return '';
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return String(value);
    return date.toLocaleString();
}

function formatMetric(value, digits = 1) {
    const num = Number(value);
    if (!Number.isFinite(num)) return '0.0';
    return num.toFixed(digits);
}

function formatMemory(kb) {
    const num = Number(kb);
    if (!Number.isFinite(num) || num <= 0) return '0 MB';
    return `${(num / 1024).toFixed(1)} MB`;
}

function truncate(value, limit) {
    const text = String(value || '-');
    return text.length > limit ? `${text.slice(0, limit - 1)}...` : text;
}

function barColor(depth) {
    return ['#2f6fed', '#18a058', '#d97706', '#7c3aed', '#c2410c'][depth % 5];
}

function auditName(event) {
    if (event === 'registered') return '注册';
    if (event === 'offline') return '离线';
    if (event === 'recovered') return '恢复';
    return event || '事件';
}

function auditColor(event) {
    if (event === 'offline') return '#dc2626';
    if (event === 'recovered') return '#16a34a';
    if (event === 'registered') return '#2563eb';
    return '#64748b';
}
