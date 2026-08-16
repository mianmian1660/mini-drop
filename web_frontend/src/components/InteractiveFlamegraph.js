import React, { useEffect, useMemo, useRef, useState } from 'react';
import { select } from 'd3-selection';
import { flamegraph as createFlamegraph } from 'd3-flame-graph';
import 'd3-flame-graph/dist/d3-flamegraph.css';
import { formatFlamegraphLabel } from '../utils/profileMetrics';

const SAFE_MAX_DEPTH = 80;
const SAFE_MAX_NODES = 3500;
const MAX_CHART_HEIGHT = 2400;

const BASE = {
    flameBox: { height: 560, overflowX: 'auto', overflowY: 'auto', border: '1px solid #eaecf0', borderRadius: 8, background: '#fff', padding: 6 },
    empty: { textAlign: 'center', padding: 44, color: '#667085', background: '#fff', border: '1px dashed #d0d7de', borderRadius: 8 },
    warn: { color: '#b42318', background: '#fff6f5', border: '1px solid #fda29b', borderRadius: 6, padding: '9px 12px', fontSize: 13 },
    note: { color: '#475467', background: '#f9fafb', border: '1px solid #eaecf0', borderRadius: 6, padding: '9px 12px', fontSize: 13, marginTop: 12 },
    button: { background: '#fff', color: '#315efb', border: '1px solid #c7d2fe', padding: '7px 11px', borderRadius: 6, cursor: 'pointer', fontSize: 13, fontWeight: 700, textDecoration: 'none' },
    actions: { display: 'flex', gap: 8, flexWrap: 'wrap', marginTop: 10 },
};

export default function InteractiveFlamegraph({
    data,
    loading = false,
    emptyMessage = '暂无火焰图数据',
    loadingMessage = '正在加载火焰图...',
    externalUrl = '',
    externalLabel = '打开原始视图',
    filterText = '',
    boxStyle = {},
    minWidth = 1120,
}) {
    const graphRef = useRef(null);
    const chartRef = useRef(null);
    const [renderError, setRenderError] = useState('');
    const [renderMode, setRenderMode] = useState('full');
    const hasData = data && !data.empty && Array.isArray(data.nodes) && data.nodes.length > 0;
    const profileStats = useMemo(() => hasData ? {
        nodes: countProfileNodes(data.nodes),
        depth: maxProfileDepth(data.nodes),
    } : { nodes: 0, depth: 0 }, [data, hasData]);
    const shouldUseSafeView = hasData && (profileStats.nodes > SAFE_MAX_NODES || profileStats.depth > SAFE_MAX_DEPTH);
    const renderData = useMemo(() => {
        if (!hasData) return null;
        return renderMode === 'safe' ? pruneFlamegraphForRender(data, SAFE_MAX_DEPTH, SAFE_MAX_NODES) : data;
    }, [data, hasData, renderMode]);
    const d3Data = useMemo(() => renderData ? miniDropToD3Flamegraph(renderData) : null, [renderData]);

    useEffect(() => {
        setRenderMode(shouldUseSafeView ? 'safe' : 'full');
        setRenderError('');
    }, [data, shouldUseSafeView]);

    useEffect(() => {
        if (!graphRef.current || !hasData || !renderData || !d3Data) return undefined;
        const selection = select(graphRef.current);
        selection.selectAll('*').remove();
        setRenderError('');
        try {
            const width = Math.max(graphRef.current.clientWidth - 16 || minWidth, minWidth);
            const height = Math.max(320, Math.min(MAX_CHART_HEIGHT, (maxProfileDepth(renderData.nodes) + 3) * 18));
            const chart = createFlamegraph()
                .width(width)
                .height(height)
                .cellHeight(17)
                .transitionDuration(120)
                .minFrameSize(0.8)
                .sort(true)
                .label(d => formatFlamegraphLabel(d, data.unit))
                .title('');
            chartRef.current = chart;
            selection.datum(d3Data).call(chart);
        } catch (err) {
            chartRef.current = null;
            if (renderMode === 'full') {
                setRenderMode('safe');
            } else {
                setRenderError(err?.message || '火焰图渲染失败');
            }
        }
        return () => {
            chartRef.current = null;
            selection.selectAll('*').remove();
        };
    }, [data, d3Data, hasData, minWidth, renderData, renderMode]);

    if (loading && !hasData) return <div style={BASE.empty}>{loadingMessage}</div>;
    if (!hasData) {
        return <FlamegraphEmpty message={data?.message || (filterText ? `该时间范围内 ${filterText} 无样本` : emptyMessage)} url={data?.profile_url || externalUrl} label={externalLabel} />;
    }

    return (
        <>
            <div ref={graphRef} style={{ ...BASE.flameBox, ...boxStyle }} />
            {renderMode === 'safe' && !renderError && (
                <div style={BASE.note}>
                    全量数据节点 {profileStats.nodes}、深度 {profileStats.depth}，已使用安全视图保证渲染稳定。
                    <div style={BASE.actions}>
                        <button type="button" style={BASE.button} onClick={() => setRenderMode('full')}>重试全量渲染</button>
                        {(data?.profile_url || externalUrl) && <a href={data?.profile_url || externalUrl} target="_blank" rel="noreferrer" style={BASE.button}>{externalLabel}</a>}
                    </div>
                </div>
            )}
            {renderError && <div style={{ ...BASE.warn, marginTop: 12 }}>火焰图渲染失败：{renderError}</div>}
            {renderError && <FlamegraphEmpty message="仍可通过原始视图或下方 TopN 排查热点。" url={data?.profile_url || externalUrl} label={externalLabel} />}
        </>
    );
}

