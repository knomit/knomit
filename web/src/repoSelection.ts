// Repo selection for the UI.
//
// The set of repositories is owned by the server and read from /api/v1/repos —
// the UI must never hardcode a repo name, because any repo (including the
// default) can be renamed or deleted server-side. pickRepo derives which repo
// to display from the live server list, and loadLastContext/saveLastContext
// remember the user's last explicit choice across reloads.

import { repoAvailable } from './api';
import type { RepoInfo } from './api';
import type { BrowseContext } from './state';

export const REPO_STORAGE_KEY = 'knomit.repo';
// The last browse context (repo | lens) is stored as a JSON tagged union under
// its own key. The legacy REPO_STORAGE_KEY (a bare repo name string) is still
// read as a migration fallback so an existing user lands back on their repo.
export const CONTEXT_STORAGE_KEY = 'knomit.context';

// pickRepo chooses which repo the UI should show, given the repos the server
// currently knows about, the currently-selected repo, and the last repo the
// user explicitly picked (persisted across reloads).
//
// Precedence:
//   1. current  — keep it if it still exists (don't yank the user off a repo
//                  they're viewing when the list is refetched)
//   2. lastUsed — the user's last explicit choice, if it still exists
//   3. repos[0] — first available (the server returns the list sorted)
//   4. ''       — only when the server has no repos at all
//
// "Exists" here means READABLE, not merely listed. The listing now includes
// registered repos with no live store, whose every endpoint answers 409 — and
// landing the whole app on one would present a blank browse surface stacked
// with errors as if that were the repo's content. They stay visible and
// explained in Manage; they are just never the automatic destination. A repo
// the user was already browsing that GOES unavailable is dropped by the same
// rule, which is the honest outcome: there is nothing left to read there.
export function pickRepo(current: string, repos: RepoInfo[], lastUsed: string | null): string {
  const readable = repos.filter(repoAvailable);
  const exists = (name: string | null): boolean =>
    !!name && readable.some(r => r.name === name);
  if (exists(current)) return current;
  if (exists(lastUsed)) return lastUsed as string;
  return readable[0]?.name ?? '';
}

// loadLastContext returns the last browse context the user was in, or null.
// Precedence:
//   1. the JSON tagged union under CONTEXT_STORAGE_KEY (repo | lens)
//   2. a legacy bare-repo name under REPO_STORAGE_KEY → {kind:'repo', repo}
//   3. null (nothing usable stored)
// Any parse/shape error is treated as "nothing stored" so a corrupt value can
// never wedge bootstrap.
export function loadLastContext(): BrowseContext | null {
  try {
    const raw = localStorage.getItem(CONTEXT_STORAGE_KEY);
    if (raw) {
      // A malformed value must not wedge bootstrap — swallow the parse error and
      // fall through to the legacy migration below.
      let parsed: Partial<BrowseContext> | null = null;
      try { parsed = JSON.parse(raw) as Partial<BrowseContext>; } catch { parsed = null; }
      if (parsed && parsed.kind === 'repo' && typeof parsed.repo === 'string' && parsed.repo) {
        return { kind: 'repo', repo: parsed.repo };
      }
      if (parsed && parsed.kind === 'lens' && typeof parsed.name === 'string' && parsed.name) {
        return { kind: 'lens', name: parsed.name };
      }
      // Unknown/corrupt shape: fall through to the legacy migration.
    }
    // Migration: an old bare-repo value loads as a {kind:'repo'} context.
    const legacy = localStorage.getItem(REPO_STORAGE_KEY);
    if (legacy) return { kind: 'repo', repo: legacy };
    return null;
  } catch {
    return null;
  }
}

// saveLastContext persists the browse context. An empty repo name (the initial
// pre-selection state) is not persisted: it is the state before the server's
// repo list has resolved, not a choice the user made.
export function saveLastContext(ctx: BrowseContext): void {
  if (ctx.kind === 'repo' && !ctx.repo) return;
  try {
    localStorage.setItem(CONTEXT_STORAGE_KEY, JSON.stringify(ctx));
    // Keep the legacy key in sync for a repo context so a downgrade still lands
    // the user on the right repo; a lens context has no legacy representation.
    if (ctx.kind === 'repo') localStorage.setItem(REPO_STORAGE_KEY, ctx.repo);
  } catch {
    /* quota exceeded / storage disabled — last-context memory is best-effort */
  }
}
