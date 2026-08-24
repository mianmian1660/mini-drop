import React, { useCallback, useEffect, useState } from 'react';
import { continuous } from '../api';
import { decodeJSONField } from '../utils/continuous';

const S = {
    container: { width: '100%', maxWidth: 900, minWidth: 0, margin: '0 auto', padding: '22px 28px 36px', fontFamily: 'Arial, sans-serif', color: '#101828' },
    title: { margin: '0 0 4px', fontSize: 24 },
    subtitle: { margin: '0 0 20px', color: '#667085', fontSize: 13, lineHeight: 1.6 },
    card: { minWidth: 0, maxWidth: '100%', background: '#fff', borderRadius: 8, padding: 20, border: '1px solid #e5e7eb', boxShadow: '0 1px 2px rgba(16,24,40,.04)' },
    label: { display: 'block', color: '#344054', fontSize: 13, fontWeight: 700, marginBottom: 6 },
    select: { width: '100%', height: 38, padding: '8px 10px', boxSizing: 'border-box', border: '1px solid #d0d5dd', borderRadius: 6, fontSize: 13, marginBottom: 18 },
    warn: { display: 'flex', gap: 8, marginBottom: 18, color: '#b54708', background: '#fffaeb', border: '1px solid #fedf89', borderRadius: 6, padding: 11, fontSize: 13, lineHeight: 1.5 },
    error: { marginBottom: 14, color: '#b42318', background: '#fff6f5', border: '1px solid #fda29b', borderRadius: 6, padding: 10, fontSize: 13 },
    row: { display: 'grid', gridTemplateColumns: '70px 1fr 1fr 70px 1fr 1.6fr 32px', gap: 8, alignItems: 'center', marginBottom: 8 },
    rowHead: { display: 'grid', gridTemplateColumns: '70px 1fr 1fr 70px 1fr 1.6fr 32px', gap: 8, marginBottom: 6, color: '#667085', fontSize: 11 },
    input: { width: '100%', height: 34, padding: '6px 8px', boxSizing: 'border-box', border: '1px solid #d0d5dd', borderRadius: 6, fontSize: 12 },
    inputDisabled: { width: '100%', height: 34, padding: '6px 8px', boxSizing: 'border-box', border: '1px solid #eaecf0', borderRadius: 6, fontSize: 12, background: '#f8fafc', color: '#98a2b3' },
    mono: { fontFamily: 'ui-monospace,SFMono-Regular,Menlo,monospace' },
    del: { width: 34, height: 34, border: '1px solid #eaecf0', background: '#fff', borderRadius: 6, cursor: 'pointer', color: '#b42318', fontSize: 15 },
    add: { marginTop: 4, background: '#fff', color: '#315efb', border: '1px solid #c7d2fe', borderRadius: 6, padding: '8px 14px', fontSize: 13, fontWeight: 700, cursor: 'pointer' },
    empty: { color: '#98a2b3', fontSize: 13, padding: '10px 0' },
    actions: { display: 'flex', justifyContent: 'flex-end', alignItems: 'center', gap: 10, marginTop: 20, borderTop: '1px solid #eef2f6', paddingTop: 16 },
    save: disabled => ({ background: disabled ? '#e5e7eb' : '#315efb', color: disabled ? '#98a2b3' : '#fff', border: 0, borderRadius: 6, padding: '9px 16px', fontWeight: 700, cursor: disabled ? 'not-allowed' : 'pointer' }),
    savedMsg: { color: '#067647', fontSize: 13 },
    loading: { textAlign: 'center', padding: 40, color: '#667085' },
};

function emptyTarget() {
    return { engine: 'mysql', instance_label: '', host: '', port: '3306', user: '', password_ref: '' };
}

