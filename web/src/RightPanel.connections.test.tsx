// The connections menu lives in the fact header, so RightPanel owns the open
// direction and the hover-close timer. The pieces are unit-tested in
// ConnectionsMenu/ConnectionsPanel; what only exists HERE is the wiring: the
// hover group spans the menu AND the panel, and the panel resets when the fact
// changes.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor, act } from '@testing-library/react';
import { RightPanel } from './RightPanel';
import { init } from './state';
import type { AppState } from './state';
import type { RefGroup } from './api';

vi.mock('./api', () => ({
  api: {
    fact: vi.fn(),
    factDiff: vi.fn().mockResolvedValue({ from: null, to: null }),
    stats: vi.fn().mockResolvedValue(null),
    activity: vi.fn().mockResolvedValue(null),
    explain: vi.fn().mockResolvedValue({ incoming: [], outgoing: [] }),
    factCommits: vi.fn().mockResolvedValue({ entries: [] }),
  },
}));
import { api } from './api';

const edge = (path: string, title: string): RefGroup => ({
  path, title, type: 'observation', deleted: false,
  versions: [{ commit: 'aaa1111', committed_at: 1, deleted: false }],
});

const incoming = [edge('kb/in.md', 'Inbound')];
const outgoing = [edge('kb/out.md', 'Outbound')];

const state: AppState = {
  ...init,
  repo: 'core', branch: 'agent/main', headCommit: 'abc1234',
  factPath: 'kb/a.md',
  asOf: { mode: 'live' },
};

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  (api.fact as ReturnType<typeof vi.fn>).mockResolvedValue({
    path: 'kb/a.md', title: 'A fact', body: 'body', type: 'observation',
    confidence: 0.9, sources: 1, domain: [], entities: [], refs: [],
    commit_hash: 'c0ffee1', commit_date: '2026-07-01T00:00:00Z',
  });
});
afterEach(() => vi.useRealTimers());

const mount = (props = {}) => render(
  <RightPanel state={state} dispatch={vi.fn()} incoming={incoming} outgoing={outgoing} {...props} />,
);

describe('RightPanel — connections menu', () => {
  it('renders both cells in the header and opens the panel on click', async () => {
    mount();
    await waitFor(() => expect(screen.getByTestId('connections-in')).toBeInTheDocument());
    expect(screen.getByTestId('connections-panel')).toHaveAttribute('data-open', 'false');

    fireEvent.click(screen.getByTestId('connections-in'));
    expect(screen.getByTestId('connections-panel')).toHaveAttribute('data-open', 'true');
    expect(screen.getByText('Inbound')).toBeInTheDocument();
  });

  it('swaps direction rather than stacking', async () => {
    mount();
    await waitFor(() => expect(screen.getByTestId('connections-in')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('connections-in'));
    fireEvent.click(screen.getByTestId('connections-out'));

    expect(screen.getAllByTestId('connections-panel')).toHaveLength(1);
    expect(screen.getByText('Outbound')).toBeInTheDocument();
    expect(screen.queryByText('Inbound')).toBeNull();
  });

  it('closes when the lit cell is clicked again', async () => {
    mount();
    await waitFor(() => expect(screen.getByTestId('connections-in')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('connections-in'));
    fireEvent.click(screen.getByTestId('connections-in'));
    expect(screen.getByTestId('connections-panel')).toHaveAttribute('data-open', 'false');
  });

  // THE HOVER GROUP IS THE WHOLE CONTROL SPAN. The menu and the panel are
  // separated by a 6px gap that belongs to neither element, so a group scoped
  // to just one of them would close while the pointer crossed between them.
  it('closes on hover-out after the grace period, and re-entering cancels', async () => {
    mount();
    await waitFor(() => expect(screen.getByTestId('connections-in')).toBeInTheDocument());
    const panel = screen.getByTestId('connections-panel');

    fireEvent.click(screen.getByTestId('connections-in'));
    fireEvent.mouseLeave(panel);
    expect(panel).toHaveAttribute('data-open', 'true');   // not immediately
    act(() => { vi.advanceTimersByTime(300); });
    expect(panel).toHaveAttribute('data-open', 'false');

    // Re-entering within the grace period cancels a pending close.
    fireEvent.click(screen.getByTestId('connections-in'));
    fireEvent.mouseLeave(panel);
    act(() => { vi.advanceTimersByTime(100); });
    fireEvent.mouseEnter(panel);
    act(() => { vi.advanceTimersByTime(500); });
    expect(panel).toHaveAttribute('data-open', 'true');
  });

  it('closes when the open fact changes', async () => {
    const { rerender } = mount();
    await waitFor(() => expect(screen.getByTestId('connections-in')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('connections-in'));
    expect(screen.getByTestId('connections-panel')).toHaveAttribute('data-open', 'true');

    rerender(
      <RightPanel state={{ ...state, factPath: 'kb/b.md' }} dispatch={vi.fn()}
        incoming={incoming} outgoing={outgoing} />,
    );
    await waitFor(() =>
      expect(screen.getByTestId('connections-panel')).toHaveAttribute('data-open', 'false'));
  });

  it('renders a zero cell as inert', async () => {
    mount({ incoming: [] });
    await waitFor(() => expect(screen.getByTestId('connections-in')).toBeInTheDocument());
    const cell = screen.getByTestId('connections-in');
    expect(cell.tagName.toLowerCase()).not.toBe('button');
    fireEvent.click(cell);
    expect(screen.getByTestId('connections-panel')).toHaveAttribute('data-open', 'false');
  });
});
