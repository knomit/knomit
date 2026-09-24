import { describe, expect, it } from 'vitest';

declare const jsdom: { window: Window & typeof globalThis };

// Guards the Storage block in test-setup.ts. On Node >= 25 without it,
// `localStorage` is Node's (undefined) and `sessionStorage` is Node's own
// in-memory Storage, not jsdom's — see the comment there.
describe('test environment Web Storage', () => {
  for (const key of ['localStorage', 'sessionStorage'] as const) {
    it(`${key} is jsdom's working Storage`, () => {
      const storage = globalThis[key];
      expect(storage).toBe(jsdom.window[key]);
      expect(storage).toBeInstanceOf(jsdom.window.Storage);
      storage.clear();
      storage.setItem('k', 'v');
      expect(storage.getItem('k')).toBe('v');
      expect(storage).toHaveLength(1);
      storage.clear();
      expect(storage.getItem('k')).toBeNull();
    });
  }
});

// Guards the EventSource baseline test-setup.ts installs for EVERY test. A file
// that installs the fake in beforeEach and uninstalls it in afterEach must hand
// that baseline back, not delete it: the setup file's cleanup() runs LAST
// (vitest's "stack" hook order) and can flush a pending passive effect that
// constructs an EventSource. With the global deleted, that throws during
// unmount and the half-torn-down tree leaks into the next test (knomit#270).
describe('test environment EventSource', () => {
  it('uninstallFakeEventSource restores the baseline rather than deleting it', async () => {
    const { FakeEventSource, installFakeEventSource, uninstallFakeEventSource } = await import('./testEventSource');
    expect(globalThis.EventSource).toBe(FakeEventSource);
    installFakeEventSource();
    uninstallFakeEventSource();
    expect(globalThis.EventSource).toBe(FakeEventSource);
  });
});

// The helper above is only half of it: a test file that removes the global
// ITSELF in its afterEach reopens the same hole, because that afterEach also
// runs before the setup file's cleanup(). Three files did exactly that after
// the helper was fixed. This reads every test source so the next one fails
// here, with the reason, instead of as a teardown flake on a loaded runner.
describe('no test file removes the EventSource baseline itself', () => {
  it('every test file restores it through uninstallFakeEventSource', () => {
    const sources = import.meta.glob('./**/*.test.{ts,tsx}', { query: '?raw', import: 'default', eager: true }) as Record<string, string>;
    // A glob that silently lost part of the tree must not pass: there are ~120.
    expect(Object.keys(sources).length).toBeGreaterThan(100);
    // window too: under jsdom it is the same global object.
    const removal = /delete\s*\(?\s*(globalThis|window)[^;]*EventSource/;
    const offenders = Object.entries(sources).filter(([, src]) => removal.test(src)).map(([path]) => path).sort();
    expect(offenders, 'these test files remove globalThis.EventSource in their own teardown. Call ' +
      'uninstallFakeEventSource() instead: it restores the FakeEventSource baseline test-setup.ts installs. ' +
      'A file\'s afterEach runs BEFORE the setup file\'s cleanup() (vitest "stack" hook order), so a removed ' +
      'global makes a pending passive effect throw during unmount and leak the half-torn-down tree into the ' +
      'next test (knomit#270).').toEqual([]);
  });
});
