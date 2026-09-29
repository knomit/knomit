import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { BranchMovedError, PushedMergeConflictError } from './api';
import type { PushedBranchRow } from './api';
import { PushedBranchesPanel } from './PushedBranchesPanel';

const listPushedBranches = vi.fn();
const branchChanges = vi.fn();
const mergePushedBranch = vi.fn();
const fact = vi.fn();

vi.mock('./api', async importOriginal => {
  const actual = await importOriginal<typeof import('./api')>();
  return {
    ...actual,
    api: {
      listPushedBranches: (...a: unknown[]) => listPushedBranches(...a),
      branchChanges: (...a: unknown[]) => branchChanges(...a),
      mergePushedBranch: (...a: unknown[]) => mergePushedBranch(...a),
      fact: (...a: unknown[]) => fact(...a),
    },
  };
});

const PEER = 'agent/peer-99887766';
const AGENT = 'agent/host-0a1b2c3d';
// The list's tip and the changes head DIFFER on purpose: the merge must send
// the head the dialog listed, not the tip the table was read at.
const LIST_TIP = 'a'.repeat(40);
const HEAD = 'b'.repeat(40);

function row(over: Partial<PushedBranchRow> = {}): PushedBranchRow {
  return {
    name: PEER, tip: LIST_TIP, tip_time: new Date(Date.now() - 3600_000).toISOString(),
    tip_author: 'peer-agent', to_merge: 2, in_agent_branch: false,
    merge_base: 'c'.repeat(40), other_files_changed: 0, ...over,
  };
}

function mount(onEnterBranch = vi.fn()) {
  render(<PushedBranchesPanel repo="alpha" agentBranch={AGENT} onEnterBranch={onEnterBranch} />);
  return onEnterBranch;
}

async function openConfirm() {
  fireEvent.click(await screen.findByTestId(`pushed-merge-${PEER}`));
  await screen.findByTestId('pushed-merge-counts');
}

beforeEach(() => {
  listPushedBranches.mockReset().mockResolvedValue([row()]);
  branchChanges.mockReset().mockResolvedValue({
    head: HEAD, hasMore: false,
    changes: [{ path: 'kb/a.md', change: 'added' }, { path: 'kb/b.md', change: 'modified' }],
  });
  mergePushedBranch.mockReset().mockResolvedValue({ mode: 'merge', new_tip: 'd'.repeat(40), into: AGENT });
  fact.mockReset().mockImplementation((_r: string, branch: string) =>
    Promise.resolve({ body: branch === AGENT ? 'HOST BODY' : 'PEER BODY' }));
});

