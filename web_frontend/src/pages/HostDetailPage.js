import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { Link, useParams, useSearchParams } from 'react-router-dom';
import { agents, profiles, schedules, tasks } from '../api';
import CreateTaskModal from '../components/CreateTaskModal';
import CreateContinuousSessionModal from '../components/CreateContinuousSessionModal';
import ContinuousSessionList from '../components/ContinuousSessionList';
import Pagination from '../components/Pagination';
import TimelineChart, { statusColor } from '../components/TimelineChart';
import { capabilityLabel, collectorLabelFromTask, parseStringList } from '../utils/collectors';
import { formatMetricValue, metricColumnLabel } from '../utils/profileMetrics';
import { browserTimeZoneLabel, formatDateTime, localDateTimeToISO } from '../utils/time';

const S = {
    container: { width: '100%', maxWidth: 1320, minWidth: 0, margin: '0 auto', padding: '22px 28px 36px', fontFamily: 'Arial, sans-serif', color: '#101828' },
    pageHead: { display: 'flex', justifyContent: 'space-between', gap: 16, alignItems: 'flex-end', marginBottom: 12 },
    eyebrow: { margin: '0 0 6px 0', color: '#667085', fontSize: 13 },
    title: { margin: 0, fontSize: 28, lineHeight: 1.2, letterSpacing: 0 },
    actions: { display: 'flex', gap: 10, flexWrap: 'wrap', justifyContent: 'flex-end' },
    card: { minWidth: 0, maxWidth: '100%', background: '#fff', borderRadius: 8, padding: 20, marginBottom: 16, border: '1px solid #e5e7eb', boxShadow: '0 1px 2px rgba(16,24,40,0.04)' },
    contextDetails: { background: '#fff', border: '1px solid #eaecf0', borderRadius: 8, marginBottom: 14 },
    contextSummary: { cursor: 'pointer', padding: '12px 14px', display: 'flex', justifyContent: 'space-between', gap: 14, alignItems: 'center', color: '#667085', fontSize: 13, listStyle: 'none' },
    contextSummaryMain: { display: 'flex', gap: 10, alignItems: 'center', flexWrap: 'wrap', minWidth: 0 },
    contextSummaryTitle: { color: '#111827', fontSize: 14, fontWeight: 700 },
    contextSummaryText: { color: '#475467', fontSize: 13, lineHeight: 1.4 },
    contextGrid: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(230px, 1fr))', gap: '14px 28px', padding: '14px', borderTop: '1px solid #f2f4f7' },
    contextItem: { minWidth: 0 },
    contextItemWide: { gridColumn: '1 / -1' },
    contextLabel: { color: '#667085', fontSize: 12, marginBottom: 5 },
    contextValue: { color: '#111827', fontSize: 14, fontWeight: 650, wordBreak: 'break-word', lineHeight: 1.45 },
    tabBar: { display: 'flex', gap: 22, flexWrap: 'wrap', marginBottom: 16, borderBottom: '1px solid #e5e7eb' },
    tab: (active) => ({ padding: '10px 0 11px', borderRadius: 0, border: 'none', borderBottom: active ? '2px solid #315efb' : '2px solid transparent', background: 'transparent', color: active ? '#315efb' : '#667085', cursor: 'pointer', fontSize: 14, fontWeight: 700 }),
    grid: { display: 'grid', gridTemplateColumns: 'minmax(280px, 0.8fr) minmax(420px, 1.2fr)', gap: 16, minWidth: 0, maxWidth: '100%' },
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
    flameGraphBox: { width: '100%', minWidth: 0, maxWidth: '100%', minHeight: 280, overflowX: 'auto', border: '1px solid #edf0f3', borderRadius: 6, background: '#fbfcfe', padding: 8 },
    fallbackTitle: { margin: '12px 0 8px', color: '#475467', fontSize: 12, fontWeight: 700 },
    flameWrap: { display: 'grid', gap: 8 },
    flameRow: { display: 'grid', gridTemplateColumns: 'minmax(140px, 220px) minmax(0, 1fr) 86px', gap: 10, alignItems: 'center', fontSize: 13 },
    barTrack: { height: 26, background: '#eef2f7', borderRadius: 6, overflow: 'hidden', border: '1px solid #e5e7eb' },
    bar: { height: '100%' },
};

