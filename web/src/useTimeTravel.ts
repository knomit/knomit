import { api } from './api';
import type { Fact } from './api';
import type { AsOf } from './state';

export async function resolveHopAnchor(
  repo: string, branch: string, path: string, pinnedCommit: string,
  deps?: { fact?: typeof api.fact },
): Promise<{ asOf: AsOf; fact: Fact | null }> {
  const factFn = deps?.fact ?? api.fact;
  // HEAD read = present-check + the target's current last commit.
  try {
    const head = await factFn(repo, branch, path);            // no commit = HEAD endpoint
    if (head.commit_hash === pinnedCommit) {
      return { asOf: { mode: 'live' }, fact: head };           // target current
    }
    const pinned = await factFn(repo, branch, path, pinnedCommit, { fallback: 'before' });
    return { asOf: { mode: 'scrubbed', commit: pinnedCommit }, fact: pinned }; // superseded
  } catch {
    // HEAD 404 -> retracted; show the last-valid version at the pinned commit.
    const pinned = await factFn(repo, branch, path, pinnedCommit, { fallback: 'before' }).catch(() => null);
    return { asOf: { mode: 'scrubbed', commit: pinnedCommit }, fact: pinned };
  }
}
