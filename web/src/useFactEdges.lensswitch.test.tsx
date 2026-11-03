// Switching the open fact between lens mounts. factSource is re-set only once
// getLensFact answers for the NEW path, and useFactEdges runs synchronously on
// the new path, while factSource still names the PREVIOUS fact's mount. Before
// the fix the hook's guard was a bare null check, so opening a read-member
// (beta) fact after a write-member (alpha) one asked alpha's route for beta's
// relative path: two console 404s on /incoming and /outgoing (and the reverse
// for beta -> alpha). The hook now waits until the source owns the open path
// (openFactSourceResolved).
//
// Driven through the REAL reducer and the REAL api.explain, spying on fetch.
import { it, expect, vi, beforeEach, afterEach } from 'vitest';
import { act, render, waitFor, screen } from '@testing-library/react';
import { useReducer } from 'react';
import type { Dispatch } from 'react';
import { useFactEdges } from './useFactEdges';
import { init, reducer } from './state';
import type { Action, AppState } from './state';
import type { Lens, LensSource } from './api';

const lens: Lens = {
  name: 'all',
  write: { uid: 'uid-alpha', name: 'alpha' },
  reads: [{ uid: 'uid-alpha', name: 'alpha' }, { uid: 'uid-beta', name: 'beta' }],
};
const alpha: LensSource = { repo: 'alpha', id: 'alphaid12345', branch: 'agent/a' };
const beta: LensSource = { repo: 'beta', id: 'betaid123456', branch: 'agent/b' };

const ALPHA_ROOT = 'kb/chain/alpha-root.md';
const BETA_MID = 'kb://betaid123456/kb/chain/beta-mid.md';
const BETA_LEAF = 'kb://betaid123456/kb/chain/beta-leaf.md';

const urls: string[] = [];

// Each mount answers only for its OWN facts, as the server does: anything else
// is the 404 the bug produced. beta-mid builds on beta-root and is cited by
// beta-leaf, so its edges are visibly non-empty when read on the right route.
function respond(url: string): Response {
  const ok = (refs: unknown[]) => ({ ok: true, status: 200, json: () => Promise.resolve({ _embedded: { refs } }) }) as Response;
  const notFound = { ok: false, status: 404, statusText: 'Not Found', json: () => Promise.resolve({ title: 'Not Found' }) } as Response;
  if (url.includes('/repos/beta/branches/agent:b/facts/kb/chain/beta-mid.md/outgoing')) return ok([{ path: 'kb/chain/beta-root.md', title: 'beta root fact', commit: 'b1' }]);
  if (url.includes('/repos/beta/branches/agent:b/facts/kb/chain/beta-mid.md/incoming')) return ok([{ path: 'kb/chain/beta-leaf.md', title: 'beta leaf fact', commit: 'b3' }]);
  if (url.includes('/repos/beta/branches/agent:b/facts/kb/chain/beta-leaf.md/')) return ok([]);
  if (url.includes('/repos/alpha/branches/agent:a/facts/kb/chain/alpha-root.md/incoming')) return ok([{ path: 'kb/chain/alpha-mid.md', title: 'alpha mid fact', commit: 'a2' }]);
  if (url.includes('/repos/alpha/branches/agent:a/facts/kb/chain/alpha-root.md/outgoing')) return ok([]);
  return notFound;
}

beforeEach(() => {
  urls.length = 0;
  vi.stubGlobal('fetch', vi.fn((url: unknown) => {
    urls.push(String(url));
    return Promise.resolve(respond(String(url)));
  }));
});
afterEach(() => vi.unstubAllGlobals());

let dispatch: Dispatch<Action> = () => {};

function Harness({ initial }: { initial: AppState }) {
  const [state, d] = useReducer(reducer, initial);
  dispatch = d;
  const e = useFactEdges(state);
  return (
    <div>
      <span data-testid="out">{e.outgoing.map(g => g.title).join(',')}</span>
      <span data-testid="in">{e.incoming.map(g => g.title).join(',')}</span>
      <span data-testid="err">{e.error ?? ''}</span>
    </div>
  );
}

function lensState(factPath: string, factSource: LensSource): AppState {
  return {
    ...init,
    context: { kind: 'lens', name: 'all' },
    lens,
    repo: 'alpha', branch: 'agent/a', headCommit: 'alphaHEAD',
    factPath, factSource,
    asOf: { mode: 'live' },
  };
}

const open = (factPath: string) => act(() => { dispatch({ type: 'APPLY_NAV', view: 'library', factPath, asOf: { mode: 'live' } }); });
const resolve = (source: LensSource) => act(() => { dispatch({ type: 'SET_FACT_SOURCE', source }); });
const settle = () => act(() => new Promise(r => setTimeout(r, 30)));

it('a read-member fact opened after a write-member fact fetches its edges on ITS OWN route, and shows them', async () => {
  render(<Harness initial={lensState(ALPHA_ROOT, alpha)} />);
  await waitFor(() => expect(screen.getByTestId('in').textContent).toBe('alpha mid fact'));
  urls.length = 0;

  open(BETA_MID);
  await settle();
  // Nothing for beta-mid may go to the write repo's route.
  expect(urls.filter(u => u.includes('/repos/alpha/'))).toEqual([]);

  resolve(beta);
  await waitFor(() => expect(screen.getByTestId('out').textContent).toBe('beta root fact'));
  expect(screen.getByTestId('in').textContent).toBe('beta leaf fact');
  expect(screen.getByTestId('err').textContent).toBe('');
  expect(urls).toEqual(expect.arrayContaining([
    expect.stringContaining('/repos/beta/branches/agent:b/facts/kb/chain/beta-mid.md/incoming'),
    expect.stringContaining('/repos/beta/branches/agent:b/facts/kb/chain/beta-mid.md/outgoing'),
  ]));
  expect(urls.filter(u => u.includes('/repos/alpha/'))).toEqual([]);
});

it('a write-member fact opened after a read-member fact is unchanged: its edges on the write route, never the read mount', async () => {
  render(<Harness initial={lensState(BETA_MID, beta)} />);
  await waitFor(() => expect(screen.getByTestId('out').textContent).toBe('beta root fact'));
  urls.length = 0;

  open(ALPHA_ROOT);
  await settle();
  expect(urls.filter(u => u.includes('/repos/beta/'))).toEqual([]);

  resolve(alpha);
  await waitFor(() => expect(screen.getByTestId('in').textContent).toBe('alpha mid fact'));
  expect(screen.getByTestId('err').textContent).toBe('');
  expect(urls).toEqual(expect.arrayContaining([
    expect.stringContaining('/repos/alpha/branches/agent:a/facts/kb/chain/alpha-root.md/incoming'),
    expect.stringContaining('/repos/alpha/branches/agent:a/facts/kb/chain/alpha-root.md/outgoing'),
  ]));
  expect(urls.filter(u => u.includes('/repos/beta/'))).toEqual([]);
});

it('a hop within one read mount keeps its source and fetches at once, without waiting for a re-resolve', async () => {
  render(<Harness initial={lensState(BETA_MID, beta)} />);
  await waitFor(() => expect(screen.getByTestId('out').textContent).toBe('beta root fact'));
  urls.length = 0;

  open(BETA_LEAF);
  await waitFor(() => expect(urls).toEqual(expect.arrayContaining([
    expect.stringContaining('/repos/beta/branches/agent:b/facts/kb/chain/beta-leaf.md/incoming'),
  ])));
  expect(urls.filter(u => u.includes('/repos/alpha/'))).toEqual([]);
});
