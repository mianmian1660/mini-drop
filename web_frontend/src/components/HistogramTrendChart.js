// ============================================================
// components/HistogramTrendChart.js — histogram 趋势时间轴 + 哨兵触发标记
// ============================================================
// 按 docs/sentinel-rule-frontend-design.md §4 的修正版设计：不是另起一个
// 独立组件叠加在原图上层各管一份比例尺，而是标记和趋势曲线在同一个 draw()
// 里共用同一份 d3 时间比例尺——一张图、一套 hover/点击逻辑，照抄
// TimelineChart.js 的骨架（mousemove/mouseleave tooltip、onClick 跳转）。
// ============================================================

import React, { useRef, useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import * as d3 from 'd3';
import { formatDateTime, formatTimeShort } from '../utils/time';
import { clampTooltipX } from './TimelineChart';

const HEIGHT = 150;
const AXIS_Y = 110;
const PAD_L = 40;
const PAD_R = 16;

export default function HistogramTrendChart({ trend = [], events = [], unit = 'us', metric = 'p99' }) {
    const wrapRef = useRef(null);
    const svgRef = useRef(null);
    const navigate = useNavigate();
    const [width, setWidth] = useState(0);
    const [tip, setTip] = useState(null);

    useEffect(() => {
        const el = wrapRef.current;
        if (!el) return undefined;
        setWidth(el.clientWidth);
        const ro = new ResizeObserver(entries => { for (const entry of entries) setWidth(entry.contentRect.width); });
        ro.observe(el);
        return () => ro.disconnect();
    }, []);

    useEffect(() => {
        if (!width || trend.length === 0) return undefined;
        const svg = d3.select(svgRef.current);
        svg.selectAll('*').remove();

        const points = trend.map(p => ({ ...p, _t: new Date(p.window_start), _v: Number(p[metric]) || 0 }));
        let t0 = d3.min(points, p => p._t);
        let t1 = d3.max(points, p => p._t);
        if (t1.getTime() - t0.getTime() < 1000) { t0 = new Date(t0.getTime() - 60000); t1 = new Date(t1.getTime() + 60000); }
        const pad = (t1.getTime() - t0.getTime()) * 0.04;
        const x = d3.scaleTime().domain([new Date(t0.getTime() - pad), new Date(t1.getTime() + pad)]).range([PAD_L, width - PAD_R]);
        const maxV = Math.max(1, d3.max(points, p => p._v) || 1, ...events.map(e => Number(e.observed_value) || 0));
        const y = d3.scaleLinear().domain([0, maxV * 1.15]).range([AXIS_Y - 10, 20]);

        const gAxis = svg.append('g').attr('transform', `translate(0,${AXIS_Y})`);
        gAxis.call(d3.axisBottom(x).ticks(Math.max(2, Math.floor((width - PAD_L - PAD_R) / 90))).tickFormat(formatTimeShort));
        gAxis.selectAll('text').attr('fill', '#888').attr('font-size', 11);
        gAxis.selectAll('line,path').attr('stroke', '#ccc');

        const line = d3.line().x(p => x(p._t)).y(p => y(p._v));
        svg.append('path').datum(points).attr('d', line).attr('fill', 'none').attr('stroke', '#8aa4d6').attr('stroke-width', 2);
        svg.selectAll('.trend-dot').data(points).join('circle').attr('class', 'trend-dot')
            .attr('cx', p => x(p._t)).attr('cy', p => y(p._v)).attr('r', 2.5).attr('fill', '#8aa4d6').attr('opacity', 0.6);

        const firedEvents = events.filter(e => e.status === 'fired');
        const gMarkers = svg.append('g');
        gMarkers.selectAll('.marker-line').data(firedEvents).join('line').attr('class', 'marker-line')
            .attr('x1', e => x(new Date(e.evaluated_at))).attr('x2', e => x(new Date(e.evaluated_at)))
            .attr('y1', 14).attr('y2', AXIS_Y)
            .attr('stroke', '#b42318').attr('stroke-width', 1.2).attr('stroke-dasharray', '3,2');
        gMarkers.selectAll('.marker-dot').data(firedEvents).join('circle').attr('class', 'marker-dot')
            .attr('cx', e => x(new Date(e.evaluated_at))).attr('cy', 14).attr('r', 5.5)
            .attr('fill', '#b42318').style('cursor', e => (e.child_tid ? 'pointer' : 'default'))
            .on('click', (ev, e) => { if (e.child_tid) navigate(`/task/result?tid=${encodeURIComponent(e.child_tid)}`); })
            .on('mousemove', (ev, e) => {
                const [px, py] = d3.pointer(ev, wrapRef.current);
                setTip({
                    x: px, y: py,
                    lines: [
                        `${metric} ${formatUnitValue(e.observed_value, unit)}（阈值 ${formatUnitValue(e.floor_value, unit)}）`,
                        `${formatDateTime(e.evaluated_at)} · ${e.child_tid ? '已触发深度诊断，点击查看' : '已触发'}`,
                    ],
                });
            })
            .on('mouseleave', () => setTip(null));

        return () => setTip(null);
    }, [trend, events, width, metric, unit, navigate]);

    if (trend.length === 0) return null;

    const tooltipMaxWidth = Math.max(0, Math.min(320, width - 16));
    const tooltipX = tip ? clampTooltipX(tip.x, width, tooltipMaxWidth) : 0;

    return (
        <div ref={wrapRef} style={{ position: 'relative', width: '100%', minWidth: 0, maxWidth: '100%' }}>
            <svg ref={svgRef} width="100%" height={HEIGHT} style={{ display: 'block' }} />
            <div style={{ display: 'flex', gap: 16, flexWrap: 'wrap', marginTop: 4, fontSize: 12, color: '#666' }}>
                <span><span style={{ display: 'inline-block', width: 9, height: 2, background: '#8aa4d6', marginRight: 5, verticalAlign: 'middle' }} />{metric} 走势</span>
                <span><span style={{ display: 'inline-block', width: 9, height: 9, borderRadius: '50%', background: '#b42318', marginRight: 5, verticalAlign: 'middle' }} />哨兵触发</span>
            </div>
            {tip && (
                <div style={{
                    position: 'absolute', left: tooltipX, top: tip.y - 10, transform: 'translate(-50%, -100%)',
                    background: 'rgba(40,40,40,0.94)', color: '#fff', padding: '6px 10px', borderRadius: 4, fontSize: 12,
                    width: 'max-content', maxWidth: tooltipMaxWidth, lineHeight: 1.5, whiteSpace: 'normal',
                    overflowWrap: 'anywhere', wordBreak: 'break-word', pointerEvents: 'none', zIndex: 5,
                }}>
                    {tip.lines.map((l, i) => <div key={i}>{l}</div>)}
                </div>
            )}
        </div>
    );
}

function formatUnitValue(value, unit) {
    const num = Number(value) || 0;
    if (unit === 'us') return num >= 1000 ? `${(num / 1000).toFixed(1)} ms` : `${Math.round(num)} us`;
    return `${num}`;
}
