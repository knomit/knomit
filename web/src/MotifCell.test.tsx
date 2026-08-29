// The motif cell: four states, and a name that is never cut.
//
// The longest real motif name in these knowledge bases is 41 characters. It is
// used here rather than a short stand-in because the rule under test only
// matters at length — a fixture that fits could not tell a component that
// truncates from one that does not.

import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { MotifCell } from './MotifCell';
import type { ResolvedMotif } from './useMotifClusters';

const LONGEST = 'threshold-encodes-unmeasured-distribution';

// df deliberately differs from carrier_count: the cell must show the number the
// pivot would land on, and a fixture where the two agree could not tell the
// fields apart. See kb/conventions/testing/wiring-fixtures.
const ok = (motif: string, carrier_count: number): ResolvedMotif => ({
  motif, status: 'ok',
  cluster: {
    cluster_key: 'k', canonical: motif, members: [motif],
    df: carrier_count + 100, carrier_count, carriers: [], aliases: [],
  },
});

const cell = () => screen.getByTestId('motif-cell');

describe('MotifCell', () => {
  it('renders the longest real name whole, with no ellipsis of any kind', () => {
    render(<MotifCell motif={ok(LONGEST, 3)} open={false} onToggle={vi.fn()} panelId="p" />);
    const name = screen.getByTestId('motif-name');
    expect(name.textContent).toBe(LONGEST);
    expect(name.textContent).not.toContain('…');
    // Not just "the text is there": a CSS ellipsis would leave textContent
    // intact and still show the reader half a claim. The component must set no
    // truncation at all, because a clipped motif inverts what it says.
    expect(name.style.textOverflow).toBe('');
    expect(name.style.overflow).toBe('');
    expect(name.style.maxWidth).toBe('');
  });

  it('shows the carrier count when resolved', () => {
    render(<MotifCell motif={ok('failure-presents-as-success', 26)} open={false} onToggle={vi.fn()} panelId="p" />);
    expect(cell()).toHaveTextContent('26');
    expect(cell().getAttribute('data-state')).toBe('ok');
  });

  it('shows dots while the count is in flight — not a zero', () => {
    render(<MotifCell motif={{ motif: 'm', status: 'loading' }} open={false} onToggle={vi.fn()} panelId="p" />);
    expect(cell()).toHaveTextContent('···');
    expect(cell()).not.toHaveTextContent(/\b0\b/);
  });

  it('shows a warning when the count failed, never a zero, and stays clickable', () => {
    const onToggle = vi.fn();
    render(<MotifCell motif={{ motif: 'm', status: 'error', error: 'HTTP 502' }}
      open={false} onToggle={onToggle} panelId="p" />);
    expect(cell()).toHaveTextContent('!');
    expect(cell()).not.toHaveTextContent(/\b0\b/);
    // A failure a reader cannot open is a dead end; the panel carries the text.
    expect(cell().getAttribute('data-interactive')).toBe('true');
    expect(cell().getAttribute('title')).toContain('502');
    fireEvent.click(cell());
    expect(onToggle).toHaveBeenCalledWith('m');
  });

  it('draws an inert but PRESENT cell when the fact has no motifs', () => {
    render(<MotifCell motif={null} open={false} onToggle={vi.fn()} panelId="p" />);
    // Present, so the header is the same shape on every fact in a list; inert,
    // so it offers nothing to click. Both halves matter.
    expect(cell()).toHaveTextContent('same shape');
    expect(cell()).toHaveTextContent('0');
    expect(cell().getAttribute('data-interactive')).toBe('false');
    expect(cell().tagName).toBe('DIV');
  });

  it('marks the open cell on its bottom edge, in its own colour', () => {
    const { rerender } = render(
      <MotifCell motif={ok('m', 5)} open={false} onToggle={vi.fn()} panelId="p" />);
    expect(cell().style.boxShadow).toBe('');
    rerender(<MotifCell motif={ok('m', 5)} open onToggle={vi.fn()} panelId="p" />);
    // currentColor, not a literal: the accent lives on the cell so the glyph,
    // the name, the count and this marker cannot drift apart.
    expect(cell().style.boxShadow).toContain('currentColor');
    expect(cell().getAttribute('aria-expanded')).toBe('true');
  });

  it('names the action rather than restating the name', () => {
    render(<MotifCell motif={ok('absence-encodes-value', 7)} open={false} onToggle={vi.fn()} panelId="p" />);
    expect(cell().getAttribute('title')).toBe('7 facts share this shape');
  });
});
