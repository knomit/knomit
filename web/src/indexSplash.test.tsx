import { describe, it, expect, vi, afterEach } from 'vitest';
// act from 'react', not the deprecated react-dom/test-utils.
import { act } from 'react';
import { createRoot } from 'react-dom/client';
// ?raw, not node:fs — the web tsconfig has no node types, and Vite resolves
// this the same way the app resolves any other asset.
import html from '../index.html?raw';

// Regression guard for "blank page during the download".
//
// The splash exists to cover the window before React runs: the bundle
// download, and on desktop the blocking config.js tag. Anything it had to
// FETCH would be unavailable during exactly that window, so the rule is not
// style preference — an external stylesheet or font in the splash makes it
// useless at the only moment it matters.

// The markup between <div id="root"> and its matching close.
function rootInnerHTML(): string {
  const open = html.indexOf('<div id="root">');
  expect(open).toBeGreaterThan(-1);
  const after = html.slice(open + '<div id="root">'.length);
  const close = after.indexOf('\n    </div>');
  expect(close).toBeGreaterThan(-1);
  return after.slice(0, close);
}

describe('index.html pre-React splash', () => {
  afterEach(() => { vi.restoreAllMocks(); });

  // THE claim, tested as itself rather than through a proxy.
  //
  // "It lives inside #root, so React's first render replaces it" used to be
  // asserted by checking WHERE the markup sits in the file. That is a location
  // check: it says nothing about what React does with the markup, so it would
  // keep passing if the splash stopped being cleared. This renders into a
  // container that actually holds the splash and asserts the outcome.
  //
  // WHAT IT DOES NOT CATCH, measured rather than assumed: a switch to
  // hydrateRoot. On React 19 + jsdom, hydrateRoot against this mismatched
  // markup produces the IDENTICAL observable result — splash gone, style gone,
  // app rendered, zero console.error calls — because React recovers from a
  // hydration mismatch by clearing the container and client-rendering, and
  // reports it through onRecoverableError rather than console. Probed both
  // ways; they are indistinguishable from here. So do not read this test as a
  // guard on the rendering entry point. It guards the OUTCOME: that the
  // splash, including its inline <style>, is gone once React has rendered.
  it('is cleared by createRoot on first render, inline style and all', async () => {
    // Without this React logs "The current testing environment is not
    // configured to support act(...)" through console.error — and the
    // no-console assertion below would then fail for a reason that looks
    // exactly like the hydrateRoot warning this test exists to rule out.
    // RTL's render() sets it; a bare createRoot() does not.
    (globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

    const container = document.createElement('div');
    container.id = 'root';
    container.innerHTML = rootInnerHTML();
    document.body.appendChild(container);

    // Precondition: the fixture really is the splash, so the assertions below
    // cannot pass vacuously against an empty container.
    expect(container.querySelector('#boot-splash')).not.toBeNull();
    expect(container.querySelector('style')).not.toBeNull();

    // Silence is an assertion, not noise suppression: a React warning during
    // this render — a bad container, a stray key, a future deprecation — would
    // otherwise pass unnoticed.
    const errSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});

    const root = createRoot(container);
    await act(async () => { root.render(<div data-testid="app-rendered">app</div>); });

    expect(container.querySelector('#boot-splash')).toBeNull();
    // The inline <style> is inside #root too, so it goes with the rest. Left
    // behind, it would leak a @keyframes into the running app.
    expect(container.querySelector('style')).toBeNull();
    expect(container.querySelector('[data-testid="app-rendered"]')).not.toBeNull();
    expect(errSpy).not.toHaveBeenCalled();
    expect(warnSpy).not.toHaveBeenCalled();

    await act(async () => { root.unmount(); });
    container.remove();
  });

  it('is present inside #root, where the render above needs it to be', () => {
    const inner = rootInnerHTML();
    expect(inner).toContain('id="boot-splash"');
    expect(inner).toContain('knomit');
  });

  it('references no external asset, or it could not render while they load', () => {
    const inner = rootInnerHTML();
    // No script, stylesheet, image, font or any URL fetch of any kind.
    expect(inner).not.toMatch(/<script/i);
    expect(inner).not.toMatch(/<link/i);
    expect(inner).not.toMatch(/<img/i);
    expect(inner).not.toMatch(/url\(/i);
    expect(inner).not.toMatch(/https?:/i);
    expect(inner).not.toMatch(/@font-face/i);
  });

  it('carries its own animation inline so it moves without JS', () => {
    const inner = rootInnerHTML();
    expect(inner).toContain('@keyframes knomit-splash');
    expect(inner).toContain('animation:');
  });

  it('matches the boot screen background, so the handover is invisible', () => {
    // BootScreen renders #141414; a different splash colour would flash.
    expect(rootInnerHTML()).toContain('#141414');
  });

  it('still loads config.js and the module entry after #root', () => {
    // The splash must not have displaced the real boot path.
    expect(html).toContain('src="./config.js"');
    expect(html).toContain('src="./src/main.tsx"');
  });
});
