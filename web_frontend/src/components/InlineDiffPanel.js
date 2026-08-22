// ============================================================
// components/InlineDiffPanel.js — 周期性深度采样内嵌基线对比面板
// ============================================================
// 面板结构（标题栏 + 折叠展开）参考 ContinuousProfilingPanel 的 Diff 区块。
// 数据来自 tasks.diff（两侧 top.json 对比），后端只支持表格结果，
// 不像 Native Continuous Profiling 那样有 format=flamegraph，
// 所以这里没有"表格/火焰图"切换——只做表格，避免承诺一个后端不支持的视图。
// ============================================================

import React, { useEffect, useState } from 'react';
import { Link } from 'react-router-dom';
import { tasks } from '../api';

const S = {
    card: { minWidth: 0, maxWidth: '100%', background: '#fff', borderRadius: 8, padding: 16, border: '1px solid #e5e7eb', boxShadow: '0 1px 2px rgba(16,24,40,0.04)' },
    head: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 12, flexWrap: 'wrap', marginBottom: 12 },
    title: { margin: 0, fontSize: 16, color: '#101828' },
    btn: { background: '#315efb', color: '#fff', border: 'none', padding: '8px 12px', borderRadius: 6, cursor: 'pointer', fontSize: 13, fontWeight: 700 },
    btnSm: { background: '#e0e0e0', color: '#333', border: 'none', padding: '4px 10px', borderRadius: 4, cursor: 'pointer', fontSize: 12 },
    input: { padding: '7px 10px', border: '1px solid #ddd', borderRadius: 6, fontSize: 13, boxSizing: 'border-box' },
    th: { textAlign: 'left', padding: '10px 12px', fontSize: 12, color: '#666', borderBottom: '2px solid #eee', whiteSpace: 'nowrap' },
    td: { padding: '10px 12px', fontSize: 13, borderBottom: '1px solid #f2f2f2' },
    hint: { fontSize: 12, color: '#888', marginTop: 4 },
    loading: { textAlign: 'center', padding: 40, color: '#999' },
};

const UP = '#d64545';
const DOWN = '#2f9e44';
const dirColor = (d) => (d === 'up' || d === 'compare_only' ? UP : DOWN);
const DIR_TEXT = { up: '更热', down: '更冷', compare_only: '仅对比', baseline_only: '仅基线' };
const pct = (v) => `${Number(v || 0).toFixed(2)}%`;
const signed = (v) => `${v > 0 ? '+' : ''}${Number(v || 0).toFixed(2)}`;

export default function InlineDiffPanel({ baselineTid, compareTid, onClose }) {
    const [thresholdDraft, setThresholdDraft] = useState('1');
    const [threshold, setThreshold] = useState('1');
    const [data, setData] = useState(null);
    const [loading, setLoading] = useState(false);
    const [error, setError] = useState('');

    useEffect(() => {
        if (!baselineTid || !compareTid) return undefined;
        let cancelled = false;
        setLoading(true); setError(''); setData(null);
        tasks.diff(baselineTid, compareTid, threshold)
            .then((r) => {
                if (cancelled) return;
                if (r.code === 0) setData(r.data);
                else setError(r.message || '对比失败');
            })
            .catch((e) => {
                if (cancelled) return;
                setError(e.response?.data?.message || ('请求失败: ' + (e.message || '')));
            })
            .finally(() => { if (!cancelled) setLoading(false); });
        return () => { cancelled = true; };
    }, [baselineTid, compareTid, threshold]);

    const rows = data?.functions || [];

    return (
        <section style={S.card}>
            <div style={S.head}>
                <h3 style={S.title}>时间窗 Diff（Baseline: {data?.baseline?.name || baselineTid} vs Compare: {data?.compare?.name || compareTid}）</h3>
                <button type="button" style={S.btn} onClick={onClose}>收起</button>
            </div>

            <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                <span style={{ fontSize: 13, color: '#555' }}>噪声阈值（百分点）</span>
                <input
                    style={{ ...S.input, width: 90 }}
                    type="number" min="0" step="0.5"
                    value={thresholdDraft}
                    onChange={(e) => setThresholdDraft(e.target.value)}
                />
                <button type="button" style={S.btnSm} onClick={() => setThreshold(thresholdDraft)}>重新对比</button>
                <Link to={`/task/diff?baseline=${baselineTid}&compare=${compareTid}`} style={{ fontSize: 13, color: '#4a6cf7' }}>
                    在独立页面查看 →
                </Link>
            </div>
            <div style={S.hint}>差值绝对值小于该阈值的函数会被滤掉。单机采样噪声较大，建议 1~2。</div>

            {loading && <div style={S.loading}>正在对比...</div>}
            {error && <div style={{ ...S.loading, color: '#f44' }}>{error}</div>}

            {!loading && !error && data && (
                rows.length === 0 ? (
                    <div style={{ ...S.loading, color: '#888' }}>两次采集的热点分布没有超过阈值的差异</div>
                ) : (
                    <div className="table-scroll" style={{ marginTop: 12 }}>
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
                                            <td style={{ ...S.td, wordBreak: 'break-all', fontFamily: 'monospace' }}>{r.function}</td>
                                            <td style={{ ...S.td, textAlign: 'right', color: '#888' }}>
                                                {r.direction === 'compare_only' ? '—' : pct(r.baseline_percentage)}
                                            </td>
                                            <td style={{ ...S.td, textAlign: 'right', color: '#888' }}>
                                                {r.direction === 'baseline_only' ? '—' : pct(r.compare_percentage)}
                                            </td>
                                            <td style={{ ...S.td, textAlign: 'right', color, fontWeight: 'bold' }}>{signed(r.delta_percentage)}</td>
                                            <td style={S.td}>
                                                <span style={{ padding: '2px 8px', borderRadius: 10, fontSize: 11, fontWeight: 'bold', background: color, color: '#fff' }}>
                                                    {DIR_TEXT[r.direction] || r.direction}
                                                </span>
                                            </td>
                                        </tr>
                                    );
                                })}
                            </tbody>
                        </table>
                        <div style={S.hint}>「仅对比」「仅基线」表示该函数没有进入另一侧的 Top20，不代表它在那次采集里完全不存在。</div>
                    </div>
                )
            )}
        </section>
    );
}
