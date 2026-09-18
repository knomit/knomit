import { describe, it, expect } from 'vitest';
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

// The markup between <div id="root"> and its matching close. The splash has to
// live in here: React's createRoot().render() replaces the container's
// children, which is what removes the splash on first render. Outside #root it
// would survive forever, on top of the app.
function rootInnerHTML(): string {
  const open = html.indexOf('<div id="root">');
  expect(open).toBeGreaterThan(-1);
  const after = html.slice(open + '<div id="root">'.length);
  const close = after.indexOf('\n    </div>');
  expect(close).toBeGreaterThan(-1);
  return after.slice(0, close);
}

describe('index.html pre-React splash', () => {
  it('is present inside #root, so React s first render replaces it', () => {
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
