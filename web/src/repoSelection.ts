// Repo selection for the UI.
//
// The set of repositories is owned by the server and read from /api/v1/repos —
// the UI must never hardcode a repo name, because any repo (including the
// default) can be renamed or deleted server-side. pickRepo derives which repo
// to display from the live server list, and the load/save helpers remember the
// user's last explicit choice across reloads.

import type { RepoInfo } from './api';

export const REPO_STORAGE_KEY = 'knomit.repo';

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
export function pickRepo(current: string, repos: RepoInfo[], lastUsed: string | null): string {
  const exists = (name: string | null): boolean =>
    !!name && repos.some(r => r.name === name);
  if (exists(current)) return current;
  if (exists(lastUsed)) return lastUsed as string;
  return repos[0]?.name ?? '';
}

export function loadLastRepo(): string | null {
  try {
    return localStorage.getItem(REPO_STORAGE_KEY);
  } catch {
    return null;
  }
}

export function saveLastRepo(repo: string): void {
  if (!repo) return;
  try {
    localStorage.setItem(REPO_STORAGE_KEY, repo);
  } catch {
    /* quota exceeded / storage disabled — last-repo memory is best-effort */
  }
}