function FlamegraphEmpty({ message, url, label }) {
    return (
        <div style={BASE.empty}>
            <div style={{ fontWeight: 700, color: '#475467', marginBottom: 6 }}>{message}</div>
            {url && <a href={url} target="_blank" rel="noreferrer" style={BASE.button}>{label}</a>}
        </div>
    );
}

function miniDropToD3Flamegraph(data) {
    return {
        name: 'root',
        value: Number(data?.total || sumProfileNodeValues(data?.nodes || [])) || 1,
        children: toD3Nodes(data?.nodes || []),
    };
}

function toD3Nodes(nodes) {
    const roots = [];
    const stack = (nodes || []).slice().reverse().map(node => ({ node, target: roots }));
    while (stack.length > 0) {
        const { node, target } = stack.pop();
        const output = {
            name: node.name || 'unknown',
            value: Math.max(0, Number(node.value || 0)),
        };
        const children = Array.isArray(node.children) ? node.children : [];
        if (children.length > 0) {
            output.children = [];
            for (let index = children.length - 1; index >= 0; index -= 1) {
                stack.push({ node: children[index], target: output.children });
            }
        }
        target.push(output);
    }
    return roots;
}

export function countProfileNodes(nodes) {
    let count = 0;
    const stack = [...(nodes || [])];
    while (stack.length > 0) {
        const node = stack.pop();
        count += 1;
        if (Array.isArray(node.children)) stack.push(...node.children);
    }
    return count;
}

export function maxProfileDepth(nodes) {
    let maxDepth = 0;
    const stack = (nodes || []).map(node => ({ node, depth: 1 }));
    while (stack.length > 0) {
        const { node, depth } = stack.pop();
        maxDepth = Math.max(maxDepth, depth);
        if (Array.isArray(node.children)) {
            node.children.forEach(child => stack.push({ node: child, depth: depth + 1 }));
        }
    }
    return maxDepth;
}

function sumProfileNodeValues(nodes) {
    return (nodes || []).reduce((sum, node) => sum + Number(node.value || 0), 0);
}

function pruneFlamegraphForRender(data, maxDepth, maxNodes) {
    let used = 0;
    const rootTotal = Number(data?.total || sumProfileNodeValues(data?.nodes || [])) || 1;
    const pruneNode = (node, depth) => {
        if (!node || used >= maxNodes) return null;
        used += 1;
        const children = Array.isArray(node.children) ? node.children : [];
        const keptChildren = [];
        if (depth < maxDepth && used < maxNodes) {
            for (const child of children) {
                if (used >= maxNodes) break;
                const kept = pruneNode(child, depth + 1);
                if (kept) keptChildren.push(kept);
            }
        }
        return { ...node, children: keptChildren };
    };
    const nodes = [];
    for (const node of data?.nodes || []) {
        if (used >= maxNodes) break;
        const kept = pruneNode(node, 1);
        if (kept) nodes.push(kept);
    }
    return {
        ...data,
        nodes,
        total: rootTotal,
    };
}

export function foldedTextToFlamegraph(text, source = 'folded') {
    const root = { name: 'root', value: 0, children: new Map() };
    const lines = String(text || '').split(/\r?\n/);
    for (const line of lines) {
        const trimmed = line.trim();
        if (!trimmed || trimmed.startsWith('#')) continue;
        const match = trimmed.match(/^(.*)\s+([0-9]+(?:\.[0-9]+)?)$/);
        if (!match) continue;
        const frames = match[1].split(';').map(frame => frame.trim()).filter(Boolean);
        const value = Number(match[2]);
        if (frames.length === 0 || !Number.isFinite(value) || value <= 0) continue;
        root.value += value;
        let cursor = root;
        for (const frame of frames) {
            if (!cursor.children.has(frame)) {
                cursor.children.set(frame, { name: frame, value: 0, children: new Map() });
            }
            cursor = cursor.children.get(frame);
            cursor.value += value;
        }
    }
    const nodes = mapChildrenToNodes(root.children, 'fg');
    return {
        nodes,
        total: root.value,
        unit: 'samples',
        empty: nodes.length === 0,
        message: nodes.length === 0 ? 'folded stacks 中没有可渲染的样本' : '',
        source,
        generated_at: new Date().toISOString(),
    };
}

function mapChildrenToNodes(children, prefix) {
    return Array.from(children.values())
        .sort((a, b) => Number(b.value || 0) - Number(a.value || 0))
        .map((node, index) => {
            const id = `${prefix}-${index}`;
            const childNodes = mapChildrenToNodes(node.children, id);
            const childTotal = childNodes.reduce((sum, child) => sum + Number(child.value || 0), 0);
            return {
                id,
                name: node.name || 'unknown',
                value: node.value,
                self: Math.max(0, node.value - childTotal),
                children: childNodes,
            };
        });
}
