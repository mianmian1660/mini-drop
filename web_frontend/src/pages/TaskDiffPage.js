// ============================================================
// pages/TaskDiffPage.js — 两次采集的热点函数对比
// ============================================================
// 从 Timeline 页选定「基线」和「对比」两个窗口后跳到这里。
// 配色沿用 git diff 语义：红=对比侧更热，绿=对比侧更冷。
//
// 数据来自两侧的 top.json，而 top.json 只保留各自 Top20，
// 所以「仅基线」「仅对比」的准确含义是"没进入对方的 Top20"，
// 不代表该函数在对方那次采集里不存在——文案上不能写成"新增/消失"。
// ============================================================

import React, { useState, useEffect } from 'react';
import { useSearchParams, Link } from 'react-router-dom';
import { tasks } from '../api';

const S = {
    container: { width: '100%', maxWidth: 1200, minWidth: 0, margin: '0 auto', padding: 20, fontFamily: 'Arial, sans-serif' },
    card: { minWidth: 0, maxWidth: '100%', background: '#fff', borderRadius: 8, padding: 24, marginBottom: 16, boxShadow: '0 2px 8px rgba(0,0,0,0.1)' },
    loading: { textAlign: 'center', padding: 60, color: '#999' },
    hint: { fontSize: 12, color: '#888', marginTop: 4 },
    input: { padding: '7px 10px', border: '1px solid #ddd', borderRadius: 6, fontSize: 13, boxSizing: 'border-box' },
    btnSm: { background: '#e0e0e0', color: '#333', border: 'none', padding: '4px 10px', borderRadius: 4, cursor: 'pointer', fontSize: 12, marginRight: 6 },
    th: { textAlign: 'left', padding: '10px 12px', fontSize: 12, color: '#666', borderBottom: '2px solid #eee', whiteSpace: 'nowrap' },
    td: { padding: '10px 12px', fontSize: 13, borderBottom: '1px solid #f2f2f2' },
    sideBox: (accent) => ({
        flex: 1, minWidth: 240, padding: '12px 16px', borderRadius: 6,
        background: '#fafafa', borderLeft: `4px solid ${accent}`,
    }),
};

// 红=更热 绿=更冷，和 git diff 同构
const UP = '#d64545';
const DOWN = '#2f9e44';
const dirColor = (d) => (d === 'up' || d === 'compare_only' ? UP : DOWN);
const DIR_TEXT = {
    up: '更热',
    down: '更冷',
    compare_only: '仅对比',
    baseline_only: '仅基线',
};

const pct = (v) => `${Number(v || 0).toFixed(2)}%`;
const signed = (v) => `${v > 0 ? '+' : ''}${Number(v || 0).toFixed(2)}`;

