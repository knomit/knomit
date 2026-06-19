import { describe, it, expect, beforeEach, beforeAll, vi } from 'vitest';
import { pickRepo, loadLastRepo, saveLastRepo, REPO_STORAGE_KEY } from './repoSelection';
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
    expect(pickRepo('work', repos('trunk', 'work'), 'trunk')).toBe('work');
  });

  it('falls back to the last-used repo when current is empty', () => {
    expect(pickRepo('', repos('trunk', 'work'), 'work')).toBe('work');
  });

  it('falls back to the last-used repo when current no longer exists', () => {
    // e.g. the user deleted "knomit"/"trunk" out from under an open session
    expect(pickRepo('trunk', repos('work'), 'work')).toBe('work');
  });

  it('ignores a last-used repo that no longer exists', () => {
    expect(pickRepo('', repos('trunk', 'work'), 'deleted')).toBe('trunk');
  });

  it('uses the first available repo when there is no usable current or last-used', () => {
    expect(pickRepo('', repos('alpha', 'beta'), null)).toBe('alpha');
  });

  it('returns empty string when the server has no repos', () => {
    expect(pickRepo('trunk', repos(), 'trunk')).toBe('');
  });

  it('never returns a name that is not in the list', () => {
    expect(pickRepo('ghost', repos('only'), 'phantom')).toBe('only');
  });
});

describe('loadLastRepo / saveLastRepo', () => {
  beforeEach(() => localStorage.clear());

  it('round-trips the last repo through localStorage', () => {
    saveLastRepo('work');
    expect(loadLastRepo()).toBe('work');
    expect(localStorage.getItem(REPO_STORAGE_KEY)).toBe('work');
  });

  it('does not persist an empty repo name', () => {
    saveLastRepo('');
    expect(localStorage.getItem(REPO_STORAGE_KEY)).toBeNull();
  });

  it('returns null when nothing has been saved', () => {
    expect(loadLastRepo()).toBeNull();
  });

  it('survives localStorage throwing', () => {
    const spy = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('disabled');
    });
    expect(loadLastRepo()).toBeNull();
    spy.mockRestore();
  });
});