describe('PushedBranchesPanel', () => {
  it('lists each pushed branch with who, when, and how many commits it brings', async () => {
    mount();
    const r = await screen.findByTestId(`pushed-row-${PEER}`);
    expect(r.textContent).toContain(PEER);
    expect(r.textContent).toContain('peer-agent');
    expect(screen.getByTestId(`pushed-tomerge-${PEER}`).textContent).toBe('2 commits');
    expect((screen.getByTestId(`pushed-merge-${PEER}`) as HTMLButtonElement).disabled).toBe(false);
  });

  it('shows a merged branch as merged, with Merge disabled', async () => {
    listPushedBranches.mockResolvedValue([row({ to_merge: 0, in_agent_branch: true })]);
    mount();
    expect((await screen.findByTestId(`pushed-tomerge-${PEER}`)).textContent).toBe('merged');
    expect((screen.getByTestId(`pushed-merge-${PEER}`) as HTMLButtonElement).disabled).toBe(true);
  });

  it('says so when nothing was pushed', async () => {
    listPushedBranches.mockResolvedValue([]);
    mount();
    await screen.findByTestId('pushed-branches-empty');
  });

  it('Browse opens the pushed branch', async () => {
    const onEnter = mount();
    fireEvent.click(await screen.findByTestId(`pushed-browse-${PEER}`));
    expect(onEnter).toHaveBeenCalledWith(PEER);
  });

  it('the confirm dialog lists changes since the merge base and merges EXACTLY the head it listed', async () => {
    mount();
    await openConfirm();
    expect(branchChanges).toHaveBeenCalledWith('alpha', PEER, 'c'.repeat(40));
    expect(screen.getByTestId('pushed-merge-counts').textContent).toBe('1 added · 1 modified · 0 deleted');
    expect(screen.getByTestId('pushed-merge-paths').textContent).toContain('kb/a.md');
    expect(screen.getByTestId('pushed-merge-other-files').textContent).toBe('Only facts are listed.');

    fireEvent.click(screen.getByTestId('pushed-merge-submit'));
    await waitFor(() => expect(mergePushedBranch).toHaveBeenCalled());
    expect(mergePushedBranch).toHaveBeenCalledWith('alpha', PEER, HEAD, undefined);
    expect((await screen.findByTestId('pushed-branches-notice')).textContent)
      .toContain(`Merged ${PEER} into ${AGENT} (merge commit ddddddd)`);
    expect(listPushedBranches).toHaveBeenCalledTimes(2);
  });

  it('counts files outside the knowledge base', async () => {
    listPushedBranches.mockResolvedValue([row({ other_files_changed: 3 })]);
    mount();
    await openConfirm();
    expect(screen.getByTestId('pushed-merge-other-files').textContent)
      .toBe('It also changes 3 other files outside the knowledge base.');
  });

  it('D4a: nothing in the panel or the dialog talks about re-cloning or lineage', async () => {
    mount();
    await openConfirm();
    const text = document.body.textContent ?? '';
    expect(text).not.toMatch(/re-?clon|lineage|main on origin|verify accept/i);
  });

  it('a refused merge shows both versions of each fact at the refusal commits, then resolves the WHOLE set', async () => {
    mergePushedBranch.mockRejectedValueOnce(
      new PushedMergeConflictError('conflict', ['kb/a.md'], 'e'.repeat(40), 'f'.repeat(40)));
    mount();
    await openConfirm();
    fireEvent.click(screen.getByTestId('pushed-merge-submit'));
    await screen.findByTestId('pushed-merge-conflict');

    fireEvent.click(screen.getByTestId('pushed-merge-versions-kb/a.md'));
    await screen.findByText('HOST BODY');
    await screen.findByText('PEER BODY');
    expect(fact).toHaveBeenCalledWith('alpha', AGENT, 'kb/a.md', 'e'.repeat(40));
    expect(fact).toHaveBeenCalledWith('alpha', PEER, 'kb/a.md', 'f'.repeat(40));

    fireEvent.click(screen.getByTestId('pushed-merge-keep-host'));
    await waitFor(() => expect(mergePushedBranch).toHaveBeenLastCalledWith('alpha', PEER, HEAD, 'host'));
  });

  it('"Take the peer\'s version" sends peer, with the same reviewed head', async () => {
    mergePushedBranch.mockRejectedValueOnce(
      new PushedMergeConflictError('conflict', ['kb/a.md'], 'e'.repeat(40), 'f'.repeat(40)));
    mount();
    await openConfirm();
    fireEvent.click(screen.getByTestId('pushed-merge-submit'));
    fireEvent.click(await screen.findByTestId('pushed-merge-take-peer'));
    await waitFor(() => expect(mergePushedBranch).toHaveBeenLastCalledWith('alpha', PEER, HEAD, 'peer'));
  });

  it('a branch that moved while the dialog was open is refused and re-read, not treated as a conflict', async () => {
    mergePushedBranch.mockRejectedValueOnce(new BranchMovedError('moved'));
    mount();
    await openConfirm();
    fireEvent.click(screen.getByTestId('pushed-merge-submit'));
    expect((await screen.findByTestId('pushed-branches-notice')).textContent)
      .toBe(`${PEER} moved since you opened it. Review it again.`);
    expect(screen.queryByTestId('pushed-merge-conflict')).toBeNull();
    expect(listPushedBranches).toHaveBeenCalledTimes(2);
  });
});
