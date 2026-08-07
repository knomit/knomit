import { describe, it, expect, beforeEach, beforeAll, vi } from 'vitest';
import { pickRepo, loadLastContext, saveLastContext, REPO_STORAGE_KEY, CONTEXT_STORAGE_KEY } from './repoSelection';
import type { RepoInfo } from './api';

const repos = (...names: string[]): RepoInfo[] => names.map(name => ({ name }));

// jsdom in this project does not expose localStorage. Install a minimal
// in-memory implementation so the persistence helpers can be exercised.
beforeAll(() => {
  if (typeof globalThis.localStorage !== 'undefined') return;
  const store = new Map<string, string>();
  globalThis.localStorage = {
    getItem: (k: string) => (store.has(k) ? store.get(k)! : null),
    setItem: (k: string, v: string) => void store.set(k, String(v)),
    removeItem: (k: string) => void store.delete(k),
    clear: () => store.clear(),
    key: (i: number) => Array.from(store.keys())[i] ?? null,
    get length() { return store.size; },
  } as Storage;
});

describe('pickRepo', () => {
  it('keeps the current repo when it still exists', () => {
    expect(pickRepo('work', repos('core', 'work'), 'core')).toBe('work');
  });

  it('falls back to the last-used repo when current is empty', () => {
    expect(pickRepo('', repos('core', 'work'), 'work')).toBe('work');
  });

  it('falls back to the last-used repo when current no longer exists', () => {
    // e.g. the user deleted "knomit"/"core" out from under an open session
    expect(pickRepo('core', repos('work'), 'work')).toBe('work');
  });

  it('ignores a last-used repo that no longer exists', () => {
    expect(pickRepo('', repos('core', 'work'), 'deleted')).toBe('core');
  });

  it('uses the first available repo when there is no usable current or last-used', () => {
    expect(pickRepo('', repos('alpha', 'beta'), null)).toBe('alpha');
  });

  it('returns empty string when the server has no repos', () => {
    expect(pickRepo('core', repos(), 'core')).toBe('');
  });

  it('never returns a name that is not in the list', () => {
    expect(pickRepo('ghost', repos('only'), 'phantom')).toBe('only');
  });
});

describe('loadLastContext / saveLastContext', () => {
  beforeEach(() => localStorage.clear());

  it('round-trips a repo context', () => {
    saveLastContext({ kind: 'repo', repo: 'work' });
    expect(loadLastContext()).toEqual({ kind: 'repo', repo: 'work' });
  });

  it('round-trips a lens context', () => {
    saveLastContext({ kind: 'lens', name: 'dev' });
    expect(loadLastContext()).toEqual({ kind: 'lens', name: 'dev' });
  });

  it('migrates a legacy bare-repo value to {kind:"repo"}', () => {
    // Old builds stored just the repo name under REPO_STORAGE_KEY.
    localStorage.setItem(REPO_STORAGE_KEY, 'legacy-repo');
    expect(loadLastContext()).toEqual({ kind: 'repo', repo: 'legacy-repo' });
  });

  it('prefers the JSON context key over the legacy bare-repo key', () => {
    localStorage.setItem(REPO_STORAGE_KEY, 'legacy-repo');
    saveLastContext({ kind: 'lens', name: 'dev' });
    expect(loadLastContext()).toEqual({ kind: 'lens', name: 'dev' });
  });

  it('returns null when nothing has been saved', () => {
    expect(loadLastContext()).toBeNull();
  });

  it('does not persist a repo context with an empty repo name', () => {
    saveLastContext({ kind: 'repo', repo: '' });
    expect(localStorage.getItem(CONTEXT_STORAGE_KEY)).toBeNull();
  });

  it('keeps the legacy repo key in sync for a repo context (downgrade safety)', () => {
    saveLastContext({ kind: 'repo', repo: 'work' });
    expect(localStorage.getItem(REPO_STORAGE_KEY)).toBe('work');
  });

  it('falls back to the legacy key when the context JSON is corrupt', () => {
    localStorage.setItem(CONTEXT_STORAGE_KEY, '{not valid json');
    localStorage.setItem(REPO_STORAGE_KEY, 'legacy-repo');
    expect(loadLastContext()).toEqual({ kind: 'repo', repo: 'legacy-repo' });
  });

  it('survives localStorage throwing', () => {
    const spy = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('disabled');
    });
    expect(loadLastContext()).toBeNull();
    spy.mockRestore();
  });
});
