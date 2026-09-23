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
