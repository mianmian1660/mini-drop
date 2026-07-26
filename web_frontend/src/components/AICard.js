import React from 'react';

const S = {
    card: { background: '#fff', border: '1px solid #c7d7fe', borderRadius: 8, padding: 18, marginBottom: 16, boxShadow: '0 1px 2px rgba(16,24,40,0.04)' },
    title: { margin: '0 0 12px 0', fontSize: 17, color: '#1e3a8a' },
    status: { display: 'inline-flex', padding: '3px 8px', borderRadius: 999, fontSize: 12, fontWeight: 700, marginBottom: 12 },
    block: { margin: '0 0 12px 0', color: '#344054', fontSize: 13, lineHeight: 1.65, whiteSpace: 'pre-wrap', wordBreak: 'break-word' },
    label: { display: 'block', color: '#475467', fontSize: 12, fontWeight: 700, marginBottom: 4 },
    evidence: { margin: 0, paddingLeft: 18, color: '#344054', fontSize: 13, lineHeight: 1.65 },
    error: { color: '#b42318' },
};

export default function AICard({ attribution }) {
    if (!attribution) return null;
    const status = String(attribution.status || 'pending');
    const tone = status === 'completed'
        ? { background: '#dcfce7', color: '#166534' }
        : status === 'error'
            ? { background: '#fee4e2', color: '#b42318' }
            : { background: '#f1f5f9', color: '#475467' };
    const evidence = Array.isArray(attribution.evidence) ? attribution.evidence : [];

    return (
        <section style={S.card} aria-label="智能归因">
            <h3 style={S.title}>智能归因</h3>
            <span style={{ ...S.status, ...tone }}>{statusLabel(status)}</span>
            {attribution.reasoning_summary && <TextBlock label="归因摘要" value={attribution.reasoning_summary} />}
            {attribution.suggestion && <TextBlock label="建议" value={attribution.suggestion} />}
            {evidence.length > 0 && (
                <div style={S.block}>
                    <span style={S.label}>支撑证据</span>
                    <ul style={S.evidence}>
                        {evidence.slice(0, 8).map((item, index) => (
                            <li key={`${item.function || 'evidence'}-${index}`}>
                                {cleanText(item.function ? `${item.function}: ${item.detail || ''}` : item.detail || '')}
                            </li>
                        ))}
                    </ul>
                </div>
            )}
            {status === 'error' && <p style={{ ...S.block, ...S.error }}>本次未采用无法验证的模型结论。</p>}
        </section>
    );
}

function TextBlock({ label, value }) {
    return (
        <p style={S.block}>
            <span style={S.label}>{label}</span>
            {cleanText(value)}
        </p>
    );
}

function statusLabel(status) {
    return { completed: '归因完成', skipped: '未启用', error: '归因未完成', pending: '归因中' }[status] || status;
}

// React 文本节点不会执行 HTML；这里额外移除标签和不安全 Markdown 链接。
function cleanText(value) {
    return String(value || '')
        .replace(/<[^>]*>/g, '')
        .replace(/!?\[([^\]]*)\]\((?:javascript|data):[^)]*\)/gi, '$1')
        .replace(/[\u0000-\u0008\u000b\u000c\u000e-\u001f]/g, '')
        .slice(0, 2400);
}
