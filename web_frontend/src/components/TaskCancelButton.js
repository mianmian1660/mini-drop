// ============================================================
// components/TaskCancelButton.js — 任务"停止"按钮
// ============================================================
// 样式对齐 ContinuousSessionList 里的停止按钮（纯文字、无边框、红色）
// 只在任务处于活跃状态（待处理/执行中/上传中）时渲染，POST /api/v1/tasks/:tid/cancel
// ============================================================

import React, { useState } from 'react';
import { tasks } from '../api';

const ACTIVE_STATUSES = new Set([0, 1, 4]); // 待处理 / 执行中 / 上传中

export default function TaskCancelButton({ tid, status, canManage = true, onCancelled, style }) {
    const [stopping, setStopping] = useState(false);

    if (!canManage || !ACTIVE_STATUSES.has(status)) return null;

    const handleCancel = async () => {
        if (!window.confirm(`确定停止任务 ${tid} 吗？`)) return;
        setStopping(true);
        try {
            const res = await tasks.cancel(tid);
            if (res.code !== 0) throw new Error(res.message || '停止失败');
            onCancelled?.();
        } catch (err) {
            alert('停止失败: ' + (err.response?.data?.message || err.message || '未知错误'));
        } finally {
            setStopping(false);
        }
    };

    return (
        <button
            style={{ color: '#b42318', background: 'transparent', border: 0, padding: 0, fontWeight: 700, cursor: 'pointer', fontSize: 13, ...style }}
            disabled={stopping}
            onClick={handleCancel}
        >
            {stopping ? '停止中' : '停止'}
        </button>
    );
}
