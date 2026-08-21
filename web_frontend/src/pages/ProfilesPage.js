import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import { profiles } from '../api';
import ContinuousProfilingPanel from '../components/ContinuousProfilingPanel';

const S = {
    container: { width: '100%', maxWidth: 1280, minWidth: 0, margin: '0 auto', padding: 24, fontFamily: 'Arial, sans-serif', color: '#202124' },
    pageHead: { display: 'flex', justifyContent: 'space-between', gap: 16, alignItems: 'flex-end', marginBottom: 18 },
    eyebrow: { margin: '0 0 6px 0', color: '#667085', fontSize: 13 },
    title: { margin: 0, fontSize: 28, lineHeight: 1.2 },
    band: { minWidth: 0, maxWidth: '100%', background: '#fff', border: '1px solid #e5e7eb', borderRadius: 8, padding: 18, marginBottom: 16, boxShadow: '0 1px 3px rgba(16,24,40,0.08)' },
    subtle: { color: '#667085', fontSize: 12 },
    error: { background: '#fff3f3', border: '1px solid #ffcdd2', color: '#b42318', borderRadius: 8, padding: 12, marginBottom: 16 },
    empty: { textAlign: 'center', padding: 46, color: '#667085', background: '#fbfcfe', border: '1px dashed #d0d7de', borderRadius: 8 },
    btnSecondary: { background: '#fff', color: '#315efb', border: '1px solid #c7d2fe', padding: '9px 12px', borderRadius: 6, cursor: 'pointer', fontSize: 13, fontWeight: 700, textDecoration: 'none' },
};

export default function ProfilesPage() {
    const [searchParams, setSearchParams] = useSearchParams();
    const [targets, setTargets] = useState([]);
    const [targetId, setTargetId] = useState(searchParams.get('target_id') || '');
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState('');

    const selectedTarget = useMemo(() => targets.find(t => t.id === targetId) || null, [targets, targetId]);
    const hasExplicitTarget = Boolean(searchParams.get('target_id'));

    const loadTargets = useCallback(async () => {
        setLoading(true);
        setError('');
        try {
            const res = await profiles.targets();
            if (res.code !== 0) {
                setError(res.message || '加载可观测对象失败');
                return;
            }
            const list = res.data?.targets || [];
            const requested = searchParams.get('target_id');
            setTargets(list);
            if (requested && list.some(t => t.id === requested)) {
                setTargetId(requested);
            } else if (hasExplicitTarget && list.length > 0) {
                setTargetId(list[0].id);
            }
        } catch (err) {
            setError(err?.message || '加载可观测对象失败');
        } finally {
            setLoading(false);
        }
    }, [searchParams, hasExplicitTarget]);

    useEffect(() => {
        loadTargets();
    }, [loadTargets]);

    const changeTarget = useCallback((nextTargetId) => {
        setTargetId(nextTargetId);
        setSearchParams({ target_id: nextTargetId });
    }, [setSearchParams]);

    return (
        <div style={S.container}>
            <div style={S.pageHead}>
                <div>
                    <p style={S.eyebrow}>Native Continuous Profiling</p>
                    <h2 style={S.title}>Native profiling</h2>
                </div>
                <Link to="/" style={S.btnSecondary}>返回主页</Link>
            </div>

            {error && <div style={S.error}>{error}</div>}

            {!hasExplicitTarget && (
                <section style={S.band}>
                    <h3 style={{ margin: '0 0 8px', fontSize: 18 }}>请先选择主机</h3>
                    <p style={{ ...S.subtle, margin: '0 0 12px' }}>Native profiling 已迁入主机性能中心。请选择一个主机后再查看火焰图和 TopN。</p>
                    <Link to="/" style={S.btnSecondary}>返回主机列表</Link>
                </section>
            )}

            {hasExplicitTarget && selectedTarget && (
                <section style={S.band}>
                    <p style={{ ...S.subtle, margin: 0 }}>
                        兼容入口：主入口在 <Link to={`/hosts/${encodeURIComponent(selectedTarget.id)}?tab=profiling`} style={{ color: '#315efb', fontWeight: 700 }}>主机性能中心</Link>，这里使用同一套视图。
                    </p>
                </section>
            )}

            {hasExplicitTarget && (
                loading && !selectedTarget
                    ? <div style={S.empty}>正在加载可观测对象...</div>
                    : <ContinuousProfilingPanel
                        target={selectedTarget}
                        targets={targets}
                        targetId={targetId}
                        onTargetChange={changeTarget}
                        showTargetSelect
                    />
            )}
        </div>
    );
}
