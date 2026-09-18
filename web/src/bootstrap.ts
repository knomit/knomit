// Bootstrap helper for App initialization.
//
// Getting to a known branch used to take two dependent requests: fetch the
// agent branch, then fetch that branch's root. The repo resource now EMBEDS
// its read branch's root (_embedded.branch), so the common path is ONE
// request — and the fallback, for an older server or a repo whose store is
// still opening, is the old two-step.
//
// Either call can fail transiently (Vite dev proxy first-request hiccup, brief
// network blip, backend just-restarted). Without retries, a single failure
// leaves the page stuck because the bootstrap effect only runs when
// `state.repo` changes — there's no other trigger to retry it. This helper
// retries with exponential backoff until it succeeds or `shouldStop` returns
// true (the parent effect re-fired or unmounted).

import type { RepoDetails, Status } from './api';

const DEFAULT_DELAYS_MS = [500, 1000, 2000, 4000, 8000, 10_000];

// BootPhase names what the boot is waiting on, so the UI can say it rather
// than showing an undifferentiated spinner.
//
// `branch` is CONDITIONAL: it happens only on the fallback path. When the
// server embedded the branch root there was no branch request to wait on, so
// the phase never occurs — which is why progress is a fraction per phase and
// not an index over a fixed total.
export type BootPhase = 'opening' | 'branch' | 'done';

export interface BootstrapDeps {
  repo: string;
  initialBranch: string;
  getRepo: (repo: string) => Promise<RepoDetails>;
  getStatus: (repo: string, branch: string) => Promise<Status>;
  onSuccess: (s: Status) => void;
  shouldStop: () => boolean;
  // Allow tests to inject a synchronous "sleep" stub. In production the helper
  // uses setTimeout via the default sleep implementation.
  sleep?: (ms: number) => Promise<void>;
  delaysMs?: number[];
  // Called once per failed attempt for observability/testing.
  onAttemptFailed?: (err: unknown, attempt: number) => void;
  // Called as each phase begins, so the boot screen can name it.
  onPhase?: (phase: BootPhase) => void;
}

// bootBranchOf picks the branch to ask about when the server did not embed
// one.
//
// read_branch, NOT agent_branch: it is the branch the repo's CONTENT comes
// from and it is always present, whereas a subscription has no agent branch at
// all and preferring it would leave us fetching branch "" — the same trap the
// server-side ReadBranch/AgentBranch split exists to avoid. See
// kb/conventions/repos/subscription/read-branch-for-content.
export function bootBranchOf(details: RepoDetails): string {
  return details.read_branch || details.agent_branch || '';
}

const realSleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

export async function bootstrapStatusWithRetry(deps: BootstrapDeps): Promise<void> {
  const delays = deps.delaysMs ?? DEFAULT_DELAYS_MS;
  const sleep = deps.sleep ?? realSleep;
  let attempt = 0;
  while (!deps.shouldStop()) {
    try {
      deps.onPhase?.('opening');
      const details = await deps.getRepo(deps.repo);
      if (deps.shouldStop()) return;

      // The one-hop path: the repo response already carried its branch root.
      if (details.branch) {
        deps.onPhase?.('done');
        deps.onSuccess(details.branch);
        return;
      }

      // Fallback: an older server, or a store still opening. One more hop.
      const branch = deps.initialBranch || bootBranchOf(details);
      deps.onPhase?.('branch');
      const s = await deps.getStatus(deps.repo, branch);
      if (deps.shouldStop()) return;
      deps.onPhase?.('done');
      deps.onSuccess(s);
      return;
    } catch (err) {
      deps.onAttemptFailed?.(err, attempt);
      const delay = delays[Math.min(attempt, delays.length - 1)];
      attempt += 1;
      await sleep(delay);
    }
  }
}