export default function TaskDiffPage() {
    const [sp, setSp] = useSearchParams();
    const baselineTid = sp.get('baseline') || '';
    const compareTid = sp.get('compare') || '';

    // 已生效的阈值以 URL 为准，输入框只是待提交的草稿——
    // 否则每敲一个字符都会触发一次重新对比
    const appliedThreshold = sp.get('threshold') || '1';

    const [thresholdDraft, setThresholdDraft] = useState(appliedThreshold);
    const [data, setData] = useState(null);
    const [loading, setLoading] = useState(false);
    const [error, setError] = useState('');

    useEffect(() => {
        if (!baselineTid || !compareTid) return undefined;
        let cancelled = false;
        setLoading(true); setError(''); setData(null);
        tasks.diff(baselineTid, compareTid, appliedThreshold)
            .then((r) => {
                if (cancelled) return;
                if (r.code === 0) setData(r.data);
                else setError(r.message || '对比失败');
            })
            .catch((e) => {
                if (cancelled) return;
                // 后端对"分析未完成""eBPF 直方图不可比"这类情况回 409 并带原因，原样透出
                setError(e.response?.data?.message || ('请求失败: ' + (e.message || '')));
            })
            .finally(() => { if (!cancelled) setLoading(false); });
        return () => { cancelled = true; };
    }, [baselineTid, compareTid, appliedThreshold]);

    const applyThreshold = () => {
        const next = new URLSearchParams(sp);
        next.set('threshold', thresholdDraft);
        setSp(next, { replace: true });
    };

    if (!baselineTid || !compareTid) {
        return (
            <div style={S.container}>
                <h2>热点对比</h2>
                <div style={S.card}>
                    <p>缺少对比参数。</p>
                    <p style={S.hint}>请到 <Link to="/timeline">时间轴</Link> 页面，
                        先给一个窗口点「设为基线」，再对另一个窗口点「与基线对比」。</p>
                </div>
            </div>
        );
    }

    const rows = data?.functions || [];

    return (
        <div style={S.container}>
            <h2>热点对比</h2>

            <div style={S.card}>
                <div style={{ display: 'flex', gap: 16, flexWrap: 'wrap', marginBottom: 16 }}>
                    <div style={S.sideBox('#888')}>
                        <div style={{ fontSize: 12, color: '#888' }}>基线</div>
                        <div style={{ fontWeight: 'bold', wordBreak: 'break-all' }}>
                            {data?.baseline?.name || baselineTid}
                        </div>
                        <Link to={`/task/result?tid=${baselineTid}`} style={{ fontSize: 12, color: '#4a6cf7' }}>
                            查看该任务 →
                        </Link>
                    </div>
                    <div style={S.sideBox('#4a6cf7')}>
                        <div style={{ fontSize: 12, color: '#888' }}>对比</div>
                        <div style={{ fontWeight: 'bold', wordBreak: 'break-all' }}>
                            {data?.compare?.name || compareTid}
                        </div>
                        <Link to={`/task/result?tid=${compareTid}`} style={{ fontSize: 12, color: '#4a6cf7' }}>
                            查看该任务 →
                        </Link>
                    </div>
                </div>

                <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                    <span style={{ fontSize: 13, color: '#555' }}>噪声阈值（百分点）</span>
                    <input
                        style={{ ...S.input, width: 90 }}
                        type="number" min="0" step="0.5"
                        value={thresholdDraft}
                        onChange={(e) => setThresholdDraft(e.target.value)}
                    />
                    <button style={S.btnSm} onClick={applyThreshold}>重新对比</button>
                    <Link to="/timeline" style={{ fontSize: 13, color: '#4a6cf7' }}>← 回时间轴</Link>
                </div>
                <div style={S.hint}>
                    差值绝对值小于该阈值的函数会被滤掉。单机采样噪声较大，建议 1~2。
                </div>
            </div>

            {loading && <div style={S.loading}>⏳ 正在对比...</div>}
            {error && <div style={{ ...S.loading, color: '#f44' }}>❌ {error}</div>}

            {!loading && !error && data && (
                <div style={S.card}>
                    <h3 style={{ marginTop: 0 }}>
                        差异函数（{rows.length} 个，按变化幅度排序）
                    </h3>

                    {rows.length === 0 ? (
                        <div style={{ ...S.loading, color: '#888' }}>
                            两次采集的热点分布没有超过阈值的差异
                        </div>
                    ) : (
                        <div className="table-scroll">
                            <table style={{ width: '100%', borderCollapse: 'collapse' }}>
                                <thead>
                                    <tr>
                                        <th style={S.th}>函数</th>
                                        <th style={{ ...S.th, textAlign: 'right' }}>基线</th>
                                        <th style={{ ...S.th, textAlign: 'right' }}>对比</th>
                                        <th style={{ ...S.th, textAlign: 'right' }}>差值</th>
                                        <th style={S.th}>方向</th>
                                    </tr>
                                </thead>
                                <tbody>
                                    {rows.map((r) => {
                                        const color = dirColor(r.direction);
                                        return (
                                            <tr key={r.function}>
                                                <td style={{ ...S.td, wordBreak: 'break-all', fontFamily: 'monospace' }}>
                                                    {r.function}
                                                </td>
                                                <td style={{ ...S.td, textAlign: 'right', color: '#888' }}>
                                                    {r.direction === 'compare_only' ? '—' : pct(r.baseline_percentage)}
                                                </td>
                                                <td style={{ ...S.td, textAlign: 'right', color: '#888' }}>
                                                    {r.direction === 'baseline_only' ? '—' : pct(r.compare_percentage)}
                                                </td>
                                                <td style={{ ...S.td, textAlign: 'right', color, fontWeight: 'bold' }}>
                                                    {signed(r.delta_percentage)}
                                                </td>
                                                <td style={S.td}>
                                                    <span style={{
                                                        padding: '2px 8px', borderRadius: 10, fontSize: 11,
                                                        fontWeight: 'bold', background: color, color: '#fff',
                                                    }}>
                                                        {DIR_TEXT[r.direction] || r.direction}
                                                    </span>
                                                </td>
                                            </tr>
                                        );
                                    })}
                                </tbody>
                            </table>
                        </div>
                    )}

                    <div style={S.hint}>
                        「仅对比」「仅基线」表示该函数没有进入另一侧的 Top20，
                        不代表它在那次采集里完全不存在。
                    </div>
                </div>
            )}
        </div>
    );
}
