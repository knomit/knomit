import { it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor, within } from '@testing-library/react';
import { TrailBreadcrumb } from './TrailBreadcrumb';
import { api } from './api';

vi.mock('./api', async (orig) => {
  const mod = await orig<typeof import('./api')>();
  return { ...mod, api: { ...mod.api, fact: vi.fn() } };
});

beforeEach(() => {
  // Title = "T <path>" so each crumb is individually identifiable.
  (api.fact as any).mockImplementation(async (_r: string, _b: string, path: string) => ({
    path, title: `T ${path}`,
    body: '', domain: [], confidence: 0, sources: 0, entities: [], refs: [],
  }));
});

const live = { mode: 'live' as const };
const hist = (commit: string) => ({ mode: 'history' as const, commit });

const trail = [
  { factPath: 'kb/a.md', asOf: live },
  { factPath: 'kb/b.md', asOf: hist('bbb1111') },
];

it('fires jump (by index)', async () => {
  const onJump = vi.fn();
  render(<TrailBreadcrumb repo="r" branch="b" trail={trail} onJump={onJump} />);
  await waitFor(() => screen.getByText('T kb/a.md'));
  fireEvent.click(screen.getByText('T kb/a.md'));
  expect(onJump).toHaveBeenCalledWith(0);
});

it('shows the fetched fact title, not the hash filename', async () => {
  render(<TrailBreadcrumb repo="r" branch="b" trail={trail} onJump={vi.fn()} />);
  await waitFor(() => screen.getByText('T kb/b.md'));
});

// 6-crumb trail collapses to: root › … › secondLast › last
const longTrail = ['a', 'b', 'c', 'd', 'e', 'f'].map((c, i) => ({
  factPath: `kb/${c}.md`,
  asOf: i === 0 ? live : hist(`${c}${c}${c}1111`),
}));

it('collapses a long trail to root + ellipsis + last two', async () => {
  render(<TrailBreadcrumb repo="r" branch="b" trail={longTrail} onJump={vi.fn()} />);
  await waitFor(() => screen.getByText('T kb/a.md'));   // root
  expect(screen.getByText('T kb/e.md')).toBeTruthy();   // second-last
  expect(screen.getByText('T kb/f.md')).toBeTruthy();   // last
  // Hidden middle crumbs are not shown inline.
  expect(screen.queryByText('T kb/b.md')).toBeNull();
  expect(screen.queryByText('T kb/c.md')).toBeNull();
  expect(screen.queryByText('T kb/d.md')).toBeNull();
  // Ellipsis affordance present.
  expect(screen.getByTestId('crumb-overflow')).toBeTruthy();
});

it('ellipsis dropdown lists hidden crumbs and jumps to the right index', async () => {
  const onJump = vi.fn();
  render(<TrailBreadcrumb repo="r" branch="b" trail={longTrail} onJump={onJump} />);
  await waitFor(() => screen.getByText('T kb/a.md'));
  fireEvent.click(screen.getByTestId('crumb-overflow'));
  const menu = screen.getByTestId('crumb-overflow-menu');
  // Hidden crumbs are indices 1,2,3 → b,c,d.
  expect(within(menu).getByText('T kb/b.md')).toBeTruthy();
  expect(within(menu).getByText('T kb/d.md')).toBeTruthy();
  fireEvent.click(within(menu).getByText('T kb/c.md')); // index 2
  expect(onJump).toHaveBeenCalledWith(2);
});

it('closes the overflow dropdown on Escape', async () => {
  render(<TrailBreadcrumb repo="r" branch="b" trail={longTrail} onJump={vi.fn()} />);
  await waitFor(() => screen.getByText('T kb/a.md'));
  fireEvent.click(screen.getByTestId('crumb-overflow'));
  expect(screen.queryByTestId('crumb-overflow-menu')).toBeTruthy();
  fireEvent.keyDown(document, { key: 'Escape' });
  await waitFor(() => expect(screen.queryByTestId('crumb-overflow-menu')).toBeNull());
});

it('renders all crumbs without an ellipsis for a short (<=4) trail', async () => {
  const four = ['a', 'b', 'c', 'd'].map((c, i) => ({
    factPath: `kb/${c}.md`, asOf: i === 0 ? live : hist(`${c}${c}${c}1111`),
  }));
  render(<TrailBreadcrumb repo="r" branch="b" trail={four} onJump={vi.fn()} />);
  await waitFor(() => screen.getByText('T kb/a.md'));
  expect(screen.getByText('T kb/b.md')).toBeTruthy();
  expect(screen.getByText('T kb/c.md')).toBeTruthy();
  expect(screen.getByText('T kb/d.md')).toBeTruthy();
  expect(screen.queryByTestId('crumb-overflow')).toBeNull();
});
