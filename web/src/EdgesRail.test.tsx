import { it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { EdgesRail } from './EdgesRail';
import { api } from './api';

vi.mock('./api', async (orig) => {
  const mod = await orig<typeof import('./api')>();
  return { ...mod, api: { ...mod.api, explain: vi.fn() } };
});

beforeEach(() => {
  (api.explain as any).mockResolvedValue({
    incoming: [{ path: 'kb/in.md', title: 'Inbound', type: 'observation', deleted: false,
      versions: [{ commit: 'src9999', committed_at: 1, deleted: false }] }],
    outgoing: [{ path: 'kb/out.md', title: 'Outbound', type: 'concept', deleted: false,
      versions: [{ commit: 'tgt8888', committed_at: 1, deleted: false }] }],
  });
});

it('renders IN and OUT groups and hops to the edge pinned commit', async () => {
  const onHop = vi.fn();
  render(<EdgesRail repo="r" branch="b" factPath="kb/a.md" anchorCommit="aaa1111" history={false} onHop={onHop} />);
  await waitFor(() => screen.getByText('Outbound'));
  fireEvent.click(screen.getByText('Outbound'));
  expect(onHop).toHaveBeenCalledWith('kb/out.md', 'tgt8888');
});

it('clicking incoming edge hops to its pinned commit', async () => {
  const onHop = vi.fn();
  render(<EdgesRail repo="r" branch="b" factPath="kb/a.md" anchorCommit="aaa1111" history={false} onHop={onHop} />);
  await waitFor(() => screen.getByText('Inbound'));
  fireEvent.click(screen.getByText('Inbound'));
  expect(onHop).toHaveBeenCalledWith('kb/in.md', 'src9999');
});

it('passes fallback only when history', async () => {
  render(<EdgesRail repo="r" branch="b" factPath="kb/a.md" anchorCommit="aaa1111" history={true} onHop={() => {}} />);
  await waitFor(() => expect(api.explain).toHaveBeenCalledWith('r', 'b', 'kb/a.md', 'aaa1111', { fallback: 'before' }));
});
