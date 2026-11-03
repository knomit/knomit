// The API base, in a module of its own: api.ts re-exports apiUrl, and code
// that must build a URL while many tests replace './api' wholesale with a mock
// (the lens content stream, useLensContentRev) imports it from here instead,
// as listSearch.ts does for the list builders.
//
// apiBase reads the configured base at call time (not import time) so it is
// robust to script/module evaluation order and easy to test. The desktop sets
// window.__KNOMIT_API_BASE__ (see api.ts for the full story).
function apiBase(): string {
  return (typeof window !== 'undefined' &&
    (window as Window & { __KNOMIT_API_BASE__?: string }).__KNOMIT_API_BASE__) || '';
}

// apiUrl prefixes an absolute API path with the runtime API base.
export function apiUrl(path: string): string {
  return apiBase() + path;
}
