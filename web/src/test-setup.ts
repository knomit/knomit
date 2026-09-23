import '@testing-library/jest-dom/vitest';
import { afterEach } from 'vitest';
import { cleanup } from '@testing-library/react';
import { FakeEventSource } from './testEventSource';

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

// jsdom does not implement Element.scrollIntoView. Components that auto-scroll
// a focused row into view (CommitPanel file list, HistoryTimeline, etc.) crash
// otherwise. The scroll behavior itself isn't observable in tests, so a no-op
// polyfill is sufficient.
if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = function () {};
}

// jsdom does not implement EventSource. Components that subscribe to a server
// stream on mount (ManageSessions, RepoManager's session badge, App's branch
// events) would throw on construction otherwise — in EVERY test that renders
// them, not just the ones about streaming. Installed globally for the same
// reason as IntersectionObserver above; a test that wants to drive events
// imports FakeEventSource and reads its recorded instances.
globalThis.EventSource = FakeEventSource as unknown as typeof EventSource;

