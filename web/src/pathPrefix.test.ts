// The three moving parts that let one bundle serve both a same-origin
// deployment and one mounted under a path prefix by a prefix-stripping reverse
// proxy (code-server's /proxy/<port>/, an nginx `location`).
//
// None of them is reachable from application code: vite.config.ts is build
// input, public/config.js ships as a static file, and the trailing-slash guard
// is inline in index.html. So they are pulled in as source text and exercised
// directly — otherwise the only thing that notices a regression is a blank page
// in a deployment nobody runs in CI.
//
// `?raw` rather than node:fs: tsconfig.app.json deliberately carries only
// `vite/client` types, and reaching for node:fs here would mean granting node
// types to all of src/.
import { describe, expect, it, vi } from 'vitest';
import indexHtml from '../index.html?raw';
import configJs from '../public/config.js?raw';
import viteConfigSrc from '../vite.config.ts?raw';

// --- vite base -------------------------------------------------------------

describe('vite base', () => {
  it('emits relative asset URLs', () => {
    // Line comments are stripped first so that the config's own prose about
    // `base: '/'` cannot satisfy — or defeat — the assertion.
    const code = viteConfigSrc.split('\n').filter(l => !l.trim().startsWith('//')).join('\n');
    // Absolute ('/', vite's default) makes the browser resolve /assets/… against
    // the ORIGIN root, which under a proxy belongs to the proxy, not knomit.
    expect(code).toMatch(/^\s*base:\s*'\.\/',\s*$/m);
  });
});

// --- public/config.js ------------------------------------------------------

// Run the shipped static config.js against a stubbed document, and report the
// base it sets. Executing the real file (rather than restating its expression)
// is the point: this fails if the file stops setting the global at all.
function deriveApiBase(baseURI: string): unknown {
  const win: Record<string, unknown> = {};
  new Function('window', 'document', configJs)(win, { baseURI });
  return win.__KNOMIT_API_BASE__;
}

describe('config.js API base', () => {
  it.each([
    ['same-origin cloud build', 'https://knomit.example/', ''],
    ['mounted under a prefix', 'https://box.example/proxy/19278/', '/proxy/19278'],
    ['mounted under a nested prefix', 'https://box.example/a/b/c/', '/a/b/c'],
    ['ignores query and fragment', 'https://knomit.example/proxy/1/?x=1', '/proxy/1'],
  ])('%s: %s -> %o', (_name, baseURI, expected) => {
    expect(deriveApiBase(baseURI)).toBe(expected);
  });

  it('produces a base that concatenates with an API path', () => {
    // Every caller in api.ts appends a path that already starts with '/', so a
    // trailing slash here would produce '//api/v1/...'.
    expect(deriveApiBase('https://box.example/proxy/19278/') + '/api/v1/repos')
      .toBe('/proxy/19278/api/v1/repos');
    expect(deriveApiBase('https://knomit.example/') + '/api/v1/repos')
      .toBe('/api/v1/repos');
  });
});

// --- index.html ------------------------------------------------------------

describe('index.html', () => {
  it('has no root-absolute asset references', () => {
    // vite rewrites the tags it injects, but not the hand-written ones. A
    // reintroduced href="/favicon.svg" resolves against the proxy's root.
    const absolute = [...indexHtml.matchAll(/(?:src|href)="(\/[^/][^"]*)"/g)].map(m => m[1]);
    expect(absolute).toEqual([]);
  });

  it('still references favicon.svg, config.js and the entry, relatively', () => {
    // Guards the assertion above against passing because the tags were deleted.
    expect(indexHtml).toContain('href="./favicon.svg"');
    expect(indexHtml).toContain('src="./config.js"');
    expect(indexHtml).toContain('src="./src/main.tsx"');
  });
});

// --- the trailing-slash guard ---------------------------------------------

// The guard is inline in index.html's <head> — the first <script> with no src.
function trailingSlashGuard(): string {
  const m = indexHtml.match(/<script>([\s\S]*?)<\/script>/);
  if (!m) throw new Error('index.html has no inline guard script');
  return m[1];
}

function runGuard(loc: Partial<Location>): { replaced: string | null } {
  let replaced: string | null = null;
  const location = {
    search: '',
    hash: '',
    replace: vi.fn((url: string) => { replaced = url; }),
    ...loc,
  };
  new Function('location', trailingSlashGuard())(location);
  return { replaced };
}

describe('trailing-slash guard', () => {
  it('redirects a prefixed mount that is missing its trailing slash', () => {
    // Without this the browser treats `proxy` as the directory and every
    // ./assets/… resolves to /proxy/assets/… — 404, blank page.
    expect(runGuard({ protocol: 'https:', pathname: '/proxy/19278' }).replaced)
      .toBe('/proxy/19278/');
  });

  it('carries query and fragment across the redirect', () => {
    expect(runGuard({
      protocol: 'https:', pathname: '/proxy/19278', search: '?a=1', hash: '#f',
    }).replaced).toBe('/proxy/19278/?a=1#f');
  });

  it('does not redirect when the slash is already there — no loop', () => {
    // The replacement path always ends in '/', so this is the second load.
    expect(runGuard({ protocol: 'https:', pathname: '/proxy/19278/' }).replaced).toBeNull();
    expect(runGuard({ protocol: 'https:', pathname: '/' }).replaced).toBeNull();
  });

  it('leaves the desktop shell alone', () => {
    // macOS: the custom wails:// scheme is excluded by protocol. Every platform:
    // tools/desktop/app.go calls window.SetURL("/"), so the pathname ends in '/'
    // anyway. Either guard alone is sufficient; both are asserted so that
    // dropping one is a test failure rather than a silent narrowing.
    expect(runGuard({ protocol: 'wails:', pathname: '/index.html' }).replaced).toBeNull();
    expect(runGuard({ protocol: 'http:', pathname: '/' }).replaced).toBeNull();
  });
});
