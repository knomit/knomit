import '@testing-library/jest-dom/vitest';
import { afterEach } from 'vitest';
import { cleanup } from '@testing-library/react';

// @testing-library/react v16's auto-registered afterEach(cleanup) only runs
// when `afterEach` is a global function. This project's vitest config does
// not set `globals: true`, so we register cleanup explicitly.
afterEach(() => {
  cleanup();
});

// jsdom does not implement IntersectionObserver. Components that use it for
// infinite scroll (HistoryTimeline, LeftPanel) crash on mount otherwise.
class MockIntersectionObserver {
  observe() {}
  unobserve() {}
  disconnect() {}
  takeRecords() { return []; }
  root = null;
  rootMargin = '';
  thresholds = [];
}
globalThis.IntersectionObserver = MockIntersectionObserver as unknown as typeof IntersectionObserver;
