import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { RightPanel } from './RightPanel';
import { init } from './state';
import type { AppState } from './state';

const { PARENT_COMMIT, REF_PATH } = vi.hoisted(() => ({
  PARENT_COMMIT: 'abc1234567890abc1234567890abc1234567890a',
  REF_PATH: 'kb/technology/ai/geopolitics/china/5f20c626.md',
}));

vi.mock('./api', () => ({
  api: {
    fact: vi.fn().mockResolvedValue({
      path: 'kb/technology/ai/geopolitics/parent.md',
      title: 'Parent Fact',
      body: 'Parent body',
      type: 'observation',
      confidence: 0.9,
      sources: 1,
      domain: [],
      entities: [],
      refs: [REF_PATH],
      commit_hash: PARENT_COMMIT,
      commit_date: '2026-05-01T00:00:00Z',
    }),
    factDiff: vi.fn().mockResolvedValue({ from: null, to: null }),
    stats: vi.fn().mockResolvedValue(null),
    activity: vi.fn().mockResolvedValue(null),
    commitDetail: vi.fn().mockResolvedValue(null),
    factCommits: vi.fn().mockResolvedValue({ entries: [] }),
  },
}));

describe('RightPanel — ref click opens Explain at parent commit', () => {
  it('clicking a local ref invokes onExplain(refPath, parent.commit_hash)', async () => {
    const onExplain = vi.fn();
    const state: AppState = {
      ...init,
      repo: 'knomit',
      branch: 'machine/test',
      factPath: 'kb/technology/ai/geopolitics/parent.md',
      asOf: { mode: 'live' },
    };
    render(<RightPanel state={state} dispatch={vi.fn()} onExplain={onExplain} />);

    const refLink = await waitFor(() => screen.getByText(new RegExp(REF_PATH.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'))));
    fireEvent.click(refLink);

    expect(onExplain).toHaveBeenCalledWith(REF_PATH, PARENT_COMMIT);
  });
});