export default function DBAccountsPage() {
    const [sessions, setSessions] = useState([]);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState('');
    const [selectedSID, setSelectedSID] = useState('');
    const [labels, setLabels] = useState({});
    const [targets, setTargets] = useState([]);
    const [saving, setSaving] = useState(false);
    const [savedMsg, setSavedMsg] = useState('');

    useEffect(() => {
        let cancelled = false;
        (async () => {
            setLoading(true);
            setError('');
            try {
                const response = await continuous.sessions({ page: 1, page_size: 200, owner_filter: 'all' });
                if (cancelled) return;
                if (response.code !== 0) throw new Error(response.message || '加载持续采集会话失败');
                const list = response.data?.sessions || [];
                setSessions(list);
                if (list.length > 0) setSelectedSID(list[0].sid);
            } catch (err) {
                if (!cancelled) setError(err?.message || '加载持续采集会话失败');
            } finally {
                if (!cancelled) setLoading(false);
            }
        })();
        return () => { cancelled = true; };
    }, []);

    useEffect(() => {
        const session = sessions.find(item => item.sid === selectedSID);
        if (!session) {
            setLabels({});
            setTargets([]);
            return;
        }
        const decoded = decodeJSONField(session.labels, {});
        setLabels(decoded);
        setTargets(Array.isArray(decoded.db_targets) ? decoded.db_targets.map(item => ({
            engine: 'mysql',
            instance_label: item.instance_label || '',
            host: item.host || '',
            port: String(item.port || ''),
            user: item.user || '',
            password_ref: item.password_ref || '',
        })) : []);
        setSavedMsg('');
    }, [selectedSID, sessions]);

    const updateTarget = useCallback((index, field, value) => {
        setTargets(current => current.map((item, i) => (i === index ? { ...item, [field]: value } : item)));
    }, []);
    const removeTarget = useCallback(index => {
        setTargets(current => current.filter((_, i) => i !== index));
    }, []);
    const addTarget = useCallback(() => {
        setTargets(current => [...current, emptyTarget()]);
    }, []);

    const selectedSession = sessions.find(item => item.sid === selectedSID);

    const save = async () => {
        if (!selectedSID || saving) return;
        setSaving(true);
        setError('');
        setSavedMsg('');
        try {
            const nextLabels = {
                ...labels,
                db_targets: targets.map(item => ({
                    engine: 'mysql',
                    instance_label: item.instance_label.trim(),
                    host: item.host.trim(),
                    port: Number(item.port) || 0,
                    user: item.user.trim(),
                    password_ref: item.password_ref.trim(),
                })),
            };
            const response = await continuous.updateLabels(selectedSID, nextLabels);
            if (response.code !== 0) throw new Error(response.message || '保存失败');
            setSessions(current => current.map(item => (item.sid === selectedSID ? { ...item, labels: response.data?.session?.labels ?? item.labels } : item)));
            setSavedMsg('已保存');
        } catch (err) {
            setError(err?.response?.data?.message || err?.message || '保存失败');
        } finally {
            setSaving(false);
        }
    };

    if (loading) return <div style={S.container}><div style={S.loading}>加载中...</div></div>;

    return (
        <div style={S.container}>
            <h1 style={S.title}>数据库账号</h1>
            <p style={S.subtitle}>
                选择一个持续采集会话,配置它的数据库巡检目标(db_targets)。
                密码不经过这个页面——password_ref 只填密码文件在 agent 主机上的路径,文件需要运维提前手动放好,和 AgentDiscoveryConfig 现有的"不存密码"约定一致。
            </p>
            {error && <div style={S.error}>{error}</div>}
            <div style={S.card}>
                <label style={S.label}>选择会话</label>
                {sessions.length === 0 ? (
                    <div style={S.empty}>还没有任何持续采集会话,先去主机页面创建一个。</div>
                ) : (
                    <select style={S.select} value={selectedSID} onChange={e => setSelectedSID(e.target.value)}>
                        {sessions.map(session => (
                            <option key={session.sid} value={session.sid}>
                                {session.name} · {session.target_ip}{session.hostname ? ` (${session.hostname})` : ''} · {session.observed_state === 'running' ? '运行中' : session.observed_state === 'stopped' ? '已停止' : session.observed_state}
                                {Array.isArray(decodeJSONField(session.labels, {}).db_targets) && decodeJSONField(session.labels, {}).db_targets.length > 0 ? `(已配置 ${decodeJSONField(session.labels, {}).db_targets.length} 个目标)` : '(未配置数据库巡检)'}
                            </option>
                        ))}
                    </select>
                )}

                {selectedSession && (
                    <>
                        {selectedSession.observed_state === 'running' && (
                            <div style={S.warn}>
                                <span>⚠</span>
                                <span>改动仅对新建/重建的巡检生效。这个会话当前是"运行中",保存后需要停止并重新创建才会真正开始采集新配置。</span>
                            </div>
                        )}

                        {targets.length === 0 ? (
                            <div style={S.empty}>还没有配置数据库目标,点下面按钮添加一个。</div>
                        ) : (
                            <>
                                <div style={S.rowHead}>
                                    <span>引擎</span><span>实例标签</span><span>主机</span><span>端口</span><span>用户名</span><span>密码文件路径</span><span />
                                </div>
                                {targets.map((item, index) => (
                                    <div style={S.row} key={index}>
                                        <input style={S.inputDisabled} value="mysql" disabled />
                                        <input style={S.input} placeholder="实例标签" value={item.instance_label} onChange={e => updateTarget(index, 'instance_label', e.target.value)} />
                                        <input style={S.input} placeholder="主机" value={item.host} onChange={e => updateTarget(index, 'host', e.target.value)} />
                                        <input style={S.input} placeholder="端口" value={item.port} onChange={e => updateTarget(index, 'port', e.target.value)} />
                                        <input style={S.input} placeholder="用户名" value={item.user} onChange={e => updateTarget(index, 'user', e.target.value)} />
                                        <input style={{ ...S.input, ...S.mono }} placeholder="/etc/mini-drop/db-credentials.d/xxx.env" value={item.password_ref} onChange={e => updateTarget(index, 'password_ref', e.target.value)} />
                                        <button type="button" style={S.del} onClick={() => removeTarget(index)} aria-label="删除">×</button>
                                    </div>
                                ))}
                            </>
                        )}
                        <button type="button" style={S.add} onClick={addTarget}>+ 添加数据库目标</button>

                        <div style={S.actions}>
                            {savedMsg && <span style={S.savedMsg}>{savedMsg}</span>}
                            <button type="button" style={S.save(saving)} disabled={saving} onClick={save}>{saving ? '保存中...' : '保存'}</button>
                        </div>
                    </>
                )}
            </div>
        </div>
    );
}
