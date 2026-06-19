// ============================================================
// components/CreateTaskModal.js — 新建任务弹窗（W3 增强版）
// ============================================================
// 功能：填写采样参数 → POST /api/v1/tasks → 通知父组件刷新
// W3 增强：
//   - 从 apiserver 拉取在线 Agent 列表供选择目标 IP
//   - 表单校验增强（PID 范围、时长范围等）
//   - 提交后显示任务 ID，可点击跳转详情
// ============================================================

import React, { useState, useEffect } from 'react';
import { Link } from 'react-router-dom';
import { tasks, agents } from '../api';

const styles = {
    card: { background: '#f8f9ff', borderRadius: 8, padding: 24, marginBottom: 16, border: '1px solid #e0e4ff' },
    input: { width: '100%', padding: '8px 12px', border: '1px solid #ddd', borderRadius: 4, fontSize: 14, marginBottom: 12, boxSizing: 'border-box' },
    select: { width: '100%', padding: '8px 12px', border: '1px solid #ddd', borderRadius: 4, fontSize: 14, marginBottom: 12, boxSizing: 'border-box', background: '#fff' },
    label: { display: 'block', marginBottom: 4, fontWeight: 'bold', fontSize: 13, color: '#555' },
    btn: { background: '#4a6cf7', color: '#fff', border: 'none', padding: '10px 20px', borderRadius: 6, cursor: 'pointer', fontSize: 14 },
    error: { color: '#f44336', fontSize: 13, marginTop: 12 },
    success: { color: '#4caf50', fontSize: 13, marginTop: 12 },
    agentLoading: { fontSize: 12, color: '#999', marginBottom: 12 },
};

export default function CreateTaskModal({ onClose, onSuccess }) {
    const [form, setForm] = useState({
        name: '',
        target_ip: '',
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
    const [createdTid, setCreatedTid] = useState('');

    // W3: 拉取在线 Agent 列表
    const [agentList, setAgentList] = useState([]);
    const [agentsLoading, setAgentsLoading] = useState(true);

    useEffect(() => {
        agents.list().then(res => {
            if (res.code === 0) {
                const list = res.data?.agents || [];
                setAgentList(list);
                // 默认选中第一个在线 Agent
                const online = list.filter(a => a.online);
                if (online.length > 0 && !form.target_ip) {
                    setForm(prev => ({ ...prev, target_ip: online[0].ip_addr }));
                }
            }
        }).catch(err => {
            console.error('获取 Agent 列表失败:', err);
        }).finally(() => {
            setAgentsLoading(false);
        });
    }, []);

    const updateField = (field, value) => {
        setForm(prev => ({ ...prev, [field]: value }));
    };

    const handleSubmit = async () => {
        // 基本校验
        if (!form.name.trim()) {
            setError('请输入任务名称');
            return;
        }
        if (!form.target_ip) {
            setError('请选择目标 Agent');
            return;
        }
        const pid = parseInt(form.target_pid);
        if (form.target_pid && (isNaN(pid) || pid < 1 || pid > 999999)) {
            setError('PID 需为 1-999999 之间的整数');
            return;
        }
        const dur = parseInt(form.duration);
        if (isNaN(dur) || dur < 1 || dur > 3600) {
            setError('采样时长需为 1-3600 秒');
            return;
        }
        const hz = parseInt(form.frequency);
        if (isNaN(hz) || hz < 1 || hz > 9999) {
            setError('采样频率需为 1-9999 Hz');
            return;
        }

        setSubmitting(true);
        setError('');
        setSuccess('');

        try {
            const payload = {
                name: form.name.trim(),
                target_ip: form.target_ip,
                target_pid: pid || 0,
                duration: dur,
                frequency: hz,
                task_type: form.task_type,
                profiler_type: form.profiler_type,
                callgraph: form.callgraph,
                event: form.event,
            };

            const res = await tasks.create(payload);

            if (res.code === 0) {
                const tid = res.data?.tid || '';
                setCreatedTid(tid);
                setSuccess(`任务创建成功！`);
                // 2 秒后关闭弹窗并通知父组件
                setTimeout(() => {
                    if (onSuccess) onSuccess();
                }, 2000);
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

            {/* W3: Agent 选择器 */}
            <div style={{ marginBottom: 16 }}>
                <label style={styles.label}>目标 Agent *</label>
                {agentsLoading ? (
                    <p style={styles.agentLoading}>加载 Agent 列表...</p>
                ) : agentList.length === 0 ? (
                    <p style={{ ...styles.agentLoading, color: '#f44336' }}>
                        ⚠️ 没有在线 Agent，请先启动 drop_agent
                    </p>
                ) : (
                    <select style={styles.select} value={form.target_ip}
                        onChange={e => updateField('target_ip', e.target.value)}>
                        <option value="">-- 选择 Agent --</option>
                        {agentList.map(a => (
                            <option key={a.ip_addr} value={a.ip_addr}>
                                {a.hostname} ({a.ip_addr}) {a.online ? '🟢' : '🔴'}
                            </option>
                        ))}
                    </select>
                )}
            </div>

            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16 }}>
                <div>
                    <label style={styles.label}>任务名称 *</label>
                    <input style={styles.input} placeholder="例如: CPU采样-nginx"
                        value={form.name} onChange={e => updateField('name', e.target.value)} />
                </div>
                <div>
                    <label style={styles.label}>目标 PID</label>
                    <input style={styles.input} type="number" placeholder="留空 = 采集整机"
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
                    <select style={styles.select} value={form.profiler_type}
                        onChange={e => updateField('profiler_type', parseInt(e.target.value))}>
                        <option value={0}>perf (CPU)</option>
                        <option value={1}>async-profiler (Java)</option>
                        <option value={2}>pprof (Go)</option>
                    </select>
                </div>
                <div>
                    <label style={styles.label}>调用图模式</label>
                    <select style={styles.select} value={form.callgraph}
                        onChange={e => updateField('callgraph', e.target.value)}>
                        <option value="fp">fp (frame pointer)</option>
                        <option value="dwarf">dwarf (DWARF)</option>
                        <option value="lbr">lbr (LBR)</option>
                    </select>
                </div>
            </div>

            {error && <p style={styles.error}>❌ {error}</p>}
            {success && (
                <div style={styles.success}>
                    ✅ {success}
                    {createdTid && (
                        <span style={{ marginLeft: 8 }}>
                            <Link to={`/task/result?tid=${createdTid}`} style={{ color: '#4a6cf7', fontWeight: 'bold' }}>
                                查看任务 → {createdTid}
                            </Link>
                        </span>
                    )}
                </div>
            )}

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
