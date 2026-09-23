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
