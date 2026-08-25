import React from 'react';
import { createRoot } from 'react-dom/client';
import { act, Simulate } from 'react-dom/test-utils';

jest.mock('../api', () => ({
    sentinelRules: {
        list: jest.fn(),
        create: jest.fn(),
        delete: jest.fn(),
    },
}));

import { sentinelRules } from '../api';
import SentinelCard from './SentinelCard';

async function render(props) {
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    await act(async () => {
        root.render(<SentinelCard {...props} />);
    });
    await act(async () => { await Promise.resolve(); });
    return { container, root };
}

beforeEach(() => {
    sentinelRules.list.mockReset();
    sentinelRules.create.mockReset();
    sentinelRules.delete.mockReset();
});

test('没有可判异信号时提示不可用', async () => {
    sentinelRules.list.mockResolvedValue({ code: 0, data: { rules: [] } });
    const { container, root } = await render({ targetIP: '10.0.0.9', signals: ['cpu_profile'] });

    expect(container.textContent).toContain('当前会话没有可用于哨兵判异的信号');

    act(() => root.unmount());
    container.remove();
});
