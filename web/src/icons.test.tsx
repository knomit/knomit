import { it, expect } from 'vitest';
import { render } from '@testing-library/react';
import { LayersIcon, TypeIcon, FrameIcon } from './icons';

it('LayersIcon renders the Feather "layers" polygon and two polylines', () => {
  const { container } = render(<LayersIcon color="#a8a4f0" />);
  const polygon = container.querySelector('polygon');
  const polylines = container.querySelectorAll('polyline');
  expect(polygon?.getAttribute('points')).toBe('12 2 2 7 12 12 22 7 12 2');
  expect(polylines).toHaveLength(2);
  expect(polylines[0].getAttribute('points')).toBe('2 12 12 17 22 12');
  expect(polylines[1].getAttribute('points')).toBe('2 17 12 22 22 17');
});

it('LayersIcon applies color and size props like sibling icons', () => {
  const { container } = render(<LayersIcon color="#a8a4f0" size={20} />);
  const svg = container.querySelector('svg');
  expect(svg?.getAttribute('stroke')).toBe('#a8a4f0');
  expect(svg?.getAttribute('width')).toBe('20');
  expect(svg?.getAttribute('height')).toBe('20');
});

it('TypeIcon draws a frame with FrameIcon, not the UnknownIcon fallback', () => {
  const frame = render(<TypeIcon type="frame" color="#4dd" />).container.innerHTML;
  const own = render(<FrameIcon color="#4dd" size={12} />).container.innerHTML;
  const unknown = render(<TypeIcon type="no-such-type" color="#4dd" />).container.innerHTML;
  expect(frame).toBe(own);
  expect(frame).not.toBe(unknown);
});
