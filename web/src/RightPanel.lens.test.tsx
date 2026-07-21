// Tests for RightPanel in a LENS context (Task 16): the open fact is fetched
// through the lens (getLensFact, RAW canonical path), the meta line prefixes a
// source-repo badge + its branch, and edit/retract controls are enabled ONLY
// for facts that live in the lens's WRITE repo — read-mount facts render
// read-only. Writes route through the repo-scoped api targeting the write repo
// and its agent branch. A failed fetch surfaces the error and never leaves a
// stale factSource paired with the new factPath (the m30 regression).

import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { RightPanel } from './RightPanel';
import { init, READ_ONLY_TITLE } from './state';
import type { AppState, AsOf } from './state';
import type { Lens, LensSource } from './api';

vi.mock('./api', () => ({
  api: {
    fact: vi.fn(),               // must NOT be called in lens context
    getLensFact: vi.fn(),
    getAgentBranch: vi.fn().mockResolvedValue('agent/main'),
    updateFact: vi.fn().mockResolvedValue({ path: 'kb/ops/rollback.md', title: 'x', body: 'x', domain: [], entities: [], refs: [], confidence: 1, sources: 1 }),
    retractFact: vi.fn().mockResolvedValue(undefined),
    factDiff: vi.fn().mockResolvedValue({ from: null, to: null }),
    stats: vi.fn().mockResolvedValue(null),
    activity: vi.fn().mockResolvedValue(null),
    commitDetail: vi.fn().mockResolvedValue(null),
    factCommits: vi.fn().mockResolvedValue({ entries: [] }),
  },
}));

import { api } from './api';

const lens: Lens = {
  name: 'eng',
  write: 'core',
  reads: [{ repo: 'core' }, { repo: 'docs' }],
};

const WRITE_PATH = 'kb/ops/rollback.md';
const READ_PATH = 'kb://docsid123456/kb/api/auth.md';

const writeSource: LensSource = { repo: 'core', id: 'coreid123456', branch: 'agent/main' };
const readSource: LensSource = { repo: 'docs', id: 'docsid123456', branch: 'main' };

function writeFact(extra: Record<string, unknown> = {}) {
  return {
    path: WRITE_PATH, title: 'Rollback runbook', body: 'body', type: 'process',
    confidence: 0.9, sources: 1, domain: [], entities: [], refs: [],
    commit_hash: 'aaa1111', commit_date: '2026-05-01T00:00:00Z',
    source: writeSource, ...extra,
  };
}

function readFact(extra: Record<string, unknown> = {}) {
  return {
    path: READ_PATH, title: 'Auth flow', body: 'body', type: 'concept',
    confidence: 0.9, sources: 1, domain: [], entities: [], refs: [],
    commit_hash: 'bbb2222', commit_date: '2026-05-01T00:00:00Z',
    source: readSource, ...extra,
  };
}

function lensState(factPath: string, factSource: LensSource | null, asOf: AsOf = { mode: 'live' }, overrides: Partial<AppState> = {}): AppState {
  return {
    ...init,
    repo: 'core',
    branch: 'agent/main',
    headCommit: 'aaaaaaa',
    context: { kind: 'lens', name: 'eng' },
    lens,
    factPath,
    factSource,
    asOf,
    ...overrides,
  };
}

