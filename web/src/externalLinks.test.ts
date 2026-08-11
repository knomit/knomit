import { describe, it, expect, vi, beforeEach, afterEach, type Mock } from 'vitest';
import { installExternalLinkHandler, openExternal } from './externalLinks';

type DesktopWindow = Window & { __KNOMIT_DESKTOP__?: boolean };

function setDesktop(on: boolean) {
  if (on) (window as DesktopWindow).__KNOMIT_DESKTOP__ = true;
  else delete (window as DesktopWindow).__KNOMIT_DESKTOP__;
}

/** Anchor in the document, so a dispatched click actually bubbles to it. */
function anchor(html: string): HTMLAnchorElement {
  const host = document.createElement('div');
  host.innerHTML = html;
  document.body.appendChild(host);
  return host.querySelector('a')!;
}

function click(el: Element, init: MouseEventInit = {}): MouseEvent {
  const e = new MouseEvent('click', { bubbles: true, cancelable: true, ...init });
  el.dispatchEvent(e);
  return e;
}

describe('installExternalLinkHandler', () => {
  let uninstall: (() => void) | undefined;
  let open: Mock<(url: string) => void>;

  beforeEach(() => {
    open = vi.fn<(url: string) => void>();
  });

  afterEach(() => {
    uninstall?.();
    uninstall = undefined;
    document.body.innerHTML = '';
    setDesktop(false);
  });

  it('does nothing outside the desktop shell, where target=_blank already works', () => {
    setDesktop(false);
    uninstall = installExternalLinkHandler({ open });

    const a = anchor('<a href="https://example.com/x" target="_blank">x</a>');
    const e = click(a);

    expect(open).not.toHaveBeenCalled();
    expect(e.defaultPrevented).toBe(false);
  });

  it('hands an external link to the OS instead of the webview', () => {
    setDesktop(true);
    uninstall = installExternalLinkHandler({ open });

    const a = anchor('<a href="https://example.com/x" target="_blank">x</a>');
    const e = click(a);

    expect(open).toHaveBeenCalledWith('https://example.com/x');
    // Must be prevented: letting it through is what leaves the click inert.
    expect(e.defaultPrevented).toBe(true);
  });

  it('resolves the anchor from a click on a nested element', () => {
    setDesktop(true);
    uninstall = installExternalLinkHandler({ open });

    const a = anchor('<a href="https://example.com/y"><span>go</span></a>');
    click(a.querySelector('span')!);

    expect(open).toHaveBeenCalledWith('https://example.com/y');
  });

  it('routes a modifier click externally too — the webview cannot open a tab either way', () => {
    setDesktop(true);
    uninstall = installExternalLinkHandler({ open });

    const a = anchor('<a href="https://example.com/z">z</a>');
    const e = click(a, { metaKey: true });

    expect(open).toHaveBeenCalledWith('https://example.com/z');
    expect(e.defaultPrevented).toBe(true);
  });

  it.each([
    ['a GFM footnote backref', '<a href="#user-content-fn-1">1</a>'],
    ['an in-app relative link', '<a href="/facts/x">x</a>'],
    ['a non-web scheme', '<a href="mailto:a@b.c">mail</a>'],
    ['an anchor with no href', '<a>bare</a>'],
  ])('leaves %s alone', (_label, html) => {
    setDesktop(true);
    uninstall = installExternalLinkHandler({ open });

    const e = click(anchor(html));

    expect(open).not.toHaveBeenCalled();
    expect(e.defaultPrevented).toBe(false);
  });

  it('yields to a handler that already prevented the click', () => {
    setDesktop(true);
    uninstall = installExternalLinkHandler({ open });

    const a = anchor('<a href="https://example.com/x">x</a>');
    a.addEventListener('click', (e) => e.preventDefault());
    click(a);

    expect(open).not.toHaveBeenCalled();
  });

  it('stops intercepting once uninstalled', () => {
    setDesktop(true);
    const off = installExternalLinkHandler({ open });
    off();

    const e = click(anchor('<a href="https://example.com/x">x</a>'));

    expect(open).not.toHaveBeenCalled();
    expect(e.defaultPrevented).toBe(false);
  });

  it('swallows a failure to reach the shell rather than rejecting into the click', async () => {
    setDesktop(true);
    const boom = vi.fn().mockRejectedValue(new Error('no runtime'));
    const onError = vi.fn();
    uninstall = installExternalLinkHandler({ open: boom, onError });

    click(anchor('<a href="https://example.com/x">x</a>'));
    await vi.waitFor(() => expect(onError).toHaveBeenCalled());
  });
});

describe('openExternal', () => {
  it('posts a Wails Browser.OpenURL runtime call', async () => {
    const fetchImpl = vi.fn().mockResolvedValue(new Response('', { status: 200 }));

    await openExternal('https://example.com/x', fetchImpl);

    expect(fetchImpl).toHaveBeenCalledTimes(1);
    const [url, init] = fetchImpl.mock.calls[0];
    // Root-relative: the desktop window's origin is the custom wails:// scheme,
    // for which location.origin is not a dependable base.
    expect(url).toBe('/wails/runtime');
    expect(init.method).toBe('POST');
    expect(init.headers).toMatchObject({ 'Content-Type': 'application/json' });
    // object 9 = Browser, method 0 = OpenURL. See the Wails v3 message processor.
    expect(JSON.parse(init.body)).toEqual({
      object: 9,
      method: 0,
      args: { url: 'https://example.com/x' },
    });
  });

  it('rejects when the shell refuses the call, so the caller can log it', async () => {
    const fetchImpl = vi.fn().mockResolvedValue(new Response('nope', { status: 400 }));

    await expect(openExternal('https://example.com/x', fetchImpl)).rejects.toThrow(/nope/);
  });
});
