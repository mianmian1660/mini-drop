import React, { useRef, useState } from 'react';

const BUBBLE_WIDTH = 240;
// 气泡高度估算（240px 宽、12px 字号下一般 2~3 行文字），用于判断上方空间是否足够。
const BUBBLE_HEIGHT_EST = 120;
const GAP = 7;
const VIEWPORT_MARGIN = 8;

const S = {
    wrap: { position: 'relative', display: 'inline-flex', alignItems: 'center', marginLeft: 5, verticalAlign: 'middle' },
    trigger: { width: 16, height: 16, padding: 0, border: '1px solid #98a2b3', borderRadius: '50%', background: '#fff', color: '#667085', fontSize: 11, fontWeight: 700, lineHeight: '14px', cursor: 'help' },
    bubble: { position: 'fixed', zIndex: 2000, width: BUBBLE_WIDTH, padding: '8px 10px', borderRadius: 6, background: '#101828', color: '#fff', fontSize: 12, lineHeight: 1.45, fontWeight: 400, boxShadow: '0 4px 12px rgba(16,24,40,.2)', whiteSpace: 'normal' },
};

// 通用悬停提示气泡。用 fixed 定位 + 打开时测量图标位置，把气泡放在图标
// 下方（空间不足时放上方），并限制在视口内——避免在弹窗/表格等带
// overflow 裁剪的容器里被切掉（例如弹窗顶部字段的气泡被裁成半截）。
export default function InfoTooltip({ children, label = '查看说明' }) {
    const [open, setOpen] = useState(false);
    const [pos, setPos] = useState({ left: 0, top: 0 });
    const wrapRef = useRef(null);

    const show = () => {
        const rect = wrapRef.current?.getBoundingClientRect();
        if (rect) {
            const left = Math.max(VIEWPORT_MARGIN, Math.min(rect.left, window.innerWidth - BUBBLE_WIDTH - VIEWPORT_MARGIN));
            const spaceBelow = window.innerHeight - rect.bottom;
            const top = spaceBelow >= BUBBLE_HEIGHT_EST + GAP
                ? rect.bottom + GAP
                : Math.max(VIEWPORT_MARGIN, rect.top - BUBBLE_HEIGHT_EST - GAP);
            setPos({ left, top });
        }
        setOpen(true);
    };

    return <span style={S.wrap} ref={wrapRef}>
        <button type="button" aria-label={label} aria-expanded={open} style={S.trigger}
            onMouseEnter={show} onMouseLeave={() => setOpen(false)}
            onFocus={show} onBlur={() => setOpen(false)}>i</button>
        {open && <span role="tooltip" style={{ ...S.bubble, left: pos.left, top: pos.top }}>{children}</span>}
    </span>;
}
