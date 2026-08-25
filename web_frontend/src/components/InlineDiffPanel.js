// ============================================================
// components/InlineDiffPanel.js — 周期性深度采样内嵌基线对比面板
// ============================================================
// 面板结构（标题栏 + 折叠展开）参考 ContinuousProfilingPanel 的 Diff 区块。
// 表格数据来自 tasks.diff（两侧 top.json 对比）；火焰图数据来自
// tasks.diffFlamegraph（GetTaskDiff 的 format=flamegraph 分支，两侧
// folded.txt 建树+按各自总样本数归一化后差分），两条互相独立的查询，
// 切 tab 不会互相清空——和 ContinuousProfilingPanel 的 diff 区块同一个做法。
// ============================================================

import React, { useEffect, useMemo, useState } from 'react';
import { Link } from 'react-router-dom';
import { tasks } from '../api';
import InteractiveFlamegraph from './InteractiveFlamegraph';

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
    tabs: { display: 'inline-flex', border: '1px solid #d0d5dd', borderRadius: 6, overflow: 'hidden', margin: '12px 0' },
    tab: (active) => ({ padding: '7px 16px', border: 0, borderRight: '1px solid #d0d5dd', background: active ? '#eef2ff' : '#fff', color: active ? '#315efb' : '#475467', fontWeight: 700, fontSize: 13, cursor: 'pointer' }),
    flameBox: { width: '100%', minWidth: 0, maxWidth: '100%', height: 420, overflowX: 'auto', overflowY: 'auto', border: '1px solid #eaecf0', borderRadius: 8, background: '#fff', padding: 6 },
};

// UP/DOWN 和火焰图 tab 的 Brendan Gregg 红/蓝惯例（diffFlamegraphColor，见
// InteractiveFlamegraph.js）统一：红=变热，蓝=变冷，同一个面板切 tab 颜色语义一致。
const UP = '#d64545';
const DOWN = '#175cd3';
const dirColor = (d) => (d === 'up' || d === 'compare_only' ? UP : DOWN);
const DIR_TEXT = { up: '更热', down: '更冷', compare_only: '仅对比', baseline_only: '仅基线' };
const pct = (v) => `${Number(v || 0).toFixed(2)}%`;
const signed = (v) => `${v > 0 ? '+' : ''}${Number(v || 0).toFixed(2)}`;

export default function InlineDiffPanel({ baselineTid, compareTid, onClose }) {
    const [viewMode, setViewMode] = useState('table');
    const [thresholdDraft, setThresholdDraft] = useState('1');
    const [threshold, setThreshold] = useState('1');
    const [data, setData] = useState(null);
    const [loading, setLoading] = useState(false);
    const [error, setError] = useState('');

    const [flameResult, setFlameResult] = useState(null);
    const [flameLoading, setFlameLoading] = useState(false);
    const [flameError, setFlameError] = useState('');

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

    // 火焰图 tab 惰性加载：第一次切进去才发请求，避免每次展开面板都多打一次
    // 后端（folded.txt 读取+建树比 top.json 表格对比重，没必要总跑）。
    useEffect(() => {
        if (viewMode !== 'flamegraph' || !baselineTid || !compareTid || flameResult || flameLoading) return undefined;
        let cancelled = false;
        setFlameLoading(true); setFlameError('');
        tasks.diffFlamegraph(baselineTid, compareTid)
            .then((r) => {
                if (cancelled) return;
                if (r.code === 0) setFlameResult(r.data);
                else setFlameError(r.message || '差分火焰图生成失败');
            })
            .catch((e) => {
                if (cancelled) return;
                setFlameError(e.response?.data?.message || ('请求失败: ' + (e.message || '')));
            })
            .finally(() => { if (!cancelled) setFlameLoading(false); });
        return () => { cancelled = true; };
    }, [viewMode, baselineTid, compareTid, flameResult, flameLoading]);

    // 重新对比阈值只影响表格；两个 tid 变化时火焰图缓存也要失效重新拉。
    useEffect(() => { setFlameResult(null); setFlameError(''); }, [baselineTid, compareTid]);

    // 把 ProfileDiffFlamegraph.root.children 铺成 InteractiveFlamegraph 期待的
    // nodes 数组，width 取 max(base_value,compare_value) 保证消失的函数仍可见
    // ——和 ContinuousProfilingPanel.js 的 diffFlamegraphNodes 是同一份逻辑。
    const flameNodes = useMemo(() => {
        if (!flameResult || flameResult.empty || !flameResult.root) return null;
        const convert = (node) => ({
            name: node.name,
            value: Math.max(Number(node.base_value) || 0, Number(node.compare_value) || 0),
            base_value: node.base_value,
            compare_value: node.compare_value,
            delta: node.delta,
            delta_percent: node.delta_percent,
            children: Array.isArray(node.children) ? node.children.map(convert) : [],
        });
        return (flameResult.root.children || []).map(convert);
    }, [flameResult]);

    const flameData = useMemo(() => {
        if (!flameResult) return null;
        return {
            nodes: flameNodes || [],
            unit: flameResult.unit,
            empty: flameResult.empty || !flameNodes || flameNodes.length === 0,
            message: flameResult.message,
        };
    }, [flameResult, flameNodes]);

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

            <div style={S.tabs}>
                <button type="button" style={S.tab(viewMode === 'table')} onClick={() => setViewMode('table')}>表格</button>
                <button type="button" style={{ ...S.tab(viewMode === 'flamegraph'), borderRight: 0 }} onClick={() => setViewMode('flamegraph')}>火焰图</button>
            </div>

            {viewMode === 'table' && loading && <div style={S.loading}>正在对比...</div>}
            {viewMode === 'table' && error && <div style={{ ...S.loading, color: '#f44' }}>{error}</div>}

            {viewMode === 'flamegraph' && (
                <>
                    <div style={S.hint}>宽度取两侧较大值，保证消失的函数仍可见；红=变热，蓝=变冷，深浅代表变化幅度；悬停查看具体数值。</div>
                    {flameError && <div style={{ ...S.loading, color: '#f44' }}>{flameError}</div>}
                    {!flameError && (
                        <div style={{ marginTop: 12 }}>
                            <InteractiveFlamegraph
                                data={flameData}
                                loading={flameLoading}
                                loadingMessage="正在生成差分火焰图..."
                                emptyMessage="两次采集没有可对比的调用栈"
                                boxStyle={S.flameBox}
                                diffMode
                            />
                        </div>
                    )}
                </>
            )}

            {viewMode === 'table' && !loading && !error && data && (
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