const tabs = [
    { id: 'overview', label: '概览' },
	{ id: 'tasks', label: '任务列表' },
	{ id: 'timeline', label: '周期性深度采样' },
	{ id: 'profiling', label: '持续采集' },
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
	const [showContinuousCreate, setShowContinuousCreate] = useState(false);
	const [continuousRefreshToken, setContinuousRefreshToken] = useState(0);

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
        const res = await tasks.list({ page: 1, pageSize: 5, target_ip: ip, owner_filter: 'all' });
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
            setProfileSummary({ empty: true, message: err?.message || 'Native Continuous Profiling 查询失败' });
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
        <div className="performance-page" style={S.container}>
            <div className="performance-page-header" style={S.pageHead}>
                <div>
                    <p style={S.eyebrow}>Performance Center</p>
                    <h2 style={S.title}>{target.hostname || target.ip}</h2>
                </div>
                <div className="performance-page-actions" style={S.actions}>
                    <Link to="/" style={S.btnSecondary}>返回主机列表</Link>
                    <button style={dropOnline ? S.btn : S.btnDisabled} onClick={() => dropOnline && setShowCreate(true)} disabled={!dropOnline}>
						新建单次采样
                    </button>
					<button style={dropOnline ? S.btn : S.btnDisabled} onClick={() => dropOnline && setShowContinuousCreate(true)} disabled={!dropOnline}>
						新建持续采集
					</button>
                </div>
            </div>

            <HostContext target={target} activeTab={activeTab} />
            {!dropOnline && <DropAgentNotice target={target} activeTab={activeTab} />}

            <div className="performance-tabs" style={S.tabBar}>
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

			{showContinuousCreate && (
				<CreateContinuousSessionModal
					target={target}
					onClose={() => setShowContinuousCreate(false)}
					onSuccess={() => {
						setShowContinuousCreate(false);
						setContinuousRefreshToken(value => value + 1);
						setTab('profiling');
					}}
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
			{activeTab === 'profiling' && <HostProfilingPanel target={target} refreshToken={continuousRefreshToken} />}
            {activeTab === 'logs' && <AgentLogsPanel audits={audits} detailLoading={detailLoading} onRefresh={() => loadAgentDetail(target.ip)} />}
        </div>
    );
}

function HostContext({ target, activeTab }) {
    const profileText = statusLabel(String(target.profile_status || 'unknown'), 'profile');
    const summary = [
        target.hostname || target.ip || '-',
        target.ip || '-',
        target.service_name || '-',
        `Native profiling ${profileText}`,
    ].join(' · ');
    return (
        <details style={S.contextDetails}>
            <summary style={S.contextSummary}>
                <span style={S.contextSummaryMain}>
                    <span style={S.contextSummaryTitle}>主机上下文</span>
                    <span style={S.contextSummaryText}>{summary}</span>
                </span>
                <span style={S.subtle}>详情</span>
            </summary>
            <div style={S.contextGrid}>
                <ContextItem label="host" value={target.hostname || '-'} />
                <ContextItem label="ip" value={target.ip || '-'} />
                <ContextItem label="service" value={target.service_name || '-'} />
                <ContextItem label="environment" value={target.environment || '-'} />
                <ContextItem label="按需采样 drop_agent" value={<StatusPill value={target.drop_agent_status} kind="drop" />} />
                <ContextItem label="Native Continuous Profiling" value={<StatusPill value={target.profile_status} kind="profile" />} />
                <ContextItem label="time range" value={activeTab === 'profiling' ? 'Tab 内选择' : '当前主机上下文'} />
                <ContextItem label="labels" value={labelSummary(target.labels)} wide />
            </div>
        </details>
    );
}

function DropAgentNotice({ target, activeTab }) {
    const profileReady = isProfileReady(target?.profile_status);
    if (activeTab === 'profiling' && profileReady) {
        return (
            <div style={S.info}>
                drop_agent 离线只影响按需采样创建；Native Continuous Profiling session 仍可查看整机 CPU 占用时长。
            </div>
        );
    }
    return (
        <div style={S.warn}>
            drop_agent 离线，暂不能新建按需采样；Native Continuous Profiling 是否可看取决于 session 状态。
        </div>
    );
}

function OverviewPanel({ target, agent, stat, detailLoading, capabilities, tasks: taskItems, schedules: scheduleItems, profileSummary, onRefresh, onTab }) {
    const profilingMsg = profileSummary?.empty
        ? (profileSummary.message || '无 Native profiling 数据')
        : `${profileSummary?.items?.length || 0} 个热点函数 · ${formatMetricValue(profileSummary?.total, profileSummary?.unit)}`;
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
                    <Metric label="深度采样窗口" value={scheduleItems.length} />
                    <Metric label="Native profiling" value={profilingMsg} />
                </div>
                <div style={S.capWrap}>
                    {capabilities.length === 0 ? <span style={S.subtle}>未声明采集能力</span> : capabilities.map(cap => <span key={cap} style={S.capPill}>{capabilityLabel(cap)}</span>)}
                </div>
            </section>

            <div className="performance-overview-grid" style={S.grid}>
                <section style={S.card}>
                    <div style={S.sectionHead}>
                        <h3 style={S.sectionTitle}>最近任务</h3>
                        <button style={S.btnSm} onClick={() => onTab('tasks')}>查看任务列表</button>
                    </div>
                    <MiniTaskList tasks={taskItems} />
                </section>
                <section style={S.card}>
                    <div style={S.sectionHead}>
                        <h3 style={S.sectionTitle}>最近深度采样窗口</h3>
                        <button style={S.btnSm} onClick={() => onTab('timeline')}>查看时间轴</button>
                    </div>
                    {scheduleItems.length === 0 ? <div style={S.empty}>该主机暂无周期性深度采样计划</div> : (
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
                    <h3 style={S.sectionTitle}>Native profiling 摘要</h3>
                    <button style={S.btnSm} onClick={() => onTab('profiling')}>进入 Native profiling</button>
                </div>
                {profileSummary?.empty ? <div style={S.empty}>{profileSummary.message || '暂无 Native profiling 数据'}</div> : <TopNTable data={profileSummary} compact />}
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
            const res = await tasks.list({ page, pageSize, target_ip: target.ip, owner_filter: 'all', status: statusFilter || undefined });
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
    const [customFrom, setCustomFrom] = useState('');
    const [customTo, setCustomTo] = useState('');
    const [appliedCustomFrom, setAppliedCustomFrom] = useState('');
    const [appliedCustomTo, setAppliedCustomTo] = useState('');
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

    const loadTimeline = useCallback(async (sid = masterTid, override = {}) => {
        if (!sid) return;
        setMasterTid(sid);
        setLoading(true);
        setError('');
        try {
            const opts = { ...timelineFilters() };
            const effectiveRange = override.range || range;
            if (effectiveRange === 'custom') {
                const from = override.from || appliedCustomFrom;
                const to = override.to || appliedCustomTo;
                if (!from || !to) {
                    setPoints([]);
                    setError('请选择自定义开始时间和结束时间后再查询');
                    return;
                }
                opts.from = from;
                opts.to = to;
            } else if (effectiveRange !== 'all') {
                const hours = Number(effectiveRange);
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
    }, [masterTid, range, appliedCustomFrom, appliedCustomTo, timelineFilters]);

    useEffect(() => { loadSchedules(); }, [loadSchedules]);
    useEffect(() => {
        if (!masterTid || range === 'custom') return;
        loadTimeline(masterTid);
    }, [masterTid, range, statusFilter, kindFilter, hasResultFilter, loadTimeline]);

    const applyCustomRange = useCallback(() => {
        if (!customFrom || !customTo) {
            setError('请选择自定义开始时间和结束时间后再查询');
            return;
        }
        const fromISO = localDateTimeToISO(customFrom);
        const toISO = localDateTimeToISO(customTo);
        if (!fromISO || !toISO) {
            setError('自定义时间格式无效');
            return;
        }
        if (new Date(toISO) < new Date(fromISO)) {
            setError('结束时间不能早于开始时间');
            return;
        }
        setRange('custom');
        setAppliedCustomFrom(fromISO);
        setAppliedCustomTo(toISO);
        loadTimeline(masterTid, { range: 'custom', from: fromISO, to: toISO });
    }, [customFrom, customTo, masterTid, loadTimeline]);

    const refreshTimeline = useCallback(() => {
        if (range === 'custom') {
            loadTimeline(masterTid, { range: 'custom' });
            return;
        }
        loadTimeline(masterTid);
    }, [range, masterTid, loadTimeline]);

    return (
        <>
            <section style={S.card}>
                <div style={S.sectionHead}>
                    <h3 style={S.sectionTitle}>该主机周期性深度采样时间轴</h3>
                    <span style={S.subtle}>只显示 target_ip = {target.ip} 的周期性深度采样窗口 · 时间按浏览器本地时区显示：{browserTimeZoneLabel()}</span>
                </div>
				{scheduleList.length === 0 ? <div style={S.empty}>该主机暂无周期性深度采样计划。点击“新建单次采样”并勾选周期性深度采样后会出现在这里。</div> : (
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
                                <option value="custom">自定义区间</option>
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
                            <button style={S.btnSm} onClick={refreshTimeline}>刷新</button>
                        </div>
                        {range === 'custom' && (
                            <div style={S.toolbar}>
                                <input
                                    type="datetime-local"
                                    style={S.input}
                                    value={customFrom}
                                    onChange={e => setCustomFrom(e.target.value)}
                                    aria-label="自定义开始时间"
                                />
                                <span style={S.subtle}>→</span>
                                <input
                                    type="datetime-local"
                                    style={S.input}
                                    value={customTo}
                                    onChange={e => setCustomTo(e.target.value)}
                                    aria-label="自定义结束时间"
                                />
                                <button style={S.btnSm} onClick={applyCustomRange}>查询</button>
                            </div>
                        )}
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
                            窗口 {formatDateTime(p.window_start || p.create_time)} → {formatDateTime(p.window_end || p.end_time) || '进行中'}
                        </div>
                    </div>
                ))}
            </div>
        </div>
    );
}

function HostProfilingPanel({ target, refreshToken }) {
	return <ContinuousSessionList target={target} refreshToken={refreshToken} />;
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
        <div className="table-scroll">
        <table style={S.table}>
            <thead>
                <tr>
                    <th style={S.th}>任务ID</th>
                    <th style={S.th}>名称</th>
                    {!compact && <th style={S.th}>采集器</th>}
                    <th style={S.th}>状态</th>
                    <th style={S.th}>创建时间</th>
                    <th style={S.th}>创建者</th>
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
                        <td style={S.td}>{task.user_name || '系统'}</td>
                        <td style={S.td}><Link to={`/task/result?tid=${task.tid}`} style={{ color: '#315efb', fontWeight: 700 }}>查看</Link></td>
                    </tr>
                ))}
            </tbody>
        </table>
        </div>
    );
}

