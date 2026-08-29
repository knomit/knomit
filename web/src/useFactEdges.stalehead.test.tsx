// A LIVE view must not treat its cached head as authoritative.
//
// In a repo context the edges request is anchored at state.headCommit, which is
// refreshed only by the page-load bootstrap, SSE `status` events, and the
// post-task status refresh. A dropped `status` broadcast (issue #178) pins the
// tab to an old commit, and every fact created after it 404s its edges — raw,
// on a fact whose body loads fine at HEAD, reading like data corruption.
//
// Live means "the current head", so a 404 there is a stale cache, not an
// answer: re-read the head and retry. A history/diff anchor is the opposite —
// the commit is the user's pin, and dropping it would violate
// kb/invariants/ui/navigation/every-hop-is-path-plus-commit.
import { it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { useFactEdges } from './useFactEdges';
import { init } from './state';
import type { AppState } from './state';

const urls: string[] = [];

function Probe({ state, onHead }: { state: AppState; onHead?: (head: string) => void }) {
  const edges = useFactEdges(state, onHead);
  if (edges.loading) return <div data-testid="out">loading</div>;
  return <div data-testid="out">{edges.error ?? `ok:${edges.incoming.length}`}</div>;
}

const refsBody = { _embedded: { refs: [{ path: 'kb/x/2.md', title: 'Cites it', commit: 'c1' }] } };

function reply(body: unknown, status = 200): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: status === 404 ? 'Not Found' : 'OK',
    json: () => Promise.resolve(body),
  } as unknown as Response;
}

// The store answers correctly for whatever anchor it is given; the wrong ANCHOR
// is the problem. So: 404 at the stale head, 200 at the fresh one.
beforeEach(() => {
  urls.length = 0;
  vi.stubGlobal('fetch', vi.fn((url: unknown) => {
    const u = String(url);
    urls.push(u);
    if (u.includes('/commits/staleHEAD/') || u.includes('/commits/pinnedSHA/')) {
      return Promise.resolve(reply({
        title: 'Fact not found',
        detail: 'no fact at path "kb/x/1.md" on branch "agent/main" at commit "staleHEAD"',
      }, 404));
    }
    if (u.endsWith('/repos/alpha/branches/agent:main')) {
      return Promise.resolve(reply({ head: 'freshHEAD', embeddings_enabled: false, index_state: 'ready' }));
    }
    return Promise.resolve(reply(refsBody));
  }));
});
afterEach(() => vi.unstubAllGlobals());

const liveStale: AppState = {
  ...init,
  context: { kind: 'repo', repo: 'alpha' },
  repo: 'alpha',
  branch: 'agent/main',
  headCommit: 'staleHEAD',
  factPath: 'kb/x/1.md',
  asOf: { mode: 'live' },
};

it('live 404 at the cached head re-reads the head and retries there', async () => {
  const heads: string[] = [];
  render(<Probe state={liveStale} onHead={h => heads.push(h)} />);

  await waitFor(() => expect(screen.getByTestId('out').textContent).toBe('ok:1'));

  // It asked the server what the head actually is...
  expect(urls.some(u => u.endsWith('/repos/alpha/branches/agent:main'))).toBe(true);
  // ...retried there...
  expect(urls.some(u => u.includes('/commits/freshHEAD/facts/kb/x/1.md/incoming'))).toBe(true);
  // ...and reported the fresh head so the rest of the app stops being stale too.
  expect(heads).toContain('freshHEAD');
});

it('a history pin that 404s is an answer, not a stale cache: no head refresh', async () => {
  const heads: string[] = [];
  render(
    <Probe
      state={{ ...liveStale, headCommit: 'freshHEAD', asOf: { mode: 'history', commit: 'pinnedSHA' } }}
      onHead={h => heads.push(h)}
    />,
  );

  await waitFor(() => expect(screen.getByTestId('out').textContent).toContain('no fact at path'));

  // The pin is the user's intent. Never re-anchor it, and never silently show
  // a different version's edges.
  expect(heads).toEqual([]);
  expect(urls.some(u => u.endsWith('/repos/alpha/branches/agent:main'))).toBe(false);
  expect(urls.every(u => !u.includes('/commits/freshHEAD/'))).toBe(true);
});

it('a live 404 that survives a fresh head is reported, not retried forever', async () => {
  vi.stubGlobal('fetch', vi.fn((url: unknown) => {
    const u = String(url);
    urls.push(u);
    if (u.endsWith('/repos/alpha/branches/agent:main')) {
      // The head has not moved — the 404 is the real answer.
      return Promise.resolve(reply({ head: 'staleHEAD', embeddings_enabled: false, index_state: 'ready' }));
    }
    return Promise.resolve(reply({ title: 'Fact not found', detail: 'gone' }, 404));
  }));

  render(<Probe state={liveStale} />);
  await waitFor(() => expect(screen.getByTestId('out').textContent).toContain('gone'));

  const edgeCalls = urls.filter(u => u.includes('/facts/kb/x/1.md/'));
  expect(edgeCalls.length).toBe(2); // one incoming + one outgoing, no retry storm
});
