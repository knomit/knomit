import { useEffect, useState } from 'react';
import { api, isTerminalCreateState } from './api';
import type { RepoCreateStatus } from './api';

// The two poll rates. ACTIVE while anything is happening, IDLE otherwise.
//
// Both are RESPONSIVENESS choices and neither is load-bearing: a create
// finishes when it finishes whether or not anyone is asking, and this list is
// a view of work the server owns. The active rate is the one a human reads as
// "live" for a bar that moves; the idle rate exists so a screen nobody is
// creating on costs one request every half minute rather than one every two
// seconds, forever.
//
// A NON-EMPTY list polls at the active rate even when every job in it has
// finished, because a finished row is one a user is about to dismiss and the
// list should notice when another client does it first.
export const CREATES_POLL_ACTIVE_MS = 2000;
export const CREATES_POLL_IDLE_MS = 30000;

// One poller for the whole app, not one per component.
//
// Three surfaces want this list at once — the top-bar indicator, the rail and
// the Manage overview — and a hook that polled per mount would triple the
// request rate for one answer, and would let the three of them disagree about
// it mid-flight. The module-level store is what makes "how many creates are
// running" a single fact rather than three approximations.
//
// It is deliberately not a context: these three surfaces are not siblings
// under one provider, and threading one through App to reach the top bar and
// the rail would be a provider whose only purpose is to be found.
let cache: RepoCreateStatus[] = [];
let timer: ReturnType<typeof setTimeout> | null = null;
let inFlight = false;
const subscribers = new Set<(s: RepoCreateStatus[]) => void>();

// Both NON-TERMINAL states count as activity. A cancelling job is still
// working — a step to finish, or a repo to delete — so a list that fell back
// to the idle poll rate the moment cancel was pressed would take up to half a
// minute to notice the outcome the user is waiting for.
function anyRunning(list: RepoCreateStatus[]): boolean {
  return list.some(c => !isTerminalCreateState(c.state));
}

function nextDelay(list: RepoCreateStatus[]): number {
  return anyRunning(list) || list.length > 0 ? CREATES_POLL_ACTIVE_MS : CREATES_POLL_IDLE_MS;
}

function publish(list: RepoCreateStatus[]) {
  cache = list;
  for (const fn of subscribers) fn(list);
}

async function poll() {
  if (inFlight) return;
  inFlight = true;
  try {
    publish(await api.listRepoCreates());
  } catch {
    // A failed poll is NOT an empty list. Publishing [] here would make every
    // pending row vanish on one dropped request and reappear on the next,
    // which reads to a user as a create that disappeared — the exact failure
    // this list exists to prevent. Keep the last known answer and retry.
  } finally {
    inFlight = false;
  }
  schedule();
}

function schedule() {
  if (timer) clearTimeout(timer);
  if (subscribers.size === 0) { timer = null; return; }
  timer = setTimeout(poll, nextDelay(cache));
}

// refreshRepoCreates polls once, now, outside the timer.
//
// For the moments where waiting up to two seconds would look broken: right
// after starting a create, and right after dismissing one.
export function refreshRepoCreates(): Promise<void> {
  if (timer) { clearTimeout(timer); timer = null; }
  return poll();
}

// __resetRepoCreatesForTest clears the module store between tests. Exported
// because the store is module-level BY DESIGN (see above) and would otherwise
// leak one test's jobs into the next.
export function __resetRepoCreatesForTest() {
  if (timer) clearTimeout(timer);
  timer = null;
  inFlight = false;
  cache = [];
  subscribers.clear();
}

// useRepoCreates subscribes to the shared list. Every caller sees the same
// array instance, and the poller runs only while at least one is mounted.
export function useRepoCreates(): RepoCreateStatus[] {
  const [list, setList] = useState<RepoCreateStatus[]>(cache);
  useEffect(() => {
    subscribers.add(setList);
    // Seed from the cache so a late-mounting surface does not show an empty
    // list for one poll interval while the others already know better.
    setList(cache);
    if (subscribers.size === 1) void poll();
    return () => {
      subscribers.delete(setList);
      if (subscribers.size === 0 && timer) { clearTimeout(timer); timer = null; }
    };
  }, []);
  return list;
}

// pendingCreates drops jobs whose repo is ALREADY in the list.
//
// A finished job outlives its create by design — a client that lost the id
// from the 202 must still be able to find the outcome — so a repo list cannot
// wait for the server to forget one. Two rows for the same name would read as
// two repositories, and the row that is a real repo is strictly the better of
// the two: it can be opened.
//
// Applied by every repo-list surface, from here rather than per surface, so
// the rail and the overview cannot disagree about which rows exist.
export function pendingCreates(list: RepoCreateStatus[], repoNames: readonly string[]): RepoCreateStatus[] {
  // CANCELLED JOBS ARE DROPPED HERE TOO, not only by the server.
  //
  // The server omits them from the collection, so this is belt and braces —
  // but it is the brace that matters: the row component returns null for a
  // cancelled job, so a surface that counted the list rather than the rows it
  // would draw showed an empty "Being created" block with a heading and
  // nothing under it. Filtering where both surfaces already filter keeps the
  // count and the rows the same fact.
  return list.filter(c => c.state !== 'cancelled' && !repoNames.includes(c.name));
}

// runningCreates is the count the top-bar indicator shows — every create with
// work still to do, which includes one that is cancelling. The light answers
// "is anything happening?", and honouring a cancel is something happening.
export function runningCreates(list: RepoCreateStatus[]): number {
  return list.filter(c => !isTerminalCreateState(c.state)).length;
}

// createFlag is the ONE word a list row says about a create.
//
// A create in a repository list is a repository that is not finished yet, so
// the row says which repository it is and flags the state it is in — nothing
// more. Everything else about the job (the step, the percent, the error, the
// controls) belongs on the create's own page, exactly as a repository's own
// details belong on its page rather than in the rail.
//
// `null` means DO NOT RENDER THIS ROW AT ALL. A cancelled create has nothing
// left to say in a list: the repository is gone and the outcome is the one the
// user asked for. The server already omits cancelled jobs from the collection;
// this is the same rule stated where the drawing happens, so a stale list
// cannot put the row back.
//
// It lives HERE, beside pendingCreates and runningCreates, rather than in
// PendingCreateRow.tsx: that file exports components, and a non-component
// export from it breaks fast refresh for every component in it.
export function createFlag(state: string): string | null {
  switch (state) {
    case 'running': return 'creating';
    case 'cancelling': return 'cancelling';
    case 'failed': return 'failed';
    case 'cancelled': return null;
    // 'done' is the brief window between the job finishing and its repo
    // appearing in the list that replaces this row. 'created' rather than
    // 'creating', because it is neither a lie nor a state anyone acts on.
    default: return 'created';
  }
}
