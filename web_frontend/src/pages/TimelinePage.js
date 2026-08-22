// ============================================================
// pages/TimelinePage.js — 周期性深度采样时间轴（旧兼容入口 /timeline）
// ============================================================
// 展示全局周期任务列表（与主机周期 Tab 共用 ScheduleList 组件），
// 点击周期任务进入独立时间轴详情页 /schedules/:sid。
// ============================================================

import React from 'react';
import { Link } from 'react-router-dom';
import ScheduleList from '../components/ScheduleList';

const S = {
    container: { width: '100%', maxWidth: 1200, minWidth: 0, margin: '0 auto', padding: 20, fontFamily: 'Arial, sans-serif' },
    card: { minWidth: 0, maxWidth: '100%', background: '#fff', borderRadius: 8, padding: 20, marginBottom: 16, border: '1px solid #e5e7eb', boxShadow: '0 1px 2px rgba(16,24,40,.04)' },
    title: { margin: 0, fontSize: 22 },
    hint: { fontSize: 12, color: '#667085', marginTop: 6 },
    btnSm: { background: '#f8fafc', color: '#475467', border: '1px solid #d0d7de', padding: '5px 10px', borderRadius: 6, cursor: 'pointer', fontSize: 12, fontWeight: 700, textDecoration: 'none' },
};

export default function TimelinePage() {
    return (
        <div style={S.container}>
            <h2 style={S.title}>周期性深度采样时间轴</h2>

            <div style={S.card}>
                <h3 style={{ marginTop: 0, marginBottom: 6 }}>请选择周期任务进入独立时间轴</h3>
                <p style={S.hint}>
                    点击下方周期任务的名称或“查看”进入该计划的独立时间轴详情页，查看历史采集窗口、区间查询、基线对比等。
                    时间轴同样已迁入具体主机页面（主机「周期任务」Tab）。
                </p>
                <Link to="/" style={{ ...S.btnSm, textDecoration: 'none' }}>返回主机列表</Link>
            </div>

            <ScheduleList detailPrefix="/schedules" />
        </div>
    );
}



