jest.mock('d3-selection', () => ({ select: jest.fn() }));
jest.mock('d3-flame-graph', () => ({ flamegraph: jest.fn() }));

import {
    countProfileNodes,
    flamegraphRenderConfig,
    matchesFlamegraphFrame,
    pruneFlamegraphForRender,
} from './InteractiveFlamegraph';

test('full flamegraph rendering never filters subpixel frames', () => {
    expect(flamegraphRenderConfig(5000)).toEqual({ minFrameSize: 0, transitionDuration: 0 });
    expect(flamegraphRenderConfig(100)).toEqual({ minFrameSize: 0, transitionDuration: 120 });
});

test('frame search is literal and case insensitive', () => {
    expect(matchesFlamegraphFrame('main.burnCPU.func1+0x1', 'BURNcpu')).toBe(true);
    expect(matchesFlamegraphFrame('runtime.map[foo]', 'map[foo]')).toBe(true);
    expect(matchesFlamegraphFrame('runtime.map', '[')).toBe(false);
});

test('safe fallback reports a bounded subset without changing the source total', () => {
    const data = {
        total: 10,
        nodes: [{ name: 'a', value: 10, children: [{ name: 'b', value: 5 }, { name: 'c', value: 5 }] }],
    };
    const safe = pruneFlamegraphForRender(data, 80, 2);
    expect(countProfileNodes(data.nodes)).toBe(3);
    expect(countProfileNodes(safe.nodes)).toBe(2);
    expect(safe.total).toBe(10);
});
