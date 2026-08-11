// External links in the Wails desktop shell.
//
// Every outward-pointing link in the app renders as target="_blank" (see the
// `a` override in markdown.tsx and the References rail in FactBody.tsx). In a
// browser that opens a tab. In the desktop window the page runs inside a
// WKWebView, and asking for a new window there routes through the WKUIDelegate
// method `webView:createWebViewWithConfiguration:forNavigationAction:`. Wails
// v3's macOS delegate does not implement it — its only WKUIDelegate method is
// the file-input open panel — and WKWebView's documented behaviour when that
// method is absent is to drop the request silently. So on desktop those links
// were inert: click, nothing.
//
// Dropping target="_blank" is not the fix. Then the link navigates the webview
// in place, and since the app has no router that is a full unload: app state
// gone, and no chrome to get back with. The link has to leave the webview
// entirely, which means handing the URL to the OS.
//
// Wails exposes exactly that as Browser.OpenURL, reachable over its runtime
// transport — a POST to /wails/runtime, which the framework's HTTP
// transport middleware answers ahead of knomit's own asset handler. Calling it
// with a bare fetch (rather than importing @wailsio/runtime) keeps the Wails
// dependency out of the shared browser bundle, the same trade TopBar.tsx makes
// when it posts "wails:drag" by hand.

/**
 * Wails' runtime endpoint. Its HTTP transport middleware wraps the asset
 * server, so this is answered before knomit's own handler ever sees the path.
 */
const RUNTIME_PATH = '/wails/runtime';
/** Wails runtime object ID for Browser. See objectNames in the v3 runtime. */
const BROWSER_OBJECT = 9;
/** Wails Browser method ID for OpenURL. */
const OPEN_URL_METHOD = 0;

/**
 * Ask the desktop shell to open `url` in the user's default browser.
 *
 * Rejects if the shell refuses the call, so callers can report it rather than
 * leaving the user with a click that did nothing — the very failure this
 * module exists to remove.
 */
export async function openExternal(url: string, fetchImpl: typeof fetch = fetch): Promise<void> {
  // Root-relative, not origin-derived. The desktop window is served over the
  // custom `wails://localhost` scheme, and `location.origin` is only
  // well-defined for the URL spec's special schemes — a relative path resolves
  // against the document regardless of how that stringifies.
  const res = await fetchImpl(RUNTIME_PATH, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      object: BROWSER_OBJECT,
      method: OPEN_URL_METHOD,
      args: { url },
    }),
  });
  if (!res.ok) {
    throw new Error(`Browser.OpenURL failed (${res.status}): ${await res.text()}`);
  }
}

export interface ExternalLinkOptions {
  /** Where to listen. Defaults to the document. */
  doc?: Document;
  /** Hand-off to the shell. Injected by tests. */
  open?: (url: string) => void | Promise<void>;
  /** Called when the hand-off fails. Defaults to a console warning. */
  onError?: (err: unknown, url: string) => void;
}

/** True only in the desktop shell, where config.js sets the flag. */
function inDesktopShell(): boolean {
  return typeof window !== 'undefined'
    && Boolean((window as Window & { __KNOMIT_DESKTOP__?: boolean }).__KNOMIT_DESKTOP__);
}

/**
 * Intercept clicks on http(s) links and route them to the OS browser.
 *
 * A single delegated listener rather than a per-link handler: the links this
 * has to cover are generated — autolinked URLs inside arbitrary fact bodies,
 * the References rail — so anything anchored to specific call sites goes stale
 * the moment a new surface renders a link.
 *
 * No-op outside the desktop shell, where target="_blank" already does the
 * right thing and intercepting would only replace a working tab with a
 * runtime call that has nothing to answer it.
 *
 * Returns a function that removes the listener.
 */
export function installExternalLinkHandler(opts: ExternalLinkOptions = {}): () => void {
  if (!inDesktopShell()) return () => {};

  const doc = opts.doc ?? document;
  const open = opts.open ?? ((url: string) => openExternal(url));
  const onError = opts.onError
    ?? ((err: unknown, url: string) => console.warn(`could not open ${url} externally:`, err));

  const onClick = (e: MouseEvent) => {
    // Bubble phase, and yield to anything that already handled the click: the
    // app's own in-place navigations (ref hops, breadcrumbs) get first refusal.
    if (e.defaultPrevented) return;
    const target = e.target;
    if (!(target instanceof Element)) return;
    const a = target.closest('a[href]');
    if (!a) return;
    const href = a.getAttribute('href');
    // Only http(s) leaves the app. Relative links, GFM footnote backrefs
    // (#user-content-fn-1) and non-web schemes stay in the webview — the same
    // boundary markdown.tsx uses to decide what gets target="_blank".
    if (!href || !/^https?:\/\//i.test(href)) return;

    e.preventDefault();
    // Modifier clicks land here too. The webview cannot open a tab or a window
    // for them either, so "open in the OS browser" is the only outcome that
    // beats nothing at all.
    try {
      const r = open(href);
      if (r && typeof (r as Promise<void>).catch === 'function') {
        (r as Promise<void>).catch((err) => onError(err, href));
      }
    } catch (err) {
      onError(err, href);
    }
  };

  doc.addEventListener('click', onClick);
  return () => doc.removeEventListener('click', onClick);
}
