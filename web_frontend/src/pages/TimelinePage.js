// ============================================================
// pages/TimelinePage.js — Continuous Profiling 时间轴
// 自动加载所有定时任务，点击即可查看历史采集窗口
// ============================================================

import React, { useState, useEffect, useCallback } from 'react';
import { Link } from 'react-router-dom';
import { tasks, schedules } from '../api';

const S = {
    container: { maxWidth: 1200, margin: '0 auto', padding: 20, fontFamily: 'Arial, sans-serif' },
    card: { background: '#fff', borderRadius: 8, padding: 24, marginBottom: 16, boxShadow: '0 2px 8px rgba(0,0,0,0.1)' },
    btn: { background: '#4a6cf7', color: '#fff', border: 'none', padding: '8px 16px', borderRadius: 6, cursor: 'pointer', fontSize: 13 },
    btnSm: { background: '#e0e0e0', color: '#333', border: 'none', padding: '4px 10px', borderRadius: 4, cursor: 'pointer', fontSize: 12, marginRight: 6 },
    timeline: { position: 'relative', paddingLeft: 30, borderLeft: '3px solid #4a6cf7', marginLeft: 10 },
    point: (st) => ({
        position: 'relative', marginBottom: 20, padding: '10px 16px',
        background: st === 2 ? '#e8f5e9' : st === 3 ? '#ffebee' : st === 1 ? '#e3f2fd' : '#f5f5f5',
        borderRadius: 6, border: '1px solid #eee',
    }),
    dot: (st) => ({
        position: 'absolute', left: -39, top: 12, width: 14, height: 14, borderRadius: '50%',
        background: st === 2 ? '#4caf50' : st === 3 ? '#f44336' : st === 1 ? '#2196f3' : '#ccc',
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

const ST = { 0: '待处理', 1: '执行中', 2: '已完成', 3: '失败' };

export default function TimelinePage() {
    const [schList, setSchList] = useState([]);
    const [masterTid, setMasterTid] = useState('');
    const [points, setPoints] = useState([]);
    const [loading, setLoading] = useState(false);
    const [schLoading, setSchLoading] = useState(true);
    const [error, setError] = useState('');

    // 加载定时任务列表
    useEffect(() => {
        schedules.list().then(r => {
            if (r.code === 0) setSchList(r.data?.schedules || []);
        }).catch(() => { }).finally(() => setSchLoading(false));
    }, []);

    const loadTimeline = useCallback(async (sid) => {
        if (!sid) return;
        setMasterTid(sid);
        setLoading(true); setError('');
        try {
            const r = await tasks.timeline(sid);
            if (r.code === 0) setPoints(r.data?.points || []);
            else setError(r.message || '查询失败');
        } catch (e) { setError('请求失败: ' + (e.message || '')); }
        finally { setLoading(false); }
    }, []);

    // 自动轮询
    useEffect(() => {
        const hasRunning = points.some(p => p.status < 2);
        if (!hasRunning || !masterTid) return;
        const iv = setInterval(() => loadTimeline(masterTid), 5000);
        return () => clearInterval(iv);
    }, [points, masterTid, loadTimeline]);

    const refreshSchedules = () => {
        setSchLoading(true);
        schedules.list().then(r => {
            if (r.code === 0) setSchList(r.data?.schedules || []);
        }).catch(() => { }).finally(() => setSchLoading(false));
    };

    const toggleSchedule = async (sid, e) => {
        e.stopPropagation();  // 阻止触发行点击
        try {
            await fetch('/api/v1/schedule/' + sid + '/toggle', {
                method: 'POST',
                headers: { 'Drop_user_uid': 'demo', 'Drop_user_name': 'demo' }
            });
            refreshSchedules();
        } catch (err) { alert('操作失败: ' + err.message); }
    };

    const deleteSchedule = async (sid, e) => {
        e.stopPropagation();
        if (!window.confirm('确定删除此定时任务？相关采集记录保留。')) return;
        try {
            await fetch('/api/v1/schedule/' + sid, {
                method: 'DELETE',
                headers: { 'Drop_user_uid': 'demo', 'Drop_user_name': 'demo' }
            });
            refreshSchedules();
            if (masterTid === sid) { setMasterTid(''); setPoints([]); }
        } catch (err) { alert('删除失败: ' + err.message); }
    };

    return (
        <div style={S.container}>
            <h2>📊 Continuous Profiling 时间轴</h2>

            {/* 定时任务列表 */}
            <div style={S.card}>
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 12 }}>
                    <h3 style={{ margin: 0 }}>定时任务列表</h3>
                    <button style={S.btnSm} onClick={refreshSchedules}>🔄 刷新</button>
                </div>
                {schLoading ? <p style={S.loading}>加载中...</p>
                    : schList.length === 0 ? (
                        <div style={{ textAlign: 'center', padding: 20, color: '#999' }}>
                            <p>暂无定时任务</p>
                            <p style={S.hint}>💡 在主页新建任务时勾选"持续采集"即可创建</p>
                        </div>
                    ) : (
                        schList.map(sch => (
                            <div key={sch.sid} style={S.schRow(masterTid === sch.sid)}
                                onClick={() => loadTimeline(sch.sid)}>
                                <div style={{ flex: 1 }}>
                                    <strong>{sch.name}</strong>
                                    <span style={{ marginLeft: 10, fontSize: 12, color: '#888' }}>{sch.cron_expr}</span>
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
                                    <button style={{ ...S.btnSm, background: sch.enabled ? '#ff9800' : '#4caf50', color: '#fff' }}
                                        onClick={(e) => toggleSchedule(sch.sid, e)}>
                                        {sch.enabled ? '⏸ 停止' : '▶ 启用'}
                                    </button>
                                    <button style={{ ...S.btnSm, background: '#f44336', color: '#fff' }}
                                        onClick={(e) => deleteSchedule(sch.sid, e)}>
                                        🗑 删除
                                    </button>
                                </div>
                            </div>
                        ))
                    )}
            </div>

            {/* 时间轴 */}
            {loading && <div style={S.loading}>⏳ 加载时间轴...</div>}
            {error && <div style={{ ...S.loading, color: '#f44' }}>❌ {error}</div>}

            {!loading && points.length > 0 && (
                <div style={S.card}>
                    <h3>历史采集 ({points.length} 个窗口) — {masterTid}</h3>
                    <div style={S.timeline}>
                        {points.map((p, i) => (
                            <div key={p.tid} style={S.point(p.status)}>
                                <div style={S.dot(p.status)} />
                                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                                    <div>
                                        <strong>{i + 1}. {p.name || p.tid}</strong>
                                        <span style={{
                                            marginLeft: 10, padding: '2px 8px', borderRadius: 10, fontSize: 11, fontWeight: 'bold',
                                            background: p.status === 2 ? '#4caf50' : p.status === 3 ? '#f44336' : p.status === 1 ? '#2196f3' : '#ffc107',
                                            color: '#fff'
                                        }}>{ST[p.status] || '未知'}</span>
                                        {p.has_result && <span style={{ marginLeft: 6, fontSize: 11, color: '#4caf50' }}>✅ 有结果</span>}
                                    </div>
                                    <Link to={`/task/result?tid=${p.tid}`}
                                        style={{ color: '#4a6cf7', fontSize: 13, fontWeight: 'bold', textDecoration: 'none' }}>
                                        查看详情 →
                                    </Link>
                                </div>
                                <div style={{ fontSize: 11, color: '#999', marginTop: 4 }}>
                                    {new Date(p.create_time).toLocaleString('zh-CN')}
                                    {p.end_time && ` → ${new Date(p.end_time).toLocaleString('zh-CN')}`}
                                </div>
                            </div>
                        ))}
                    </div>
                </div>
            )}

            {!loading && masterTid && points.length === 0 && !error && (
                <div style={{ ...S.loading, color: '#888' }}>该定时任务暂无子任务记录</div>
            )}
        </div>
    );
}
