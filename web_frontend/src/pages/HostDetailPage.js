import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { Link, useParams, useSearchParams } from 'react-router-dom';
import { agents, continuous, profiles, schedules, storage, tasks } from '../api';
import CreateTaskModal from '../components/CreateTaskModal';
import CreateContinuousSessionModal from '../components/CreateContinuousSessionModal';
import ContinuousSessionList from '../components/ContinuousSessionList';
import ScheduleList from '../components/ScheduleList';
import Pagination from '../components/Pagination';
import TaskCancelButton from '../components/TaskCancelButton';
import { capabilityDescription, capabilityLabel, collectorLabelFromTask, parseStringList } from '../utils/collectors';
import { continuousStateColor, continuousStateLabel, decodeJSONField, formatRelativeTime, selectorIdentity, selectorModeLabel, signalLabel } from '../utils/continuous';
import { clampPercent, formatCapacity, formatCollectedAt, hostMetricAvailable, usageColor } from '../utils/hostMetrics';
import { computeStorageAlert, resolveStorageAlert } from '../utils/storageStatus';

const S = {
    container: { width: '100%', maxWidth: 1320, minWidth: 0, margin: '0 auto', padding: '22px 28px 36px', fontFamily: 'Arial, sans-serif', color: '#101828' },
    pageHead: { display: 'flex', justifyContent: 'space-between', gap: 16, alignItems: 'flex-end', marginBottom: 12 },
    eyebrow: { margin: '0 0 6px 0', color: '#667085', fontSize: 13 },
    title: { margin: 0, fontSize: 28, lineHeight: 1.2, letterSpacing: 0 },
    actions: { display: 'flex', gap: 10, flexWrap: 'wrap', justifyContent: 'flex-end' },
    card: { minWidth: 0, maxWidth: '100%', background: '#fff', borderRadius: 8, padding: 20, marginBottom: 16, border: '1px solid #e5e7eb', boxShadow: '0 1px 2px rgba(16,24,40,0.04)' },
    contextGrid: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(230px, 1fr))', gap: '14px 28px', padding: '14px', borderTop: '1px solid #f2f4f7' },
    contextItem: { minWidth: 0 },
    contextItemWide: { gridColumn: '1 / -1' },
    contextLabel: { color: '#667085', fontSize: 12, marginBottom: 5 },
    contextValue: { color: '#111827', fontSize: 14, fontWeight: 650, wordBreak: 'break-word', lineHeight: 1.45 },
    // 主机身份（操作系统/内核/架构/CPU 型号/核数）常显网格
    hostIdentityGrid: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))', gap: '12px 24px', padding: '14px', borderTop: '1px solid #f2f4f7' },
    identityTitle: { color: '#475467', fontSize: 12, fontWeight: 700, margin: '14px 0 0', padding: '0 14px' },
    // 主机上下文常显摘要卡 + 整机资源块
    contextCard: { background: '#fff', border: '1px solid #e5e7eb', borderRadius: 8, padding: '16px 18px', marginBottom: 16, boxShadow: '0 1px 2px rgba(16,24,40,0.04)' },
    contextHeader: { display: 'flex', justifyContent: 'space-between', gap: 16, alignItems: 'flex-start', flexWrap: 'wrap' },
    contextHeaderTitle: { color: '#111827', fontSize: 15, fontWeight: 700, marginBottom: 8 },
    contextHeaderMeta: { display: 'flex', gap: 12, alignItems: 'center', flexWrap: 'wrap', color: '#475467', fontSize: 13 },
    contextMetaItem: { display: 'inline-flex', alignItems: 'center', gap: 5, minWidth: 0 },
    contextLiveInfo: { display: 'flex', flexDirection: 'column', gap: 4, alignItems: 'flex-end', color: '#667085', fontSize: 12, whiteSpace: 'nowrap' },
    hostResourceGrid: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(320px, 1fr))', gap: 12, marginTop: 14 },
    hostResource: { background: '#fbfcfe', border: '1px solid #edf0f3', borderRadius: 6, padding: '12px 14px', minWidth: 0 },
    hostResourceHead: { display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', gap: 10, marginBottom: 8 },
    hostResourceTitle: { color: '#475467', fontSize: 12, fontWeight: 700 },
    hostResourcePct: { color: '#111827', fontSize: 20, fontWeight: 700, fontVariantNumeric: 'tabular-nums' },
    hostBarTrack: { height: 6, background: '#eef2f7', borderRadius: 999, overflow: 'hidden' },
    hostBar: { height: '100%', borderRadius: 999, transition: 'width 0.4s ease' },
    hostResourceValue: { marginTop: 6, color: '#667085', fontSize: 12, wordBreak: 'break-all' },
    hostMissingNote: { marginTop: 12, color: '#667085', fontSize: 12, background: '#f8fafc', border: '1px dashed #d0d7de', borderRadius: 6, padding: '8px 12px' },
    // 阶段 0：服务端存储压力紧凑提示（仅主机上下文卡片内，资源块上方）
    storageAlertWarning: { marginTop: 12, background: '#fffaeb', border: '1px solid #fedf89', color: '#b54708', borderRadius: 6, padding: '8px 12px', fontSize: 12, lineHeight: 1.5 },
    storageAlertDanger: { marginTop: 12, background: '#fff6f5', border: '1px solid #fda29b', color: '#b42318', borderRadius: 6, padding: '8px 12px', fontSize: 12, lineHeight: 1.5 },
    contextFooter: { display: 'flex', justifyContent: 'flex-end', marginTop: 12, borderTop: '1px solid #f2f4f7', paddingTop: 10 },
    contextToggle: { background: '#f8fafc', color: '#315efb', border: '1px solid #d0d7de', padding: '5px 10px', borderRadius: 6, cursor: 'pointer', fontSize: 12, fontWeight: 700 },
    // 采集能力：胶囊标签 + 能力说明紧凑网格
    capabilityPills: { display: 'flex', flexWrap: 'wrap', gap: '6px' },
    capabilityPill: { display: 'inline-flex', alignItems: 'center', padding: '2px 10px', borderRadius: 999, background: '#eef2ff', color: '#3730a3', fontSize: 12, lineHeight: '20px', whiteSpace: 'nowrap' },
    capabilityList: { display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(300px, 1fr))', gap: '4px 24px' },
    capabilityListItem: { fontSize: 12, lineHeight: '20px', minWidth: 0 },
    capabilityListName: { color: '#111827', fontWeight: 650 },
    capabilityListUsage: { color: '#667085' },
    tabBar: { display: 'flex', gap: 22, flexWrap: 'wrap', marginBottom: 16, borderBottom: '1px solid #e5e7eb' },
    tab: (active) => ({ padding: '10px 0 11px', borderRadius: 0, border: 'none', borderBottom: active ? '2px solid #315efb' : '2px solid transparent', background: 'transparent', color: active ? '#315efb' : '#667085', cursor: 'pointer', fontSize: 14, fontWeight: 700 }),
    sectionHead: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 12, marginBottom: 12 },
    sectionTitle: { margin: 0, fontSize: 18 },
    btn: { background: '#315efb', color: '#fff', border: 'none', padding: '9px 14px', borderRadius: 6, cursor: 'pointer', fontSize: 13, fontWeight: 700 },
    btnDisabled: { background: '#e5e7eb', color: '#667085', border: 'none', padding: '9px 14px', borderRadius: 6, cursor: 'not-allowed', fontSize: 13, fontWeight: 700 },
    btnSm: { background: '#f8fafc', color: '#475467', border: '1px solid #d0d7de', padding: '6px 10px', borderRadius: 6, cursor: 'pointer', fontSize: 12, textDecoration: 'none' },
    btnSecondary: { background: '#fff', color: '#315efb', border: '1px solid #c7d2fe', padding: '8px 12px', borderRadius: 6, cursor: 'pointer', fontSize: 13, fontWeight: 700, textDecoration: 'none' },
    btnDangerSm: { background: '#fff', color: '#b42318', border: '1px solid #fda29b', padding: '5px 10px', borderRadius: 6, cursor: 'pointer', fontSize: 12, fontWeight: 700 },
    mono: { maxWidth: 320, color: '#475467', fontSize: 12, fontFamily: 'ui-monospace,SFMono-Regular,Menlo,monospace', wordBreak: 'break-all' },
    badge: { display: 'inline-flex', alignItems: 'center', padding: '3px 9px', borderRadius: 999, fontSize: 12, fontWeight: 700 },
    metricGrid: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(130px, 1fr))', gap: 10 },
    metric: { background: '#f8fafc', border: '1px solid #edf0f3', borderRadius: 6, padding: 12 },
    metricLabel: { color: '#667085', fontSize: 12, marginBottom: 6 },
    metricValue: { fontSize: 18, fontWeight: 700, wordBreak: 'break-word' },
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
	{ id: 'tasks', label: '单次任务列表' },
	{ id: 'timeline', label: '周期任务' },
	{ id: 'profiling', label: '持续采集' },
    { id: 'logs', label: 'Agent 日志' },
];
const statusColors = { 0: '#ffc107', 1: '#2196f3', 2: '#4caf50', 3: '#f44336', 4: '#7c3aed' };
const statusNames = { 0: '待处理', 1: '执行中', 2: '已完成', 3: '失败', 4: '上传中' };