function TopNTable({ data, loading, compact = false, profileURL }) {
    if (loading && !data) return <div style={S.empty}>正在查询 TopN...</div>;
    const items = data?.items || [];
    if (data?.empty || items.length === 0) {
        return <ProfileEmptyState profileURL={profileURL} compact={compact} />;
    }
    return (
        <div className="table-scroll">
        <table style={S.table}>
            <thead>
                <tr>
                    <th style={S.th}>函数</th>
                    <th style={S.th}>{metricColumnLabel(data.unit, '累计占用时长')}</th>
                    <th style={S.th}>{metricColumnLabel(data.unit, '自身占用时长')}</th>
                </tr>
            </thead>
            <tbody>
                {items.slice(0, compact ? 5 : 14).map((item, index) => (
                    <tr key={`${item.name}-${index}`}>
                        <td style={S.td} title={item.name}>{truncate(item.name, compact ? 44 : 36)}</td>
                        <td style={S.td}>{formatMetricValue(item.value, item.unit || data.unit)}</td>
                        <td style={S.td}>{formatMetricValue(item.self, item.unit || data.unit)}</td>
                    </tr>
                ))}
            </tbody>
        </table>
        </div>
    );
}

function ProfileEmptyState({ profileURL, compact = false }) {
    return (
        <div style={S.empty}>
            <div style={{ fontWeight: 700, color: '#475467', marginBottom: 6 }}>
                暂无 Native profiling 样本
            </div>
            {!compact && (
                <div style={{ ...S.subtle, marginBottom: 12 }}>
                    请切换时间范围或打开 Native profiling 标签页查看详情。
                </div>
            )}
            {profileURL && (
                <a href={profileURL} target="_blank" rel="noreferrer" style={S.btnSecondary}>打开 Profile 查询</a>
            )}
        </div>
    );
}

