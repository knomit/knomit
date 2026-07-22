import { it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor, within } from '@testing-library/react';
import { TrailBreadcrumb } from './TrailBreadcrumb';
import { api } from './api';

vi.mock('./api', async (orig) => {
  const mod = await orig<typeof import('./api')>();
  return { ...mod, api: { ...mod.api, fact: vi.fn(), getLensFact: vi.fn() } };
});

beforeEach(() => {
  (api.fact as any).mockClear();
  // Title = "T <path>" so each crumb is individually identifiable.
  (api.fact as any).mockImplementation(async (_r: string, _b: string, path: string) => ({
    path, title: `T ${path}`,
    body: '', domain: [], confidence: 0, sources: 0, entities: [], refs: [],
  }));
  (api.getLensFact as any).mockReset();
  (api.getLensFact as any).mockImplementation(async (_lens: string, path: string) => ({
    path, title: `L ${path}`,
    body: '', domain: [], confidence: 0, sources: 0, entities: [], refs: [],
    source: { repo: 'docs', id: 'aaabbbcccddd', branch: 'main' },
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

// Regression: in a lens context, crumbs carry canonical paths (kb://<id12>/…
// for read mounts) that the repo-scoped fact endpoint cannot resolve — a
// commit-anchored api.fact(state.repo, …, "kb://…") 404s server-side. With
// lensName set, titles come from the lens endpoint (which routes canonical
// paths to their mount) and the repo endpoint is never called.
it('lens context: titles fetched via getLensFact with the RAW canonical path, never api.fact', async () => {
  const lensTrail = [
    { factPath: 'kb/a.md', asOf: live },                                    // write-repo fact (bare)
    { factPath: 'kb://aaabbbcccddd/kb/b.md', asOf: hist('bbb1111') },       // read-mount fact (qualified)
  ];
  render(<TrailBreadcrumb repo="core" branch="agent/x" lensName="dev" trail={lensTrail} onJump={vi.fn()} />);
  await waitFor(() => screen.getByText('L kb://aaabbbcccddd/kb/b.md'));
  expect(api.getLensFact).toHaveBeenCalledWith('dev', 'kb/a.md');
  expect(api.getLensFact).toHaveBeenCalledWith('dev', 'kb://aaabbbcccddd/kb/b.md');
  expect(api.fact).not.toHaveBeenCalled();
});

it('lens context: a failed title fetch still falls back to the basename', async () => {
  (api.getLensFact as any).mockRejectedValue(new Error('404'));
  const lensTrail = [{ factPath: 'kb://aaabbbcccddd/kb/deep/fail.md', asOf: hist('ccc2222') }];
  render(<TrailBreadcrumb repo="core" branch="agent/x" lensName="dev" trail={lensTrail} onJump={vi.fn()} />);
  await waitFor(() => screen.getByText('fail')); // basename strips .md
});

// Regression (retracted-crumb breadcrumb bug): the RightPanel already read the
// fact when we navigated to it, so its title is in the shared cache. The
// breadcrumb must use that — never re-fetch — because a retracted fact 404s on
// the live lens single-fact endpoint (which would strand the basename hash).
it('lens context: labels a cached (retracted) crumb from the cache without fetching', async () => {
  (api.getLensFact as any).mockRejectedValue(new Error('404')); // retracted → live endpoint 404s
  const lensTrail = [
    { factPath: 'kb/a.md', asOf: live },
    { factPath: 'kb://aaabbbcccddd/kb/6cf51b30.md', asOf: hist('ddd3333') }, // retracted read-mount fact
  ];
  const titles = {
    'kb/a.md@HEAD': 'Canonical fact',
    'kb://aaabbbcccddd/kb/6cf51b30.md@ddd3333': 'AI agents are compromised via over-broad permissions',
  };
  render(<TrailBreadcrumb repo="core" branch="agent/x" lensName="dev" trail={lensTrail} titles={titles} onJump={vi.fn()} />);
  // The cached title shows even though the live endpoint would 404 for this tombstone.
  await screen.findByText('AI agents are compromised via over-broad permissions');
  expect(screen.getByText('Canonical fact')).toBeTruthy();
  expect(screen.queryByText('6cf51b30')).toBeNull(); // never the raw hash
  expect(api.getLensFact).not.toHaveBeenCalled();     // reused, not re-fetched
});
