// ============================================================
// components/Pagination.js — 通用分页组件
// ============================================================
// 使用方式：
//   <Pagination page={page} totalPages={totalPages} onPageChange={setPage} />
//
// 显示效果：
//   ← 上一页   1  2  [3]  4  5  ...  12   下一页 →
// ============================================================

import React from 'react';

const styles = {
    container: {
        display: 'flex', justifyContent: 'center', alignItems: 'center',
        gap: 6, marginTop: 20, flexWrap: 'wrap',
    },
    btn: {
        padding: '6px 14px', border: '1px solid #ddd', borderRadius: 4,
        background: '#fff', cursor: 'pointer', fontSize: 13, color: '#333',
    },
    btnDisabled: {
        padding: '6px 14px', border: '1px solid #eee', borderRadius: 4,
        background: '#f5f5f5', fontSize: 13, color: '#ccc', cursor: 'not-allowed',
    },
    active: {
        padding: '6px 14px', border: '1px solid #4a6cf7', borderRadius: 4,
        background: '#4a6cf7', color: '#fff', cursor: 'default', fontSize: 13, fontWeight: 'bold',
    },
    ellipsis: { padding: '6px 4px', color: '#999', fontSize: 13 },
    info: { fontSize: 13, color: '#999', marginLeft: 12 },
};

export default function Pagination({ page, totalPages, onPageChange }) {
    if (totalPages <= 1) return null;

    // 生成要显示的页码数组
    const getPages = () => {
        const pages = [];
        const maxShow = 5; // 最多显示 5 个页码按钮

        if (totalPages <= maxShow + 2) {
            // 总页数不多，全部显示
            for (let i = 1; i <= totalPages; i++) pages.push(i);
        } else {
            // 总页数多，显示首尾 + 当前附近
            pages.push(1);

            let start = Math.max(2, page - 1);
            let end = Math.min(totalPages - 1, page + 1);

            // 扩展范围确保显示足够页码
            if (page <= 2) end = Math.min(maxShow, totalPages - 1);
            if (page >= totalPages - 1) start = Math.max(totalPages - maxShow + 1, 2);

            if (start > 2) pages.push('...');
            for (let i = start; i <= end; i++) pages.push(i);
            if (end < totalPages - 1) pages.push('...');

            pages.push(totalPages);
        }
        return pages;
    };

    return (
        <div style={styles.container}>
            <button
                style={page <= 1 ? styles.btnDisabled : styles.btn}
                onClick={() => page > 1 && onPageChange(page - 1)}
                disabled={page <= 1}
            >
                ← 上一页
            </button>

            {getPages().map((p, i) =>
                p === '...' ? (
                    <span key={`ellipsis-${i}`} style={styles.ellipsis}>...</span>
                ) : (
                    <button
                        key={p}
                        style={p === page ? styles.active : styles.btn}
                        onClick={() => p !== page && onPageChange(p)}
                    >
                        {p}
                    </button>
                )
            )}

            <button
                style={page >= totalPages ? styles.btnDisabled : styles.btn}
                onClick={() => page < totalPages && onPageChange(page + 1)}
                disabled={page >= totalPages}
            >
                下一页 →
            </button>

            <span style={styles.info}>共 {totalPages} 页</span>
        </div>
    );
}
