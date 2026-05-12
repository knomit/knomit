// Bootstrap helper for App initialization.
//
// The branch root endpoint requires knowing the branch. We bootstrap by
// fetching the agent branch, then calling the branch root for full status.
// Either call can fail transiently (Vite dev proxy first-request hiccup, brief
// network blip, backend just-restarted). Without retries, a single failure
// leaves the page stuck on "Loading…" because the bootstrap effect only runs
// when `state.repo` changes — there's no other trigger to retry it.
//
// This helper retries with exponential backoff until it succeeds or `shouldStop`
// returns true (the parent effect re-fired or unmounted).

import type { Status } from './api';

const DEFAULT_DELAYS_MS = [500, 1000, 2000, 4000, 8000, 10_000];

export interface BootstrapDeps {
  repo: string;
  initialBranch: string;
  getAgentBranch: (repo: string) => Promise<string>;
  getStatus: (repo: string, branch: string) => Promise<Status>;
  onSuccess: (s: Status) => void;
  shouldStop: () => boolean;
  // Allow tests to inject a synchronous "sleep" stub. In production the helper
  // uses setTimeout via the default sleep implementation.
  sleep?: (ms: number) => Promise<void>;
  delaysMs?: number[];
  // Called once per failed attempt for observability/testing.
  onAttemptFailed?: (err: unknown, attempt: number) => void;
}

const realSleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

export async function bootstrapStatusWithRetry(deps: BootstrapDeps): Promise<void> {
  const delays = deps.delaysMs ?? DEFAULT_DELAYS_MS;
  const sleep = deps.sleep ?? realSleep;
  let attempt = 0;
  while (!deps.shouldStop()) {
    try {
      const branch = deps.initialBranch || (await deps.getAgentBranch(deps.repo));
      if (deps.shouldStop()) return;
      const s = await deps.getStatus(deps.repo, branch);
      if (deps.shouldStop()) return;
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
