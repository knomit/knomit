// Tests for in-body reference hops in the RightPanel fact view.
//
// Invariant (kb/principles/philosophy/historical-not-current): a ref resolves
// at the commit-point of the referrer — following it opens the target as it was
// WHEN it was referenced, not at the referrer's own current version. The
// authoritative "version-as-referenced" is the outgoing DERIVED_FROM edge's
// target_commit. Clicking a ref in the fact body must pin the hop to THAT
// commit (matching the EdgesRail "OUT" rows), NOT to fact.commit_hash — which
// 404s when the target was retracted before the referrer's displayed version.

import { it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { RightPanel } from './RightPanel';
import { init } from './state';
import type { AppState } from './state';

vi.mock('./api', () => ({
  api: {
    fact: vi.fn(),
    factDiff: vi.fn().mockResolvedValue({ from: null, to: null }),
    stats: vi.fn().mockResolvedValue(null),
    activity: vi.fn().mockResolvedValue(null),
    explain: vi.fn(),
    factCommits: vi.fn().mockResolvedValue({ entries: [] }),
  },
}));

import { api } from './api';

const REFERRER_COMMIT = '3c50ca0aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'; // referrer's own version
const EDGE_TARGET_COMMIT = 'f28d736bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'; // version-as-referenced
const REF_PATH = 'kb/technology/security/ai-agents/6cf51b30.md';

const liveState: AppState = {
  ...init,
  repo: 'core',
  branch: 'agent/mindev.local-8ef0cd32',
  view: 'library',
  factPath: 'kb/technology/security/ai-agents/synthesis/agent-access-is-blast-radius.md',
  asOf: { mode: 'live' },
};

beforeEach(() => {
  (api.fact as ReturnType<typeof vi.fn>).mockResolvedValue({
    path: liveState.factPath,
    title: 'CANONICAL: an AI agent’s access is the attacker’s blast radius',
    body: 'body',
    type: 'synthesis',
    confidence: 0.9,
    sources: 1,
    domain: [],
    entities: [],
    // Refs arrive pre-classified from the server, which also supplies the
    // repo-relative `path` a hop addresses.
    refs: [{ raw: REF_PATH, kind: 'fact' as const, path: REF_PATH }],
    commit_hash: REFERRER_COMMIT,
    commit_date: '2026-07-01T00:00:00Z',
  });
  // The referrer's outgoing edge pins the target to the version it reasoned
  // over — retracted now (deleted:true), but resolvable at EDGE_TARGET_COMMIT.
  (api.explain as ReturnType<typeof vi.fn>).mockResolvedValue({
    incoming: [],
    outgoing: [{
      path: REF_PATH,
      title: 'AI agents are compromised via over-broad permissions and injected instructions',
      type: 'synthesis',
      deleted: true,
      versions: [{ commit: EDGE_TARGET_COMMIT, committed_at: 1783570610, deleted: true }],
    }],
  });
});

it('in-body ref hop pins to the edge target_commit, not the referrer commit', async () => {
  const onHopRef = vi.fn();
  render(<RightPanel state={liveState} dispatch={vi.fn()} onHopRef={onHopRef} />);

  const link = await screen.findByText(
    (_, el) => el?.tagName === 'SPAN' && el.childElementCount === 0 && !!el.textContent && el.textContent.includes(REF_PATH),
  );
  fireEvent.click(link);

  await waitFor(() => expect(onHopRef).toHaveBeenCalled());
  expect(onHopRef).toHaveBeenCalledWith(REF_PATH, EDGE_TARGET_COMMIT);
});
