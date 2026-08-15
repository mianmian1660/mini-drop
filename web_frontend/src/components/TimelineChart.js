// ============================================================
// components/TimelineChart.js — 采集窗口横向时间轴（d3）
// ============================================================
// 与页面下方的纵向列表互补：列表给的是每个窗口的明细，
// 这里给的是"窗口在时间上怎么分布、各自跑了多久"——
// 色块的 x 位置 = 触发时刻，宽度 = 采集时长。
// 支持滚轮缩放、拖动平移、点击色块跳转任务详情。
// ============================================================

import React, { useRef, useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import * as d3 from 'd3';
import { formatDateTime, formatTimeShort } from '../utils/time';

const HEIGHT = 112;    // svg 总高
const BAR_TOP = 30;    // 色块顶端 y
const BAR_H = 38;      // 色块高
const AXIS_Y = 78;     // 坐标轴基线 y
const MIN_BAR_W = 4;   // 色块最小宽度：一小时跨度里 30 秒的窗口只有几像素，太窄点不中
const PAD_L = 20;
const PAD_R = 20;

// 状态配色的唯一来源，页面的徽章和这里的色块共用，避免两处颜色语义漂移
export const statusColor = (status) => {
    if (status === 2) return '#4caf50';  // 已完成
    if (status === 3) return '#f44336';  // 失败
    if (status === 4) return '#7c3aed';  // 上传中
    if (status === 1) return '#2196f3';  // 执行中
    return '#ffc107';                    // 待处理
};

const ST = { 0: '待处理', 1: '执行中', 2: '已完成', 3: '失败', 4: '上传中' };

// 窗口起点：后端 window_start 恒等于 create_time，留 create_time 兜底
const startOf = (p) => new Date(p.window_start || p.create_time);

// 窗口终点：采集参数缺 duration 时后端不下发 window_end（omitempty），
// 这里给一个 30 秒占位时长，保证色块有宽度、比例尺定义域也不会退化。
// e > s 是廉价兜底，挡住脏数据导致终点早于起点的情况。
const endOf = (p) => {
    const s = startOf(p);
    const e = p.window_end ? new Date(p.window_end) : null;
    return e && e > s ? e : new Date(s.getTime() + 30000);
};

export default function TimelineChart({ points }) {
    const wrapRef = useRef(null);
    const svgRef = useRef(null);
    const navigate = useNavigate();
    const [width, setWidth] = useState(0);
    const [tip, setTip] = useState(null);

    // d3 需要确切像素宽度，容器尺寸变化时要重新测量并重绘
    useEffect(() => {
        const el = wrapRef.current;
        if (!el) return undefined;
        setWidth(el.clientWidth);
        const ro = new ResizeObserver((entries) => {
            for (const entry of entries) setWidth(entry.contentRect.width);
        });
        ro.observe(el);
        return () => ro.disconnect();
    }, []);

    useEffect(() => {
        if (!width || !points || points.length === 0) return undefined;

        const svg = d3.select(svgRef.current);
        svg.selectAll('*').remove();   // 每次重绘先清空，否则多次更新会层层叠加

        // 定义域覆盖全部窗口，两端各留 5% 空白，免得首尾色块贴在边上
        let t0 = d3.min(points, startOf);
        let t1 = d3.max(points, endOf);
        if (t1.getTime() - t0.getTime() < 1000) {
            // 只有一个窗口（或全部窗口时刻相同）时给一个可视跨度，否则比例尺退化
            t0 = new Date(t0.getTime() - 60000);
            t1 = new Date(t1.getTime() + 60000);
        }
        const pad = (t1.getTime() - t0.getTime()) * 0.05;
        const x = d3.scaleTime()
            .domain([new Date(t0.getTime() - pad), new Date(t1.getTime() + pad)])
            .range([PAD_L, width - PAD_R]);

        const innerW = Math.max(0, width - PAD_L - PAD_R);

        // 放大后色块会越出坐标轴范围，裁掉
        const clipId = `tlc-clip-${Math.random().toString(36).slice(2, 9)}`;
        svg.append('defs').append('clipPath').attr('id', clipId)
            .append('rect')
            .attr('x', PAD_L).attr('y', 0)
            .attr('width', innerW).attr('height', AXIS_Y);

        const gBars = svg.append('g').attr('clip-path', `url(#${clipId})`);
        const gAxis = svg.append('g').attr('transform', `translate(0,${AXIS_Y})`);

        const makeAxis = (scale) =>
            d3.axisBottom(scale)
                .ticks(Math.max(2, Math.floor(innerW / 90)))
                .tickFormat(formatTimeShort);

        const draw = (scale) => {
            gAxis.call(makeAxis(scale));
            gAxis.selectAll('text').attr('fill', '#888').attr('font-size', 11);
            gAxis.selectAll('line,path').attr('stroke', '#ccc');

            gBars.selectAll('rect')
                .data(points, (d) => d.tid)
                .join('rect')
                .attr('x', (d) => scale(startOf(d)))
                .attr('width', (d) => Math.max(MIN_BAR_W, scale(endOf(d)) - scale(startOf(d))))
                // 生效窗口画得比其他窗口高一圈，配合描边在缩小时也能一眼认出
                .attr('y', (d) => (d.is_effective ? BAR_TOP - 4 : BAR_TOP))
                .attr('height', (d) => (d.is_effective ? BAR_H + 8 : BAR_H))
                .attr('rx', 2)
                .attr('fill', (d) => statusColor(d.status))
                .attr('stroke', (d) => (d.is_effective ? '#1a1a1a' : 'none'))
                .attr('stroke-width', (d) => (d.is_effective ? 1.5 : 0))
                .style('cursor', 'pointer')
                .on('click', (ev, d) => navigate(`/task/result?tid=${d.tid}`))
                .on('mousemove', (ev, d) => {
                    const [px, py] = d3.pointer(ev, wrapRef.current);
                    setTip({
                        x: px,
                        y: py,
                        lines: [
                            d.name || d.tid,
                            `${ST[d.status] || '未知'}${d.is_effective ? ' · 当时生效' : ''}`,
                            `${formatDateTime(startOf(d))} → ${formatDateTime(endOf(d))}`,
                        ],
                    });
                })
                .on('mouseleave', () => setTip(null));
        };

        draw(x);

        const zoom = d3.zoom()
            .scaleExtent([1, 500])
            .extent([[PAD_L, 0], [width - PAD_R, HEIGHT]])
            .translateExtent([[PAD_L, 0], [width - PAD_R, HEIGHT]])
            .on('zoom', (ev) => draw(ev.transform.rescaleX(x)));

        svg.call(zoom);

        return () => {
            svg.on('.zoom', null);   // 卸载时摘掉 zoom 监听，避免重复绑定
            setTip(null);
        };
    }, [points, width, navigate]);

    if (!points || points.length === 0) return null;

    return (
        <div ref={wrapRef} style={{ position: 'relative', width: '100%' }}>
            <svg ref={svgRef} width="100%" height={HEIGHT} style={{ display: 'block' }} />

            <div style={{ display: 'flex', gap: 16, flexWrap: 'wrap', marginTop: 4, fontSize: 12, color: '#666' }}>
                {[2, 3, 1, 4, 0].map((st) => (
                    <span key={st}>
                        <span style={{
                            display: 'inline-block', width: 9, height: 9, borderRadius: 2,
                            background: statusColor(st), marginRight: 5,
                        }} />
                        {ST[st]}
                    </span>
                ))}
                <span>
                    <span style={{
                        display: 'inline-block', width: 9, height: 9, borderRadius: 2,
                        background: '#fff', border: '1.5px solid #1a1a1a', marginRight: 5,
                    }} />
                    当时生效
                </span>
            </div>

            {tip && (
                <div style={{
                    position: 'absolute', left: tip.x, top: tip.y - 10,
                    transform: 'translate(-50%, -100%)',
                    background: 'rgba(40,40,40,0.94)', color: '#fff',
                    padding: '6px 10px', borderRadius: 4, fontSize: 12,
                    lineHeight: 1.5, whiteSpace: 'nowrap', pointerEvents: 'none', zIndex: 5,
                }}>
                    {tip.lines.map((l, i) => <div key={i}>{l}</div>)}
                </div>
            )}
        </div>
    );
}
