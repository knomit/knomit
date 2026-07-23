// Task 17 fix: the App→EdgesRail seam for a read-mount lens fact in LIVE mode.
// App feeds EdgesRail the fact's MOUNT repo/branch + relative path (factHistoryAnchor)
// and an edge anchor of "" (edgeAnchorCommit in a lens context) — so the edges
// fetch must hit the mount's live-HEAD explain URL, NOT a commit-anchored URL
// carrying the WRITE repo's head sha (which doesn't exist in the read mount →
// empty edges in the default view). Uses the REAL api.explain, spying on fetch to
// observe the exact URL — this pins the commit dimension the props-only coverage missed.
import { it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, waitFor } from '@testing-library/react';
import { EdgesRail } from './EdgesRail';
import { edgeAnchorCommit, factHistoryAnchor, init } from './state';
import type { AppState } from './state';
import type { Lens, LensSource } from './api';

const urls: string[] = [];

beforeEach(() => {
  urls.length = 0;
  vi.stubGlobal('fetch', vi.fn((url: unknown) => {
    urls.push(String(url));
    return Promise.resolve({ ok: true, json: () => Promise.resolve({ _embedded: { refs: [] } }) } as Response);
  }));
});
afterEach(() => vi.unstubAllGlobals());

const lens: Lens = { name: 'eng', write: 'core', reads: [{ repo: 'core' }, { repo: 'docs' }] };
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
  // Derive the props exactly as App does.
  const edge = factHistoryAnchor(liveReadMountState);
  const anchor = edgeAnchorCommit(liveReadMountState);
  expect(anchor).toBe(''); // guard: the App-side derivation dropped the write head

  render(<EdgesRail repo={edge.repo} branch={edge.branch} factPath={edge.path} anchorCommit={anchor} history={false} onHop={() => {}} />);
  await waitFor(() => expect(urls.length).toBeGreaterThan(0));

  // Mount repo + branch, non-commit-anchored (live HEAD).
  expect(urls.some(u => u.includes('/repos/docs/branches/main/facts/kb/api/auth.md/incoming'))).toBe(true);
  // Never a commit-anchored URL, and never the write repo's head sha.
  expect(urls.every(u => !u.includes('/commits/'))).toBe(true);
  expect(urls.every(u => !u.includes('coreHEADsha'))).toBe(true);
  expect(urls.every(u => !u.includes('/repos/core/'))).toBe(true);
});
