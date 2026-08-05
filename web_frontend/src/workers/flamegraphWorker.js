self.onmessage = async (event) => {
    const { url, maxNodes = 6000, maxDepth = 160 } = event.data || {};
    if (!url) {
        self.postMessage({ ok: false, reason: 'missing_url' });
        return;
    }
    try {
        const response = await fetch(url, { credentials: 'include' });
        if (!response.ok) {
            self.postMessage({ ok: false, reason: `http_${response.status}` });
            return;
        }
        const text = await response.text();
        const nodeCount = (text.match(/<rect\b/gi) || []).length;
        const depth = Math.max(
            ...Array.from(text.matchAll(/translate\([^,]+,\s*([0-9.]+)/gi)).map(match => Math.round(Number(match[1]) / 16)),
            0,
        );
        self.postMessage({
            ok: true,
            nodeCount,
            depth,
            truncated: nodeCount > maxNodes || depth > maxDepth,
            maxNodes,
            maxDepth,
        });
    } catch (err) {
        self.postMessage({ ok: false, reason: err?.message || 'worker_failed' });
    }
};
