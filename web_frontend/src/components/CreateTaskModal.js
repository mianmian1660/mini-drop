// ============================================================
// components/CreateTaskModal.js — 新建任务弹窗
// ============================================================
// 功能：填写采样参数 → POST /api/v1/tasks → 通知父组件刷新
// ============================================================

import React, { useState } from 'react';
import { tasks } from '../api';

const styles = {
    card: { background: '#f8f9ff', borderRadius: 8, padding: 24, marginBottom: 16, border: '1px solid #e0e4ff' },
    input: { width: '100%', padding: '8px 12px', border: '1px solid #ddd', borderRadius: 4, fontSize: 14, marginBottom: 12, boxSizing: 'border-box' },
    label: { display: 'block', marginBottom: 4, fontWeight: 'bold', fontSize: 13, color: '#555' },
    btn: { background: '#4a6cf7', color: '#fff', border: 'none', padding: '10px 20px', borderRadius: 6, cursor: 'pointer', fontSize: 14 },
    error: { color: '#f44336', fontSize: 13, marginTop: 12 },
    success: { color: '#4caf50', fontSize: 13, marginTop: 12 },
};

export default function CreateTaskModal({ onClose, onSuccess }) {
    const [form, setForm] = useState({
        name: '',
        target_ip: '127.0.0.1',
        target_pid: '',
        duration: 10,
        frequency: 99,
        task_type: 0,
        profiler_type: 0,
        callgraph: 'fp',
        event: '',
    });
    const [submitting, setSubmitting] = useState(false);
    const [error, setError] = useState('');
    const [success, setSuccess] = useState('');

    const updateField = (field, value) => {
        setForm(prev => ({ ...prev, [field]: value }));
    };

    const handleSubmit = async () => {
        // 基本校验
        if (!form.name.trim()) {
            setError('请输入任务名称');
            return;
        }
        if (!form.target_ip.trim()) {
            setError('请输入目标 IP');
            return;
        }

        setSubmitting(true);
        setError('');
        setSuccess('');

        try {
            const payload = {
                name: form.name.trim(),
                target_ip: form.target_ip.trim(),
                target_pid: parseInt(form.target_pid) || 0,
                duration: parseInt(form.duration) || 10,
                frequency: parseInt(form.frequency) || 99,
                task_type: form.task_type,
                profiler_type: form.profiler_type,
                callgraph: form.callgraph,
                event: form.event,
            };

            const res = await tasks.create(payload);

            if (res.code === 0) {
                setSuccess(`任务创建成功！ID: ${res.data?.tid || ''}`);
                // 1.5 秒后关闭弹窗并通知父组件
                setTimeout(() => {
                    if (onSuccess) onSuccess();
                }, 1500);
            } else {
                setError(res.message || '创建失败');
            }
        } catch (err) {
            setError('请求失败: ' + (err.message || '无法连接后端'));
        } finally {
            setSubmitting(false);
        }
    };

    return (
        <div style={styles.card}>
            <h3>新建采样任务</h3>
            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16 }}>
                <div>
                    <label style={styles.label}>任务名称 *</label>
                    <input style={styles.input} placeholder="例如: CPU采样-nginx"
                        value={form.name} onChange={e => updateField('name', e.target.value)} />
                </div>
                <div>
                    <label style={styles.label}>目标 IP *</label>
                    <input style={styles.input} value={form.target_ip}
                        onChange={e => updateField('target_ip', e.target.value)} />
                </div>
                <div>
                    <label style={styles.label}>目标 PID</label>
                    <input style={styles.input} type="number" placeholder="进程 PID"
                        value={form.target_pid} onChange={e => updateField('target_pid', e.target.value)} />
                </div>
                <div>
                    <label style={styles.label}>采样时长（秒）</label>
                    <input style={styles.input} type="number" value={form.duration}
                        onChange={e => updateField('duration', e.target.value)} />
                </div>
                <div>
                    <label style={styles.label}>采样频率（Hz）</label>
                    <input style={styles.input} type="number" value={form.frequency}
                        onChange={e => updateField('frequency', e.target.value)} />
                </div>
                <div>
                    <label style={styles.label}>采集器类型</label>
                    <select style={styles.input} value={form.profiler_type}
                        onChange={e => updateField('profiler_type', parseInt(e.target.value))}>
                        <option value={0}>perf (CPU)</option>
                        <option value={1}>async-profiler (Java)</option>
                        <option value={2}>pprof (Go)</option>
                    </select>
                </div>
            </div>

            {error && <p style={styles.error}>❌ {error}</p>}
            {success && <p style={styles.success}>✅ {success}</p>}

            <div style={{ marginTop: 16, display: 'flex', gap: 10 }}>
                <button style={{ ...styles.btn, opacity: submitting ? 0.6 : 1 }}
                    onClick={handleSubmit} disabled={submitting}>
                    {submitting ? '提交中...' : '提交任务'}
                </button>
                <button style={{ ...styles.btn, background: '#999' }}
                    onClick={onClose} disabled={submitting}>取消</button>
            </div>
        </div>
    );
}