export default function HostDetailPage() {
    const { targetId: rawTargetId } = useParams();
    const [searchParams, setSearchParams] = useSearchParams();
    const targetId = decodeURIComponent(rawTargetId || '');
    const [targets, setTargets] = useState([]);
    const [agentDetail, setAgentDetail] = useState(null);
    const [hostTasks, setHostTasks] = useState([]);
    const [hostSchedules, setHostSchedules] = useState([]);
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
        const res = await tasks.list({ page: 1, pageSize: 5, target_ip: ip, owner_filter: 'all', task_scope: 'single' });
        if (res.code === 0) setHostTasks(res.data?.tasks || []);
    }, []);

    const loadHostSchedules = useCallback(async (ip) => {
        if (!ip) return;
        const res = await schedules.list();
        if (res.code === 0) {
            setHostSchedules((res.data?.schedules || []).filter(s => s.target_ip === ip));
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
    }, [target, loadAgentDetail, loadHostTasks, loadHostSchedules]);

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

            <HostContext target={target} activeTab={activeTab} agent={agent} stat={stat} capabilities={capabilities} />
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
                    tasks={hostTasks}
                    onRefresh={() => {
                        loadAgentDetail(target.ip);
                        loadHostTasks(target.ip);
                        loadHostSchedules(target.ip);
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

// 服务端存储压力轮询：每 30 秒刷新一次；接口临时失败时不显示错误大横幅，
// 只保留上一次成功状态最多 90 秒，超时后自动隐藏。
function useStorageStatus(intervalMs = 30000, staleMs = 90000) {
    const [lastSuccess, setLastSuccess] = useState(null); // { alert, fetchedAt }
    const [now, setNow] = useState(() => Date.now());

    useEffect(() => {
        let disposed = false;
        const load = async () => {
            try {
                const res = await storage.status();
                if (disposed) return;
                if (res && res.code === 0 && res.data) {
                    setLastSuccess({ alert: computeStorageAlert(res.data), fetchedAt: Date.now() });
                }
            } catch (err) {
                // 接口临时失败：保留上一次成功状态，不展示错误横幅
            }
        };
        load();
        const timer = setInterval(load, intervalMs);
        return () => { disposed = true; clearInterval(timer); };
    }, [intervalMs]);

    // 独立心跳驱动过期判定：即使接口持续失败，超过 staleMs 也会自动隐藏。
    useEffect(() => {
        const tick = setInterval(() => setNow(Date.now()), Math.min(intervalMs, staleMs));
        return () => clearInterval(tick);
    }, [intervalMs, staleMs]);

    return resolveStorageAlert(lastSuccess, now, staleMs);
}

function HostContext({ target, activeTab, agent, stat, capabilities }) {
    const [expanded, setExpanded] = useState(false);
    const storageAlert = useStorageStatus();
    const host = stat?.host || null;
    const hostMetadata = stat?.host_metadata || null;
    const online = agent?.online === true || target?.drop_agent_status === 'online';
    const sourceLabel = stat?.source === 'grpc' ? '实时 gRPC' : '数据库快照';
    const collectedAt = host ? formatCollectedAt(host.collected_at) : '--';

    // 每个维度独立降级：后端 *_available=false 时该维度显示 "--"，
    // 不因某一个维度失败而让整个区域消失。
    const cpuAvailable = hostMetricAvailable(host, 'cpu');
    const memAvailable = hostMetricAvailable(host, 'memory');
    const diskAvailable = hostMetricAvailable(host, 'disk');

    // host 整体缺失：区分"Agent 离线"与"当前 Agent 版本暂不支持"
    const missingReason = host
        ? null
        : (online
            ? '当前 Agent 版本暂不支持整机资源上报，升级按需采集器后即可查看 CPU / 内存 / 系统盘。'
            : '按需采集器离线，暂无整机资源。');

    // 主机元数据缺失：显示明确原因，不显示 0 冒充有效值
    const metaMissingReason = hostMetadata
        ? null
        : (online
            ? '当前 Agent 版本暂未上报主机信息（操作系统 / 内核 / CPU 型号）。'
            : '按需采集器离线，主机信息为最后已知值或暂未上报。');

    const osLabel = hostMetadata
        ? [hostMetadata.os_name, hostMetadata.os_version].filter(Boolean).join(' ') || '--'
        : '--';

    return (
        <section style={S.contextCard}>
            <div style={S.contextHeader}>
                <div style={{ minWidth: 0 }}>
                    <div style={S.contextHeaderTitle}>主机信息</div>
                    <div style={S.contextHeaderMeta}>
                        <span style={S.contextMetaItem} title="主机名">{target.hostname || target.ip || '-'}</span>
                        <span style={S.contextMetaItem} title="IP">{target.ip || '-'}</span>
                        <span style={S.contextMetaItem} title="按需采集器（drop_agent）状态">按需采集器 <StatusPill value={target.drop_agent_status} kind="drop" /></span>
                        <span style={S.contextMetaItem} title="持续采集状态">持续采集 <StatusPill value={target.profile_status} kind="profile" /></span>
                    </div>
                </div>
                <div style={S.contextLiveInfo}>
                    <span>实时数据 · 10 秒刷新</span>
                    <span>最近采集 {collectedAt}</span>
                </div>
            </div>

            {/* 阶段 0：服务端存储压力紧凑提示（normal 时 computeStorageAlert 返回 null，不渲染） */}
            {storageAlert && <StoragePressureBanner alert={storageAlert} />}

            {/* 主机身份：操作系统 / 内核 / 架构 / CPU 型号 / 核数 */}
            <div style={S.identityTitle}>主机身份</div>
            <div style={S.hostIdentityGrid}>
                <ContextItem label="操作系统" value={osLabel} />
                <ContextItem label="内核版本" value={hostMetadata?.kernel_version || '--'} />
                <ContextItem label="架构" value={hostMetadata?.architecture || '--'} />
                <ContextItem label="CPU 型号" value={hostMetadata?.cpu_model || '--'} />
                <ContextItem label="CPU 核数" value={hostMetadata?.cpu_cores ? `${hostMetadata.cpu_cores} 核` : '--'} />
            </div>
            {metaMissingReason && <div style={S.hostMissingNote}>{metaMissingReason}</div>}

            {/* 主机资源块：无数据时保留稳定布局，数值显示 "--" */}
            <div style={S.identityTitle}>主机资源</div>
            <div style={S.hostResourceGrid}>
                <HostResourceBlock
                    title="整机 CPU"
                    percent={host?.cpu_percent}
                    available={cpuAvailable}
                    detail={cpuAvailable ? `使用率（0–100%）· 采集于 ${collectedAt}` : null}
                />
                <HostResourceBlock
                    title="整机内存"
                    percent={host?.memory_percent}
                    available={memAvailable}
                    detail={memAvailable ? `已用 ${formatCapacity(host.memory_used_bytes)} / 总量 ${formatCapacity(host.memory_total_bytes)} · 采集于 ${collectedAt}` : null}
                />
                <HostResourceBlock
                    title={`系统盘 ${host?.disk_mount || '/'}`}
                    percent={host?.disk_percent}
                    available={diskAvailable}
                    detail={diskAvailable ? `已用 ${formatCapacity(host.disk_used_bytes)} / 总量 ${formatCapacity(host.disk_total_bytes)} · 采集于 ${collectedAt}` : null}
                />
            </div>

            {missingReason && <div style={S.hostMissingNote}>{missingReason}</div>}

            <div style={S.contextFooter}>
                <button style={S.contextToggle} onClick={() => setExpanded(prev => !prev)}>
                    {expanded ? '收起采集器状态' : '展开采集器状态'}
                </button>
            </div>

            {expanded && (
                <div style={S.contextGrid}>
                    <ContextItem label="Agent 版本" value={agent.version || '-'} />
                    <ContextItem label="在线状态" value={online ? '在线' : '离线'} />
                    <ContextItem label="最近心跳" value={agent.last_seen ? formatRelativeTime(agent.last_seen) : '--'} />
                    <ContextItem label="按需采集器" value={<StatusPill value={target.drop_agent_status} kind="drop" />} />
                    <ContextItem label="持续采集" value={<StatusPill value={target.profile_status} kind="profile" />} />
                    <ContextItem label="采集能力" value={<CapabilityPills capabilities={capabilities} />} wide />
                    <ContextItem label="能力说明" value={<CapabilityList capabilities={capabilities} />} wide />
                    <ContextItem label="时间范围" value={activeTab === 'profiling' ? 'Tab 内选择' : '当前主机上下文'} />
                    <ContextItem label="数据来源" value={sourceLabel} />
                </div>
            )}
        </section>
    );
}

// 采集能力：胶囊标签流式布局，每个能力一个标签，hover 显示用途说明。
function CapabilityPills({ capabilities }) {
    const list = Array.isArray(capabilities) ? capabilities : [];
    if (list.length === 0) return '未声明采集能力';
    return (
        <div style={S.capabilityPills}>
            {list.map((cap, idx) => {
                const desc = capabilityDescription(cap);
                const label = desc ? desc.name : capabilityLabel(cap);
                return (
                    <span key={`${cap}-${idx}`} style={S.capabilityPill} title={desc ? `${desc.name} — ${desc.usage}` : label}>
                        {label}
                    </span>
                );
            })}
        </div>
    );
}

// 能力说明：按中文名去重后的紧凑网格列表（"名称 — 用途"）。
function CapabilityList({ capabilities }) {
    const list = Array.isArray(capabilities) ? capabilities : [];
    if (list.length === 0) return '未声明采集能力';
    const seen = new Set();
    const items = [];
    for (const cap of list) {
        const desc = capabilityDescription(cap);
        const name = desc ? desc.name : capabilityLabel(cap);
        if (seen.has(name)) continue;
        seen.add(name);
        items.push(desc ? { name, usage: desc.usage } : { name, usage: '' });
    }
    return (
        <div style={S.capabilityList}>
            {items.map(item => (
                <div key={item.name} style={S.capabilityListItem}>
                    <span style={S.capabilityListName}>{item.name}</span>
                    {item.usage ? <span style={S.capabilityListUsage}> — {item.usage}</span> : null}
                </div>
            ))}
        </div>
    );
}

// 阶段 0：服务端存储压力紧凑提示条。warning 用浅黄，critical/emergency/unknown 用浅红；
// 只占一行，不遮挡操作。
function StoragePressureBanner({ alert }) {
    return (
        <div data-testid="storage-pressure-banner" style={alert.tone === 'danger' ? S.storageAlertDanger : S.storageAlertWarning} role="status">
            {alert.message}
        </div>
    );
}

// 单个整机资源块（CPU / 内存 / 系统盘）。细进度条颜色按使用率：
// <70% 品牌蓝，70-89% 警告橙，>=90% 危险红。
function HostResourceBlock({ title, percent, available, detail }) {
    const pct = clampPercent(percent);
    const color = usageColor(pct);
    return (
        <div style={S.hostResource}>
            <div style={S.hostResourceHead}>
                <span style={S.hostResourceTitle}>{title}</span>
                <span style={S.hostResourcePct}>{available ? `${formatMetric(pct, 1)}%` : '--'}</span>
            </div>
            <div style={S.hostBarTrack}>
                {available ? <div style={{ ...S.hostBar, width: `${pct}%`, background: color }} /> : null}
            </div>
            <div style={S.hostResourceValue}>{available && detail ? detail : '--'}</div>
        </div>
    );
}

function DropAgentNotice({ target, activeTab }) {
    const profileReady = isProfileReady(target?.profile_status);
    if (activeTab === 'profiling' && profileReady) {
        return (
            <div style={S.info}>
                按需采集器离线只影响按需采样创建；持续采集 session 仍可查看整机 CPU 占用时长。
            </div>
        );
    }
    return (
        <div style={S.warn}>
            按需采集器离线，暂不能新建按需采样；持续采集是否可看取决于 session 状态。
        </div>
    );
}

function OverviewPanel({ target, agent, stat, detailLoading, tasks: taskItems, onRefresh, onTab }) {
    const [diagOpen, setDiagOpen] = useState(false);
    const [sessions, setSessions] = useState([]);
    const [sessionsLoading, setSessionsLoading] = useState(true);
    const [stopping, setStopping] = useState('');

    const loadSessions = useCallback(async (silent = false) => {
        if (!target?.ip) return;
        if (!silent) setSessionsLoading(true);
        try {
            const res = await continuous.sessions({ target_ip: target.ip, page_size: 20 });
            if (res.code === 0) {
                setSessions(res.data?.sessions || []);
            }
        } catch (err) {
            console.error('加载运行中的持续采集失败:', err);
        } finally {
            if (!silent) setSessionsLoading(false);
        }
    }, [target?.ip]);

    useEffect(() => { loadSessions(); }, [loadSessions]);
    useEffect(() => {
        const timer = window.setInterval(() => loadSessions(true), 10000);
        return () => window.clearInterval(timer);
    }, [loadSessions]);

    const stopSession = async (session) => {
        if (!window.confirm(`停止持续采集“${session.name}”？停止后不会自动恢复。`)) return;
        setStopping(session.sid);
        try {
            const res = await continuous.stopSession(session.sid);
            if (res.code !== 0) throw new Error(res.message || '停止失败');
            await loadSessions(true);
        } catch (err) {
            console.error('停止持续采集失败:', err);
        } finally {
            setStopping('');
        }
    };

    // 运行状态：指标新鲜度（主机指标 90 秒内视为新鲜）
    const hostCollectedMs = stat?.host?.collected_at ? new Date(stat.host.collected_at).getTime() : null;
    const hostFresh = hostCollectedMs ? (Date.now() - hostCollectedMs) <= 90000 : null;
    const runningSessions = sessions.filter(session => session.desired_state === 'running');
    const successCount = taskItems.filter(t => t.status === 2).length;
    const failedCount = taskItems.filter(t => t.status === 3).length;

    return (
        <>
            <section style={S.card}>
                <div style={S.sectionHead}>
                    <h3 style={S.sectionTitle}>运行状态</h3>
                    <button style={S.btnSecondary} onClick={onRefresh} disabled={detailLoading}>{detailLoading ? '刷新中' : '刷新'}</button>
                </div>
                <div style={S.metricGrid}>
                    <Metric
                        label="当前是否可采集"
                        value={
                            <span style={{ color: agent.online ? '#16a34a' : (target.drop_agent_status === 'offline' ? '#dc2626' : '#64748b'), fontWeight: 700 }}>
                                {agent.online ? '可采集' : statusLabel(target.drop_agent_status, 'drop')}
                            </span>
                        }
                    />
                    <Metric label="Agent 版本" value={agent.version || '--'} />
                    <Metric label="最近心跳" value={agent.last_seen ? formatRelativeTime(agent.last_seen) : '--'} />
                    <Metric label="主机指标采集" value={hostCollectedMs ? formatRelativeTime(stat.host.collected_at) : '--'} />
                    <Metric
                        label="指标新鲜度"
                        value={hostFresh === null ? '--' : <span style={{ color: hostFresh ? '#16a34a' : '#dc2626', fontWeight: 700 }}>{hostFresh ? '数据新鲜' : '数据已过期'}</span>}
                    />
                    <Metric label="最近单次任务" value={taskItems.length} />
                    <Metric label="运行中持续采集" value={runningSessions.length} />
                    <Metric label="任务成功 / 失败" value={`${successCount} / ${failedCount}`} />
                </div>
                <div style={{ ...S.subtle, marginTop: 10 }}>数据来源：{stat.source === 'grpc' ? '实时 gRPC' : '数据库快照'}</div>

                {/* 采集器诊断：Agent 自身资源不再作为主指标，折叠展示 */}
                <div style={{ marginTop: 12, borderTop: '1px solid #f2f4f7', paddingTop: 10 }}>
                    <button style={S.contextToggle} onClick={() => setDiagOpen(prev => !prev)}>
                        {diagOpen ? '收起采集器诊断' : '展开采集器诊断'}
                    </button>
                    {diagOpen && (
                        <div style={S.metricGrid}>
                            <Metric label="采集器进程 CPU" value={`${formatMetric(stat.cpu_percent, 1)}%`} />
                            <Metric label="采集器进程内存" value={formatCapacity(stat.memory_kb * 1024)} />
                            <Metric label="读取吞吐" value={`${formatMetric(stat.read_kb_per_s, 0)} KB/s`} />
                            <Metric label="写入吞吐" value={`${formatMetric(stat.write_kb_per_s, 0)} KB/s`} />
                        </div>
                    )}
                </div>
            </section>

            <section style={S.card}>
                <div style={S.sectionHead}>
                    <h3 style={S.sectionTitle}>最近单次任务</h3>
                    <button style={S.btnSm} onClick={() => onTab('tasks')}>查看任务列表</button>
                </div>
                <MiniTaskList tasks={taskItems} onCancelled={onRefresh} />
            </section>

            <section style={S.card}>
                <div style={S.sectionHead}>
                    <h3 style={S.sectionTitle}>最近周期任务</h3>
                    <button style={S.btnSm} onClick={() => onTab('timeline')}>查看周期任务</button>
                </div>
                <ScheduleList
                    compact
                    bare
                    targetIp={target.ip}
                    detailPrefix={`/hosts/${encodeURIComponent(target.id)}/schedules`}
                />
            </section>

            <RunningSessionsCard
                target={target}
                onTab={onTab}
                sessions={sessions}
                loading={sessionsLoading}
                stopping={stopping}
                onStop={stopSession}
            />
        </>
    );
}

// ============================================================
// 概览卡：运行中的持续采集任务（仅概览页使用，不改动"持续采集"Tab）
// 数据与字段格式与持续采集页（ContinuousSessionList）保持一致。
// 数据加载由 OverviewPanel 统一负责，本组件为纯展示。
// ============================================================
function RunningSessionsCard({ target, onTab, sessions, loading, stopping, onStop }) {
    const activeSessions = sessions.filter(session => session.desired_state === 'running');
    const running = activeSessions.slice(0, 5);

    return (
        <section style={S.card}>
            <div style={S.sectionHead}>
                <h3 style={S.sectionTitle}>运行中的持续采集</h3>
                <div style={{ display: 'flex', gap: 10, alignItems: 'center', flexWrap: 'wrap' }}>
                    <span style={S.subtle}>共 {activeSessions.length} 个</span>
                    <button style={S.btnSm} onClick={() => onTab('profiling')}>进入持续采集</button>
                </div>
            </div>
            {loading && sessions.length === 0 ? (
                <div style={S.empty}>正在加载运行中的持续采集...</div>
            ) : running.length === 0 ? (
                <div style={S.empty}>
                    <div style={{ fontWeight: 700, color: '#475467', marginBottom: 6 }}>暂无运行中的持续采集任务</div>
                    <div style={{ ...S.subtle, marginBottom: 12 }}>可新建整机或进程持续采集，运行状态会实时显示在这里。</div>
                    <button style={S.btnSecondary} onClick={() => onTab('profiling')}>进入持续采集</button>
                </div>
            ) : (
                <div className="table-scroll">
                    <table style={S.table}>
                        <thead>
                            <tr>
                                <th style={S.th}>名称</th>
                                <th style={S.th}>范围与目标</th>
                                <th style={S.th}>状态</th>
                                <th style={S.th}>信号</th>
                                <th style={S.th}>最近上传</th>
                                <th style={S.th}>操作</th>
                            </tr>
                        </thead>
                        <tbody>
                            {running.map(session => {
                                const state = session.observed_state || 'pending';
                                const [background, color] = continuousStateColor(state);
                                const signals = decodeJSONField(session.signals, ['cpu_profile']);
                                const active = decodeJSONField(session.active_processes, []);
                                const isProcess = session.scope === 'process';
                                const isDegraded = session.continuity_mode === 'degraded' && session.desired_state === 'running';
                                const detailUrl = `/hosts/${encodeURIComponent(target.id)}/continuous/${encodeURIComponent(session.sid)}`;
                                return (
                                    <tr key={session.sid}>
                                        <td style={S.td}>
                                            <Link to={detailUrl} style={S.linkStrong}>{session.name}</Link>
                                            <div style={S.subtle}>{shortSID(session.sid)}</div>
                                        </td>
                                        <td style={S.td}>
                                            {isProcess ? <strong>进程 · {selectorModeLabel(selectorIdentity(session).mode)} · {active.length} 个活动实例</strong> : <strong>整机</strong>}
                                            {session.selector_exe ? <div style={S.mono} title={session.selector_exe}>{session.selector_exe}</div> : null}
                                            <div style={S.subtle}>样本 {formatCount(session.sample_count)}</div>
                                        </td>
                                        <td style={S.td}>
                                            <span style={{ ...S.badge, background, color }}>{continuousStateLabel(state)}</span>
                                            {isDegraded && <div style={{ ...S.subtle, color: '#b54708', marginTop: 4 }}>降级连续性</div>}
                                        </td>
                                        <td style={S.td}>
                                            <div style={{ display: 'flex', flexWrap: 'wrap', gap: 4 }}>
                                                {signals.map(sig => <span key={sig} style={S.capPill}>{signalLabel(sig)}</span>)}
                                            </div>
                                        </td>
                                        <td style={S.td}>{formatRelativeTime(session.last_upload_at)}</td>
                                        <td style={S.td}>
                                            <Link style={S.linkStrong} to={detailUrl}>查看</Link>
                                            {session.desired_state === 'running' && session.can_manage && (
                                                <button style={{ ...S.btnDangerSm, marginLeft: 8 }} disabled={stopping === session.sid} onClick={() => onStop(session)}>
                                                    {stopping === session.sid ? '停止中' : '停止'}
                                                </button>
                                            )}
                                        </td>
                                    </tr>
                                );
                            })}
                        </tbody>
                    </table>
                </div>
            )}
        </section>
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
            const res = await tasks.list({ page, pageSize, target_ip: target.ip, owner_filter: 'all', status: statusFilter || undefined, task_scope: 'single' });
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
            {loading && taskList.length === 0 ? <div style={S.loading}>加载任务...</div> : <TaskTable tasks={taskList} onCancelled={loadTasks} />}
            <Pagination page={page} totalPages={totalPages} onPageChange={setPage} />
        </section>
    );
}

function HostTimelinePanel({ target }) {
    return (
        <ScheduleList
            targetIp={target.ip}
            detailPrefix={`/hosts/${encodeURIComponent(target.id)}/schedules`}
        />
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

function MiniTaskList({ tasks: taskItems, onCancelled }) {
    if (taskItems.length === 0) return <div style={S.empty}>该主机暂无按需任务</div>;
    return <TaskTable tasks={taskItems.slice(0, 5)} compact onCancelled={onCancelled} />;
}

function TaskTable({ tasks: taskItems, compact = false, onCancelled }) {
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
                        <td style={S.td}>
                            <Link to={`/task/result?tid=${task.tid}`} style={{ color: '#315efb', fontWeight: 700, marginRight: 10 }}>查看</Link>
                            <TaskCancelButton tid={task.tid} status={task.status} canManage={task.can_manage} onCancelled={onCancelled} />
                        </td>
                    </tr>
                ))}
            </tbody>
        </table>
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

function shortSID(value) {
    return String(value || '').length > 18 ? `${value.slice(0, 10)}...${value.slice(-4)}` : value;
}

function formatCount(value) {
    const count = Number(value) || 0;
    if (count >= 1000000) return `${(count / 1000000).toFixed(1)}M`;
    if (count >= 1000) return `${(count / 1000).toFixed(1)}K`;
    return String(Math.round(count));
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