function MessagePage({ message }) {
    return <div style={S.container}><div style={S.error}>{message}</div><Link to="/" style={S.btnSecondary}>返回主机列表</Link></div>;
}

function ContextItem({ label, value, wide = false }) {
    return <div style={wide ? { ...S.contextItem, ...S.contextItemWide } : S.contextItem}><div style={S.contextLabel}>{label}</div><div style={S.contextValue}>{value}</div></div>;
}

function Metric({ label, value }) {
    return <div style={S.metric}><div style={S.metricLabel}>{label}</div><div style={S.metricValue}>{value}</div></div>;
}

function StatusPill({ value, kind = '' }) {
    const status = String(value || 'unknown');
    const color = isProfileReady(status) ? '#16a34a' : status === 'offline' ? '#dc2626' : (status === 'unconfigured' || status === 'no_session') ? '#64748b' : '#7c3aed';
    return <span style={{ ...S.badge, background: color, color: '#fff' }}>{statusLabel(status, kind)}</span>;
}

function isProfileReady(status) {
    return status === 'running' || status === 'online_with_samples';
}

function statusLabel(status, kind = '') {
    if (status === 'online_with_samples') return '有样本';
    if (status === 'online_no_samples') return '在线无样本';
    if (status === 'online') return '在线';
    if (status === 'offline') return kind === 'drop' ? '离线（仅影响按需采样）' : '离线';
    if (status === 'unconfigured') return '未配置';
    if (status === 'no_session') return '暂无 session';
    if (status === 'running') return '运行中';
    if (status === 'stopped') return '已停止';
    if (status === 'query_unsupported') return '查询不兼容';
    return status || '未知';
}

function labelSummary(labels) {
    const entries = Object.entries(labels || {});
    if (entries.length === 0) return 'node/job/env/instance 待接入';
    return entries.slice(0, 4).map(([k, v]) => `${k}=${v}`).join(', ');
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
