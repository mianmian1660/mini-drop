import React, { useState } from 'react';

const S = {
    wrap: { position: 'relative', display: 'inline-flex', alignItems: 'center', marginLeft: 5, verticalAlign: 'middle' },
    trigger: { width: 16, height: 16, padding: 0, border: '1px solid #98a2b3', borderRadius: '50%', background: '#fff', color: '#667085', fontSize: 11, fontWeight: 700, lineHeight: '14px', cursor: 'help' },
    bubble: { position: 'absolute', zIndex: 20, left: '50%', bottom: 'calc(100% + 7px)', transform: 'translateX(-50%)', width: 240, padding: '8px 10px', borderRadius: 6, background: '#101828', color: '#fff', fontSize: 12, lineHeight: 1.45, fontWeight: 400, boxShadow: '0 4px 12px rgba(16,24,40,.2)', whiteSpace: 'normal' },
};

export default function InfoTooltip({ children, label = '查看说明' }) {
    const [open, setOpen] = useState(false);
    return <span style={S.wrap}>
        <button type="button" aria-label={label} aria-expanded={open} style={S.trigger}
            onMouseEnter={() => setOpen(true)} onMouseLeave={() => setOpen(false)}
            onFocus={() => setOpen(true)} onBlur={() => setOpen(false)}>i</button>
        {open && <span role="tooltip" style={S.bubble}>{children}</span>}
    </span>;
}
