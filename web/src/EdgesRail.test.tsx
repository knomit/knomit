import { it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { EdgesRail } from './EdgesRail';
import type { RefGroup } from './api';

// The rail is presentational now — App fetches (useFactEdges) and passes the
// edges down, so these render props directly instead of mocking api.explain.
// The three tests that pinned the rail's own fetch and its "I am empty" report
// to App went with that protocol: the anchor one moved to
// useFactEdges.anchor.test.tsx, the rest describe machinery that no longer
// exists.
const incoming: RefGroup[] = [{
  path: 'kb/in.md', title: 'Inbound', type: 'observation', deleted: false,
  versions: [{ commit: 'src9999', committed_at: 1, deleted: false }],
}];
const outgoing: RefGroup[] = [{
  path: 'kb/out.md', title: 'Outbound', type: 'concept', deleted: false,
  versions: [{ commit: 'tgt8888', committed_at: 1, deleted: false }],
}];

const base = { incoming, outgoing, loading: false, error: null };

it('renders IN and OUT groups and hops to the edge pinned commit', () => {
  const onHop = vi.fn();
  render(<EdgesRail {...base} onHop={onHop} />);
  fireEvent.click(screen.getByText('Outbound'));
  expect(onHop).toHaveBeenCalledWith('kb/out.md', 'tgt8888');
});

it('clicking incoming edge hops to its pinned commit', () => {
  const onHop = vi.fn();
  render(<EdgesRail {...base} onHop={onHop} />);
  fireEvent.click(screen.getByText('Inbound'));
  expect(onHop).toHaveBeenCalledWith('kb/in.md', 'src9999');
});

it('renders both groups as empty without claiming an error', () => {
  render(<EdgesRail incoming={[]} outgoing={[]} loading={false} error={null} onHop={() => {}} />);
  expect(screen.getAllByText('none')).toHaveLength(2);
});

// An unreachable server is not the same answer as "this fact stands alone", and
// the rail is the only surface that can say so.
it('surfaces a fetch error', () => {
  render(<EdgesRail incoming={[]} outgoing={[]} loading={false} error="backend down" onHop={() => {}} />);
  expect(screen.getByText(/backend down/)).toBeInTheDocument();
});