describe('RightPanel — lens fact view', () => {
  beforeEach(() => { vi.clearAllMocks(); });

  it('fetches the open fact through the lens with the RAW canonical path (never api.fact)', async () => {
    (api.getLensFact as ReturnType<typeof vi.fn>).mockResolvedValue(readFact());
    render(<RightPanel state={lensState(READ_PATH, readSource)} dispatch={vi.fn()} />);
    await screen.findByTestId('fact-title');
    expect(api.getLensFact).toHaveBeenCalledWith('eng', READ_PATH);
    expect(api.fact).not.toHaveBeenCalled();
  });

  it('renders the source badge + branch chip on the meta line and strips the kb://<id12>/ qualifier', async () => {
    (api.getLensFact as ReturnType<typeof vi.fn>).mockResolvedValue(readFact());
    const { container } = render(<RightPanel state={lensState(READ_PATH, readSource)} dispatch={vi.fn()} />);
    await screen.findByTestId('fact-title');
    const badge = screen.getByTestId('source-badge');
    expect(badge.getAttribute('data-repo')).toBe('docs');
    expect(badge.textContent).toContain('docs');
    // Branch chip carries the mount's read branch.
    expect(container.textContent).toContain('main');
    // Displayed path is stripped of the opaque mount id.
    expect(container.textContent).toContain('kb/api/auth.md');
    expect(container.textContent).not.toContain('docsid123456');
    expect(container.textContent).not.toContain('kb://');
  });

  it('renders a read-mount fact read-only: retract disabled with a lens-specific title', async () => {
    (api.getLensFact as ReturnType<typeof vi.fn>).mockResolvedValue(readFact());
    render(<RightPanel state={lensState(READ_PATH, readSource)} dispatch={vi.fn()} />);
    const btn = await screen.findByTestId('retract-btn');
    expect(btn).toBeDisabled();
    expect(btn.getAttribute('title')).toMatch(/read-only mount|edits go to core/i);
  });

  it('enables retract for a write-repo fact when live', async () => {
    (api.getLensFact as ReturnType<typeof vi.fn>).mockResolvedValue(writeFact());
    render(<RightPanel state={lensState(WRITE_PATH, writeSource)} dispatch={vi.fn()} />);
    const btn = await screen.findByTestId('retract-btn');
    expect(btn).not.toBeDisabled();
    expect(btn.getAttribute('title')).toBe('Retract fact');
    expect(screen.getByTestId('source-badge').getAttribute('data-repo')).toBe('core');
  });

  it('renders a write-repo fact read-only when off-live (history) — existing invariant', async () => {
    // Off-live in a lens context now reads the anchored version through the
    // mount's repo-scoped commit endpoint (C1), not getLensFact — factSource is
    // already set, so no re-dispatch is needed.
    (api.fact as ReturnType<typeof vi.fn>).mockResolvedValue(writeFact());
    render(<RightPanel state={lensState(WRITE_PATH, writeSource, { mode: 'history', commit: 'b812d40' })} dispatch={vi.fn()} />);
    const btn = await screen.findByTestId('retract-btn');
    expect(btn).toBeDisabled();
    expect(btn.getAttribute('title')).toBe(READ_ONLY_TITLE);
  });

  it('routes an edit of a write-repo fact to the repo endpoint with the write repo + agent branch', async () => {
    (api.getLensFact as ReturnType<typeof vi.fn>).mockResolvedValue(writeFact({ parse_error: 'bad frontmatter' }));
    render(<RightPanel state={lensState(WRITE_PATH, writeSource)} dispatch={vi.fn()} />);
    const editor = await screen.findByTestId('fact-editor');
    fireEvent.change(editor, { target: { value: 'edited content' } });
    fireEvent.click(screen.getByTestId('fact-save-btn'));
    await waitFor(() => expect(api.updateFact).toHaveBeenCalledWith('core', 'agent/main', WRITE_PATH, 'edited content'));
  });

  it('routes a retract of a write-repo fact to the repo endpoint with the write repo + agent branch', async () => {
    (api.getLensFact as ReturnType<typeof vi.fn>).mockResolvedValue(writeFact());
    render(<RightPanel state={lensState(WRITE_PATH, writeSource)} dispatch={vi.fn()} />);
    const btn = await screen.findByTestId('retract-btn');
    fireEvent.click(btn);
    fireEvent.click(await screen.findByTestId('retract-confirm-btn'));
    await waitFor(() => expect(api.retractFact).toHaveBeenCalledWith('core', 'agent/main', WRITE_PATH));
  });

  it('routes writes to the write repo AGENT branch, not the write mount read branch, when they diverge', async () => {
    // The lens pins its write repo's READ mount to 'main' (a non-agent branch):
    // factSource.branch === 'main'. state.branch is also 'main' here. Writes must
    // still land on the write repo's AGENT branch, resolved via getAgentBranch —
    // the only WritableBranch — never on the pinned read-mount branch.
    const pinnedSource: LensSource = { repo: 'core', id: 'coreid123456', branch: 'main' };
    (api.getLensFact as ReturnType<typeof vi.fn>).mockResolvedValue(writeFact({ source: pinnedSource }));
    (api.getAgentBranch as ReturnType<typeof vi.fn>).mockResolvedValue('agent/main');
    render(<RightPanel state={lensState(WRITE_PATH, pinnedSource, { mode: 'live' }, { branch: 'main' })} dispatch={vi.fn()} />);
    const btn = await screen.findByTestId('retract-btn');
    fireEvent.click(btn);
    fireEvent.click(await screen.findByTestId('retract-confirm-btn'));
    await waitFor(() => expect(api.retractFact).toHaveBeenCalledWith('core', 'agent/main', WRITE_PATH));
    expect(api.getAgentBranch).toHaveBeenCalledWith('core');
    // Never routed to the pinned read-mount branch.
    expect(api.retractFact).not.toHaveBeenCalledWith('core', 'main', WRITE_PATH);
  });

  it('routes an EDIT to the write repo AGENT branch when the write mount read branch diverges', async () => {
    const pinnedSource: LensSource = { repo: 'core', id: 'coreid123456', branch: 'main' };
    (api.getLensFact as ReturnType<typeof vi.fn>).mockResolvedValue(writeFact({ source: pinnedSource, parse_error: 'bad frontmatter' }));
    (api.getAgentBranch as ReturnType<typeof vi.fn>).mockResolvedValue('agent/main');
    render(<RightPanel state={lensState(WRITE_PATH, pinnedSource, { mode: 'live' }, { branch: 'main' })} dispatch={vi.fn()} />);
    const editor = await screen.findByTestId('fact-editor');
    fireEvent.change(editor, { target: { value: 'edited content' } });
    fireEvent.click(screen.getByTestId('fact-save-btn'));
    await waitFor(() => expect(api.updateFact).toHaveBeenCalledWith('core', 'agent/main', WRITE_PATH, 'edited content'));
    expect(api.updateFact).not.toHaveBeenCalledWith('core', 'main', WRITE_PATH, 'edited content');
  });

  it('surfaces a failed lens fetch and clears factSource so no stale source pairs with the new fact (m30)', async () => {
    (api.getLensFact as ReturnType<typeof vi.fn>).mockRejectedValue(new Error('boom 404'));
    const dispatch = vi.fn();
    render(<RightPanel state={lensState(READ_PATH, readSource)} dispatch={dispatch} />);
    await waitFor(() => expect(screen.getByText(/boom 404/)).toBeTruthy());
    expect(dispatch).toHaveBeenCalledWith({ type: 'SET_FACT_SOURCE', source: null });
  });

  it('re-dispatches SET_FACT_SOURCE from its own fetch response (coherent with the row-click open)', async () => {
    (api.getLensFact as ReturnType<typeof vi.fn>).mockResolvedValue(readFact());
    const dispatch = vi.fn();
    render(<RightPanel state={lensState(READ_PATH, readSource)} dispatch={dispatch} />);
    await screen.findByTestId('fact-title');
    await waitFor(() => expect(dispatch).toHaveBeenCalledWith({ type: 'SET_FACT_SOURCE', source: readSource }));
  });

  // Task 17 (fixes m36): VersionWalker is a HISTORY surface, not a write surface.
  // It must read the open fact's versions through the fact's SOURCE MOUNT
  // (openFactSource) with the RELATIVE path — not the write target with the raw
  // kb:// address, which the mount's repo-scoped /commits endpoint can't resolve.
  it('reads a read-mount fact history against the MOUNT repo/branch + relative path (m36)', async () => {
    (api.getLensFact as ReturnType<typeof vi.fn>).mockResolvedValue(readFact());
    render(<RightPanel state={lensState(READ_PATH, readSource)} dispatch={vi.fn()} />);
    await screen.findByTestId('fact-title');
    await waitFor(() => expect(api.factCommits).toHaveBeenCalledWith('docs', 'main', 'kb/api/auth.md'));
    // Never the write target paired with the raw kb:// path (the m36 no-op).
    expect(api.factCommits).not.toHaveBeenCalledWith('core', 'agent/main', READ_PATH);
  });

  it('reads a write-repo fact history against the write mount + bare path', async () => {
    (api.getLensFact as ReturnType<typeof vi.fn>).mockResolvedValue(writeFact());
    render(<RightPanel state={lensState(WRITE_PATH, writeSource)} dispatch={vi.fn()} />);
    await screen.findByTestId('fact-title');
    await waitFor(() => expect(api.factCommits).toHaveBeenCalledWith('core', 'agent/main', WRITE_PATH));
  });

  // C1: an anchored lens read (scrub/diff entered from an open fact) must fetch
  // the VERSION at the anchor through the mount's repo-scoped commit endpoint —
  // getLensFact ignores the anchor and always returns live, which showed the live
  // body under an off-live UI and mis-fired the retracted badge.
  it('reads an anchored (scrubbed) read-mount fact via the mount commit endpoint, not getLensFact (C1)', async () => {
    (api.fact as ReturnType<typeof vi.fn>).mockResolvedValue(readFact({ commit_hash: 'ccc3333' }));
    const dispatch = vi.fn();
    render(<RightPanel state={lensState(READ_PATH, readSource, { mode: 'history', commit: 'ccc3333' })} dispatch={dispatch} />);
    await screen.findByTestId('fact-title');
    // History mode opts into ?fallback=before, anchored on the mount + relative path.
    expect(api.fact).toHaveBeenCalledWith('docs', 'main', 'kb/api/auth.md', 'ccc3333', { fallback: 'before' });
    expect(api.getLensFact).not.toHaveBeenCalled();
    // factSource is already set (same fact/mount) — the anchored path never re-dispatches it.
    expect(dispatch).not.toHaveBeenCalledWith(expect.objectContaining({ type: 'SET_FACT_SOURCE' }));
  });

  it('shows no retracted badge when scrubbing to a live version of a lens fact (C1 regression)', async () => {
    // The anchored read returns the version AT the scrub commit, so commit_hash
    // matches the anchor → no spurious "retracted at" badge (the old always-live
    // getLensFact read returned a mismatched commit_hash and mis-fired it).
    (api.fact as ReturnType<typeof vi.fn>).mockResolvedValue(readFact({ commit_hash: 'ccc3333' }));
    render(<RightPanel state={lensState(READ_PATH, readSource, { mode: 'history', commit: 'ccc3333' })} dispatch={vi.fn()} />);
    await screen.findByTestId('fact-title');
    await waitFor(() => expect(screen.queryByTestId('retracted-version-badge')).toBeNull());
  });
});
