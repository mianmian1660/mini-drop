import React from 'react';
import { createRoot } from 'react-dom/client';
import { act, Simulate } from 'react-dom/test-utils';

jest.mock('../workers/createFlamegraphWorker', () => jest.fn());
jest.mock('../components/InteractiveFlamegraph', () => ({
    __esModule: true,
    default: () => null,
    foldedTextToFlamegraph: jest.fn(),
}));
jest.mock('../components/JavaFlamegraphPanel', () => () => null);
jest.mock('../components/AICard', () => () => null);

import { ArtifactsPanel } from './TaskResultPage';

global.IS_REACT_ACT_ENVIRONMENT = true;

function renderPanel(overrides = {}) {
    const props = {
        files: [],
        artifacts: [{ id: 1, name: 'perf.data', size: 1024, kind: 'RAW', retention_class: 'raw_large' }],
        cleanedArtifacts: [{ id: 2, name: 'old.svg', size: 2048, kind: 'RESULT', delete_reason: 'expired', deleted_at: '2026-08-23T00:00:00Z' }],
        artifactLinks: {},
        artifactError: '',
        onRefreshDownload: jest.fn(),
        task: { artifacts_pinned: false },
        canManage: true,
        pinBusy: false,
        pinError: '',
        pinReason: '',
        setPinReason: jest.fn(),
        showPinReason: false,
        setShowPinReason: jest.fn(),
        confirmUnpin: false,
        setConfirmUnpin: jest.fn(),
        onPin: jest.fn(),
        onUnpin: jest.fn(),
        ...overrides,
    };
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    act(() => root.render(<ArtifactsPanel {...props} />));
    return { container, root, props };
}

test('展示可用产物、固定入口，并将墓碑默认折叠', () => {
    const { container, root, props } = renderPanel();

    expect(container.textContent).toContain('perf.data');
    expect(container.textContent).toContain('固定全部产物');
    expect(container.textContent).toContain('展开 已清理产物（1）');
    expect(container.textContent).not.toContain('old.svg');

    const buttons = Array.from(container.querySelectorAll('button'));
    act(() => Simulate.click(buttons.find(button => button.textContent === '固定全部产物')));
    expect(props.setShowPinReason).toHaveBeenCalledWith(true);

    act(() => Simulate.click(buttons.find(button => button.textContent.includes('已清理产物'))));
    expect(container.textContent).toContain('old.svg');
    expect(container.textContent).toContain('expired');

    act(() => root.unmount());
    container.remove();
});

test('固定状态展示保护说明和取消固定入口', () => {
    const { container, root, props } = renderPanel({ task: { artifacts_pinned: true } });

    expect(container.textContent).toContain('产物受保护，不会自动清理');
    const cancel = Array.from(container.querySelectorAll('button')).find(button => button.textContent === '取消固定');
    act(() => Simulate.click(cancel));
    expect(props.setConfirmUnpin).toHaveBeenCalledWith(true);

    act(() => root.unmount());
    container.remove();
});
