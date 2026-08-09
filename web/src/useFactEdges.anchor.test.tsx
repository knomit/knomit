// Task 17 fix, now pinned at the hook that owns the fetch. For a read-mount lens
// fact in LIVE mode the edges request must hit the fact's MOUNT repo/branch +
// relative path (factHistoryAnchor) at live HEAD — NOT a commit-anchored URL
// carrying the WRITE repo's head sha, which does not exist in the read mount and
// yields empty edges in the default view.
//
// This used to render EdgesRail with App-derived props. The rail no longer
// fetches: useFactEdges does, for RightPanel and the connections panel at once,
// so the anchor derivation this pins moved inside the hook and the test follows
// it. Uses the REAL api.explain, spying on fetch to observe the exact URL.
import { it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, waitFor } from '@testing-library/react';
import { useFactEdges } from './useFactEdges';
import { edgeAnchorCommit, init } from './state';
import type { AppState } from './state';
import type { Lens, LensSource } from './api';

// Minimal probe: the hook's only observable here is the request it issues.
function Probe({ state }: { state: AppState }) {
  useFactEdges(state);
  return null;
}

const urls: string[] = [];

beforeEach(() => {
  urls.length = 0;
  vi.stubGlobal('fetch', vi.fn((url: unknown) => {
    urls.push(String(url));
    return Promise.resolve({ ok: true, json: () => Promise.resolve({ _embedded: { refs: [] } }) } as Response);
  }));
});
afterEach(() => vi.unstubAllGlobals());

const lens: Lens = { name: 'eng', write: { uid: 'uid-core', name: 'core' }, reads: [{ uid: 'uid-core', name: 'core' }, { uid: 'uid-docs', name: 'docs' }] };
const readSource: LensSource = { repo: 'docs', id: 'docsid123456', branch: 'main' };

// The state App would hold: a lens context, live, with a docs read-mount fact
// open. state.headCommit is the WRITE repo (core) head — the sha that must NOT
// leak into the mount's explain URL.
const liveReadMountState: AppState = {
  ...init,
  context: { kind: 'lens', name: 'eng' },
  lens,
  repo: 'core', branch: 'agent/main', headCommit: 'coreHEADsha',
  factPath: 'kb://docsid123456/kb/api/auth.md',
  factSource: readSource,
  asOf: { mode: 'live' },
};

it('live read-mount fact: edges fetch hits the MOUNT at live HEAD, with NO commit sha', async () => {
  // Guard: the anchor derivation drops the write head for a lens context.
  expect(edgeAnchorCommit(liveReadMountState)).toBe('');

  render(<Probe state={liveReadMountState} />);
  await waitFor(() => expect(urls.length).toBeGreaterThan(0));

  // Mount repo + branch, non-commit-anchored (live HEAD).
  expect(urls.some(u => u.includes('/repos/docs/branches/main/facts/kb/api/auth.md/incoming'))).toBe(true);
  // Never a commit-anchored URL, and never the write repo's head sha.
  expect(urls.every(u => !u.includes('/commits/'))).toBe(true);
  expect(urls.every(u => !u.includes('coreHEADsha'))).toBe(true);
  expect(urls.every(u => !u.includes('/repos/core/'))).toBe(true);
});

// The guard RightPanel had and the rail did not. Fetching before the lens mount
// resolves reads the WRITE repo, and the resulting empty edge set looks exactly
// like a fact with no connections.
it('does not fetch in a lens context until the fact source has resolved', async () => {
  render(<Probe state={{ ...liveReadMountState, factSource: null }} />);
  await new Promise(r => setTimeout(r, 30));
  expect(urls).toHaveLength(0);
});

// THE POINT OF THE HOIST. RightPanel (in-body ref pins) and the connections
// panel each used to call api.explain for the same fact, at the same anchor,
// with the same fallback — two identical requests per fact open, of which
// RightPanel discarded `incoming`. One hook, one request, both consumers.
it('issues exactly ONE explain request per fact open', async () => {
  const repoState: AppState = {
    ...init,
    repo: 'core', branch: 'agent/main', headCommit: 'abc1234',
    factPath: 'kb/a.md',
    asOf: { mode: 'live' },
  };
  const { rerender } = render(<Probe state={repoState} />);
  await waitFor(() => expect(urls.length).toBeGreaterThan(0));
  // A re-render with the same anchor must not re-fetch either.
  rerender(<Probe state={{ ...repoState }} />);
  await new Promise(r => setTimeout(r, 30));

  const explainCalls = urls.filter(u => /\/(incoming|outgoing)/.test(u));
  // api.explain issues one request per direction; what must not double is the
  // number of ROUNDS, which two independent callers produced.
  expect(explainCalls.length).toBe(new Set(explainCalls).size);
});
