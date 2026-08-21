jest.mock('d3', () => ({}));

import { clampTooltipX } from './TimelineChart';

test('timeline tooltip is clamped inside both chart edges', () => {
    expect(clampTooltipX(0, 320, 240)).toBe(128);
    expect(clampTooltipX(320, 320, 240)).toBe(192);
    expect(clampTooltipX(160, 320, 240)).toBe(160);
});

test('timeline tooltip width is bounded by a narrow chart', () => {
    expect(clampTooltipX(0, 100, 320)).toBe(50);
    expect(clampTooltipX(100, 100, 320)).toBe(50);
});
