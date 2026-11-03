import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { FactSetView, NOTICE_REFRESHED } from './FactSetView';
import { PATH_UNIT_CHARS } from './factsetAnchors';
import { CHANGED_SINCE_CITED, TIP_DIFFERENT_VERSION, TIP_RULE1 } from './materializeParts';
import { NESTING, ribbonWidth } from './materializeOptions';
import { DEFAULT_OPTIONS } from './materializeApi';
import { FsError } from './factsetApi';
import type { FactSetApi, FactSetLink, FactSetNode, FactSetPage, FsSearch } from './factsetApi';
import type { SaveOutcome } from './saveFile';
import materializeCss from './materialize.css?raw';

class MockIO {
  static all: MockIO[] = [];
  live = false;
  watched: Element[] = [];
  cb: IntersectionObserverCallback;
  constructor(cb: IntersectionObserverCallback) { this.cb = cb; MockIO.all.push(this); }
  observe(el: Element) { this.live = true; this.watched.push(el); }
  unobserve() {}
  disconnect() { this.live = false; this.watched = []; }
  takeRecords() { return []; }
}
const realIO = globalThis.IntersectionObserver;
beforeEach(() => { MockIO.all = []; globalThis.IntersectionObserver = MockIO as unknown as typeof IntersectionObserver; });
afterEach(() => { globalThis.IntersectionObserver = realIO; vi.restoreAllMocks(); delete document.documentElement.dataset.matFocus; });

const REPO: FsSearch = { target: { kind: 'repo', repo: 'kb', branch: 'main' }, sort: 'relevance', params: 'q=x' };
const LENS: FsSearch = { target: { kind: 'lens', lens: 'dev' }, sort: 'recent', params: 'path=kb%2Fu' };

function n(unit: number, idx: number, title: string, extra: Partial<FactSetNode> = {}): FactSetNode {
  return {
    n: idx, path: `kb/z/${title.toLowerCase().replace(/\W+/g, '-')}.md`, repo: 'r', commit: 'c0ffee1', depth: idx - 1,
    role: idx === 1 ? 'root' : 'ref', title, type: 'observation', kind: 'epistemic', confidence: 0.7,
    anchor_id: `f-${title.toLowerCase().replace(/\W+/g, '')}`, unit, number: idx === 1 ? String(unit) : `${unit}.${idx - 1}`,
    tree_depth: idx === 1 ? 0 : 1, written: true, body: `${title} body.`, ...extra,
  };
}
const stub = (unit: number, idx: number, title: string, link: Partial<FactSetLink>, extra: Partial<FactSetNode> = {}): FactSetNode =>
  n(unit, idx, title, {
    written: false, body: undefined, ...extra,
    link: { anchor_id: `f-${title.toLowerCase().replace(/\W+/g, '')}`, direction: 'back', cited_blob: 'b', ...link },
  });

function env(units: number, unitTo: number, cursor: string | null, hasMore: boolean): Omit<FactSetPage, 'facts'> {
  return {
    mode: 'factset', title: 'Observations about x', search_line: 'text "x" · sorted by relevance', anchor: 'a', unpinned: false,
    cross_kb: false, depth: 4, max_nodes: 200, facts_limit: 500, units, unit_from: 1, unit_to: unitTo, cursor, has_more: hasMore,
  };
}

// Page 1: unit 1 = Alpha (root) + Shared (written, RULE 1 changed) + Gamma (a
// FORWARD stub to unit 3). Page 2: unit 2 = Beta (root, RULE 1 deleted) + a
// BACK stub to Shared at a different version. Unit 3 (only by seek) = Gamma.
const P1: FactSetPage = { ...env(3, 1, 'ID.2.0', true), facts: [
  n(1, 1, 'Alpha'),
  n(1, 2, 'Shared', { marks: ['changed_since_cited'] }),
  stub(1, 3, 'Gamma: a very long title that the stub must cut short', { direction: 'forward', unit: 3, number: '3' }, { anchor_id: 'f-gamma' }),
] };
const P2: FactSetPage = { ...env(3, 2, 'ID.3.0', true), facts: [
  n(2, 1, 'Beta', { marks: ['deleted'] }),
  stub(2, 2, 'Shared', { direction: 'back', unit: 1, different_version: true }),
] };
const P3: FactSetPage = { ...env(3, 3, 'ID.4.0', false), facts: [n(3, 1, 'Gamma: a very long title that the stub must cut short', { anchor_id: 'f-gamma' })] };

function makeApi(over: Partial<FactSetApi> = {}) {
  const calls: string[] = [];
  const api: FactSetApi = {
    first: vi.fn(async () => { calls.push('first'); return P1; }),
    next: vi.fn(async (_t, c) => { calls.push(`next ${c}`); return P2; }),
    seek: vi.fn(async (_t, _c, u) => { calls.push(`seek ${u}`); return u === 3 ? P3 : u === 2 ? P2 : P1; }),
    exportMarkdown: vi.fn(async (_t, c) => { calls.push(`export ${c}`); return { text: '# doc', serverName: 'fs-observations.md' }; }),
    close: vi.fn(async () => {}),
    ...over,
  };
  return { api, calls };
}

function nearBottom() {
  act(() => {
    for (const o of MockIO.all.filter(x => x.live)) {
      const sentinel = o.watched.find(e => (e as HTMLElement).dataset.testid === 'fs-sentinel');
      if (sentinel) o.cb([{ isIntersecting: true, target: sentinel } as unknown as IntersectionObserverEntry], o as unknown as IntersectionObserver);
    }
  });
}
const sections = () => screen.queryAllByTestId('mat-section') as HTMLElement[];

// Test 5 (plan 7.5). Each case asserts what the reader sees and which call was
// made.
//
// Sabotage, one run each: (a) render link.number in the stub (the stub reads
// "see 3"); (b) show a RULE 1 mark with the different_version tooltip; (c)
// enable Export before page 1 (clicking it before page 1 calls export with no
// cursor); (d) let Esc bubble (the shell's handler sees it).
describe('FactSetView (test 5)', () => {
  it('units: a ribbon in the gutter holds the unit number, the root heading is the title only; server numbers; tree-depth ladder', async () => {
    const { api } = makeApi();
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    liveLayout(screen.getByTestId('fs-scroll'));
    await waitFor(() => expect(sections()).toHaveLength(3));
    nearBottom();
    await waitFor(() => expect(sections()).toHaveLength(5));
    // The reader is on unit 1 (at the top of the pane), though unit 2 is loaded (unit_to 2).
    expect(screen.getByTestId('fs-progress').textContent).toBe('unit 1 of 3');
    const roots = sections().filter(s => s.classList.contains('k-fs-unit'));
    expect(roots.map(s => s.dataset.fsUnit)).toEqual(['1', '2']);
    // Book design §5 (ruling 2026-11-03, supersedes Q-UI8's numbered heading):
    // the number is in the ribbon on the root's row, in the gutter, and the
    // heading is the title alone.
    expect(roots.map(s => within(within(s).getByTestId('mat-gutter')).getByTestId('fs-ribbon').textContent)).toEqual(['1', '2']);
    expect(roots.map(s => within(s).queryByTestId('mat-num'))).toEqual([null, null]);
    expect(roots.map(s => s.querySelector('.k-mat-h')!.textContent)).toEqual(['Alpha', 'Beta']);
    const shared = sections()[1];
    expect(within(shared).getByTestId('mat-num').textContent?.trim()).toBe('1.1');
    expect(shared.dataset.td).toBe('1');
    expect(shared.dataset.lv).toBe('2');
    expect(screen.getByTestId('fs-title').textContent).toBe('Observations about x');
    expect(screen.getByTestId('fs-search-line').textContent).toBe('text "x" · sorted by relevance');
  });

  it('link stubs show "see" and a short title, no number, no body; only the written section is a scroll target', async () => {
    const { api } = makeApi();
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    await waitFor(() => expect(sections()).toHaveLength(3));
    const fwd = sections()[2];
    expect(fwd.dataset.fsStub).toBe('true');
    expect(fwd.dataset.anchor).toBeUndefined();
    const label = within(fwd).getByTestId('fs-stub-link').textContent ?? '';
    expect(label).toBe('see Gamma: a very long title that…');
    expect(label).not.toMatch(/\b3\b/);
    expect(fwd.querySelector('.k-mat-body')).toBeNull();
    expect(document.querySelectorAll('[data-anchor="f-shared"]')).toHaveLength(1);
  });

  it('RULE 1 marks are inline chips after the meta line, distinct from different_version (Q-UI12 tooltips)', async () => {
    const { api } = makeApi();
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    await waitFor(() => expect(sections()).toHaveLength(3));
    nearBottom();
    await waitFor(() => expect(sections()).toHaveLength(5));
    const rule1 = within(sections()[1]).getByTestId('fs-mark-rule1');
    expect(rule1.textContent).toBe(CHANGED_SINCE_CITED);
    expect(rule1.getAttribute('title')).toBe('the cited version differs from what the branch holds now');
    expect(rule1.getAttribute('title')).toBe(TIP_RULE1);
    expect(rule1.parentElement?.dataset.testid).toBe('mat-meta');
    expect(within(sections()[3]).getByTestId('fs-mark-deleted')).toBeTruthy();
    const dv = within(sections()[4]).getByTestId('fs-different-version');
    expect(dv.textContent).toBe(CHANGED_SINCE_CITED);
    expect(dv.getAttribute('title')).toBe('the cited version differs from the one shown here');
    expect(dv.getAttribute('title')).toBe(TIP_DIFFERENT_VERSION);
    expect(within(sections()[4]).queryByTestId('fs-mark-rule1')).toBeNull();
  });

  it('a back stub to a target on screen scrolls (no call); a forward stub seeks to its unit', async () => {
    const { api, calls } = makeApi();
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    await waitFor(() => expect(sections()).toHaveLength(3));
    nearBottom();
    await waitFor(() => expect(sections()).toHaveLength(5));
    fireEvent.click(within(sections()[4]).getByTestId('fs-stub-link'));
    expect(calls).toEqual(['first', 'next ID.2.0']);
    fireEvent.click(within(sections()[2]).getByTestId('fs-stub-link'));
    await waitFor(() => expect(calls).toEqual(['first', 'next ID.2.0', 'seek 3']));
    await waitFor(() => expect(sections().map(s => s.dataset.fsUnit)).toEqual(['3']));
  });

  it('Export is disabled until page 1 has arrived, then its URL carries the cursor and the server name is saved', async () => {
    let release!: (p: FactSetPage) => void;
    const { api, calls } = makeApi({ first: vi.fn(() => new Promise<FactSetPage>(r => { release = r; })) });
    const saved: string[] = [];
    const save = async (name: string): Promise<SaveOutcome> => { saved.push(name); return { kind: 'downloaded' } as SaveOutcome; };
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} save={save} />);
    const btn = screen.getByTestId('fs-export') as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
    fireEvent.click(btn);
    expect(api.exportMarkdown).not.toHaveBeenCalled();
    await act(async () => { release(P1); });
    await waitFor(() => expect((screen.getByTestId('fs-export') as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(screen.getByTestId('fs-export'));
    await waitFor(() => expect(saved).toEqual(['fs-observations.md']));
    expect(calls).toContain('export ID.2.0');
  });

  it('an export answered 404 re-creates the FactSet and shows the user\'s notice', async () => {
    const { api, calls } = makeApi({ exportMarkdown: vi.fn(async () => { throw new FsError('Cursor not found', 404); }) });
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    await waitFor(() => expect(sections()).toHaveLength(3));
    fireEvent.click(screen.getByTestId('fs-export'));
    await waitFor(() => expect(screen.getByTestId('fs-notice').textContent).toBe(NOTICE_REFRESHED));
    expect(NOTICE_REFRESHED).toBe('FactSet refreshed');
    expect(calls.filter(c => c === 'first')).toHaveLength(2);
    expect(screen.queryByTestId('fs-export-failed')).toBeNull();
  });

  it('Esc in Focus is taken in the capture phase, never reaches the shell, and closes the view like Back (3fedfce6, #710 M2)', async () => {
    const { api } = makeApi();
    const shell = vi.fn();
    const back = vi.fn();
    window.addEventListener('keydown', shell);
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={back} api={api} />);
    await waitFor(() => expect(sections()).toHaveLength(3));
    expect(document.documentElement.dataset.matFocus).toBe('1');
    fireEvent.keyDown(document.body, { key: 'Escape' });
    expect(shell).not.toHaveBeenCalled();
    expect(back).toHaveBeenCalledTimes(1);
    window.removeEventListener('keydown', shell);
  });

  it('on a lens: unit roots name their member, there is no Cross-KB switch, and the lens route is read', async () => {
    const lensP1: FactSetPage = { ...P1, facts: [n(1, 1, 'Alpha', { repo_name: 'alpha' }), n(1, 2, 'Shared')] , has_more: false };
    const { api } = makeApi({ first: vi.fn(async () => lensP1) });
    render(<FactSetView search={LENS} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    await waitFor(() => expect(sections()).toHaveLength(2));
    expect(within(sections()[0]).getByTestId('fs-repo-name').textContent).toBe('alpha');
    expect(within(sections()[1]).queryByTestId('fs-repo-name')).toBeNull();
    expect(screen.queryByTestId('fs-cross-kb')).toBeNull();
    expect(api.first).toHaveBeenCalledWith(LENS, DEFAULT_OPTIONS, expect.anything());
  });

  it('units: 0 is the empty state, with no export', async () => {
    const { api } = makeApi({ first: vi.fn(async () => ({ ...env(0, 0, null, false), facts: [] })) });
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    await waitFor(() => expect(screen.getByTestId('fs-empty')).toBeTruthy());
    expect((screen.getByTestId('fs-export') as HTMLButtonElement).disabled).toBe(true);
  });
});

// ── Where a click lands (M1, L2, L3) ────────────────────────────────────────
//
// jsdom lays nothing out, so every section reports a top of 100 x its index in
// the article and the scroller records what it is scrolled to: the section a
// scroll lands on is the one whose top the pane moved to.

function layOut() {
  const rect = vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(function (this: Element) {
    const all = Array.from(document.querySelectorAll('[data-testid="mat-section"]'));
    const i = all.indexOf(this);
    const top = i >= 0 ? 1000 + 100 * i : 0;
    return { top, bottom: top + 50, left: 0, right: 0, width: 0, height: 50, x: 0, y: top, toJSON() {} } as DOMRect;
  });
  const landed: string[] = [];
  const desc = Object.getOwnPropertyDescriptor(Element.prototype, 'scrollTop')!;
  Object.defineProperty(HTMLElement.prototype, 'scrollTop', {
    configurable: true,
    get() { return 0; },
    set(v: number) {
      // The pane moves by (section top - pane top - PATH_H - 8): the section at top v + 36.
      const all = Array.from(document.querySelectorAll<HTMLElement>('[data-testid="mat-section"]'));
      const sec = all[(v + 36 - 1000) / 100];
      if (sec) landed.push(sec.dataset.anchor ?? `stub:${sec.dataset.fsUnit}.${sec.dataset.matN}`);
    },
  });
  return { landed, restore: () => { rect.mockRestore(); Object.defineProperty(HTMLElement.prototype, 'scrollTop', desc); } };
}

/** A paged FactSet: `per` sections per unit, `size` per page (crossing units). */
function pagedView(units: number, per: number, size: number, shape: (u: number, k: number) => Partial<FactSetNode> = () => ({})) {
  const calls: string[] = [];
  const sec = (u: number, k: number): FactSetNode =>
    n(u, k + 1, `U${u} S${k + 1}`, { anchor_id: `f-${u}-${k + 1}`, number: k === 0 ? String(u) : `${u}.${k}`, ...shape(u, k) });
  const from = (u: number, k: number): FactSetPage => {
    const facts: FactSetNode[] = [];
    let cu = u, ck = k;
    while (facts.length < size && cu <= units) { facts.push(sec(cu, ck)); ck++; if (ck >= per) { cu++; ck = 0; } }
    return { ...env(units, facts[facts.length - 1].unit, `F.${cu}.${ck}`, cu <= units), facts };
  };
  const api: FactSetApi = {
    first: vi.fn(async () => { calls.push('first'); return from(1, 0); }),
    next: vi.fn(async (_t, c) => { calls.push(`next ${c}`); const [, u, k] = c.split('.'); return from(Number(u), Number(k)); }),
    seek: vi.fn(async (_t, _c, u) => { calls.push(`seek ${u}`); return from(u, 0); }),
    exportMarkdown: vi.fn(), close: vi.fn(async () => {}),
  };
  return { api, calls };
}

// Sabotage (M1): drop readOn from the stream's seek: the run ends at section 5
// and the scroll lands on the unit root (f-2-1 / f-4-1), not the target.
describe('a link to a section past the first page of its unit (M1)', () => {
  it('a back link to a descendant past section 5 seeks, reads on, and scrolls to THAT anchor', async () => {
    // Unit 3's first section is a back stub to f-2-7 (section 7 of unit 2).
    const { api, calls } = pagedView(4, 8, 5, (u, k) => u === 3 && k === 1
      ? { written: false, body: undefined, title: 'U2 S7', anchor_id: 'f-2-7', link: { anchor_id: 'f-2-7', direction: 'back', unit: 2, cited_blob: 'b' } } : {});
    const lay = layOut();
    try {
      render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
      await waitFor(() => expect(sections()).toHaveLength(5));
      act(() => { (api.seek as ReturnType<typeof vi.fn>).mockClear(); });
      // Get unit 3 on screen first (the unit control seeks).
      fireEvent.change(screen.getByTestId('fs-unit'), { target: { value: '3' } });
      fireEvent.blur(screen.getByTestId('fs-unit'), { target: { value: '3' } });
      await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('3'));
      calls.length = 0;
      lay.landed.length = 0;
      fireEvent.click(within(sections()[1]).getByTestId('fs-stub-link'));
      await waitFor(() => expect(lay.landed).toContain('f-2-7'));
      expect(calls).toEqual(['seek 2', 'next F.2.5']);
      expect(lay.landed[lay.landed.length - 1]).toBe('f-2-7');
    } finally { lay.restore(); }
  });

  it('a forward link to N.k past the first page seeks to unit N, reads on, and scrolls to it', async () => {
    // Unit 1's second section is a forward stub to f-3-7 ("3.6").
    const { api, calls } = pagedView(3, 8, 5, (u, k) => u === 1 && k === 1
      ? { written: false, body: undefined, title: 'U3 S7', anchor_id: 'f-3-7', link: { anchor_id: 'f-3-7', direction: 'forward', unit: 3, number: '3.6', cited_blob: 'b' } } : {});
    const lay = layOut();
    try {
      render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
      await waitFor(() => expect(sections()).toHaveLength(5));
      fireEvent.click(within(sections()[1]).getByTestId('fs-stub-link'));
      await waitFor(() => expect(lay.landed).toContain('f-3-7'));
      expect(calls).toEqual(['first', 'seek 3', 'next F.3.5']);
      expect(lay.landed[lay.landed.length - 1]).toBe('f-3-7');
    } finally { lay.restore(); }
  });
});

// Sabotage (L3): drop the reach() branch of onGoto: the click makes no call
// and nothing lands.
describe('a builds-on link to a section of the unit not read yet (L3)', () => {
  it('reads on from the tail to section 8 of unit 1 and scrolls to it', async () => {
    const { api, calls } = pagedView(2, 9, 5, (u, k) => u === 1 && k === 0
      ? { builds_on: [{ n: 8, title: 'U1 S8', id: 'u1s8', anchor_id: 'f-1-8' }] } : {});
    const lay = layOut();
    try {
      render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
      await waitFor(() => expect(sections()).toHaveLength(5));
      fireEvent.click(within(sections()[0]).getByTestId('mat-builds-toggle'));
      fireEvent.click(within(sections()[0]).getByTestId('mat-builds-link'));
      await waitFor(() => expect(lay.landed).toContain('f-1-8'));
      expect(calls).toEqual(['first', 'next F.1.5']);
    } finally { lay.restore(); }
  });
});

// Sabotage (L2): render the head error in the tail block again: fs-head-error
// is missing and the error sits below the sections.
describe('a failure reading the unit above the run (L2)', () => {
  it('is shown at the top, above the first section, not at the tail', async () => {
    const { api } = pagedView(4, 2, 2);
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    await waitFor(() => expect(sections()).toHaveLength(2));
    fireEvent.change(screen.getByTestId('fs-unit'), { target: { value: '3' } });
    fireEvent.blur(screen.getByTestId('fs-unit'), { target: { value: '3' } });
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('3'));
    (api.seek as ReturnType<typeof vi.fn>).mockImplementation(async () => { throw new FsError('boom', 500); });
    // Only the reader's own scroll UP arms the top sentinel after a seek (QA 2).
    const userScroll = scrollPane(screen.getByTestId('fs-scroll'));
    await settle();
    userScroll(200);
    userScroll(100);
    act(() => {
      for (const o of MockIO.all.filter(x => x.live)) {
        const t = o.watched.find(e => (e as HTMLElement).dataset.testid === 'fs-top-sentinel');
        if (t) o.cb([{ isIntersecting: true, target: t } as unknown as IntersectionObserverEntry], o as unknown as IntersectionObserver);
      }
    });
    const err = await screen.findByTestId('fs-head-error');
    expect(err.textContent).toContain('boom');
    expect(err.compareDocumentPosition(sections()[0]) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.queryByTestId('fs-tail-error')).toBeNull();
  });
});

/** Makes the pane scrollable in jsdom: its scrollTop is kept (clamped at 0),
 *  and the returned function is the READER's scroll to a position (it fires
 *  the scroll event a real scroll would). */
function scrollPane(box: HTMLElement) {
  let v = 0;
  Object.defineProperty(box, 'scrollTop', { configurable: true, get: () => v, set: (x: number) => { v = Math.max(0, x); } });
  return (to: number) => { act(() => { v = to; fireEvent.scroll(box); }); };
}

/** A laid-out pane: section i sits at 10 + 100 i - scrollTop below the pane
 *  top, the top sentinel just above section 0, and the pane is as tall as its
 *  sections (so keeping the place on a prepend moves scrollTop). */
function liveLayout(box: HTMLElement) {
  const userScroll = scrollPane(box);
  const secs = () => Array.from(document.querySelectorAll('[data-testid="mat-section"]'));
  Object.defineProperty(box, 'scrollHeight', { configurable: true, get: () => 100 * secs().length });
  vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(function (this: Element) {
    const i = secs().indexOf(this);
    const st = box.scrollTop;
    const top = i >= 0 ? 10 + 100 * i - st : (this as HTMLElement).dataset?.testid === 'fs-top-sentinel' ? 9 - st : 0;
    return { top, bottom: top + (i >= 0 ? 100 : 1), left: 0, right: 0, width: 0, height: 0, x: 0, y: top, toJSON() {} } as DOMRect;
  });
  return userScroll;
}

const fireTop = () => act(() => {
  for (const o of MockIO.all.filter(x => x.live)) {
    const t = o.watched.find(e => (e as HTMLElement).dataset.testid === 'fs-top-sentinel');
    if (t) o.cb([{ isIntersecting: true, target: t } as unknown as IntersectionObserverEntry], o as unknown as IntersectionObserver);
  }
});
/** Waits for the run to settle (one frame after it landed). */
const settle = () => act(async () => { await new Promise<void>(r => requestAnimationFrame(() => r())); });
const seekUnit = (u: number) => {
  fireEvent.change(screen.getByTestId('fs-unit'), { target: { value: String(u) } });
  fireEvent.blur(screen.getByTestId('fs-unit'), { target: { value: String(u) } });
};

// QA 1: the "Related, not included" entries. A lens FactSet's not-followed
// leaf reads "<repo name> `<path>`" (ruling S2, as the export writes it); every
// other reason renders as before.
//
// Sabotage: render the lens leaf through the old "title || id || path
// (reason)" line: it reads "kb://eaa1900d37f1/kb/... (lens_repo)".
describe('not-included entries (QA 1)', () => {
  const rel = (sec: HTMLElement) => Array.from(sec.querySelectorAll<HTMLElement>('.k-mat-rel .k-mat-src')).map(e => e.textContent);

  it('a lens_repo leaf reads the repo name, then the fact path without kb://<id12>/, no reason code', async () => {
    const page: FactSetPage = { ...P1, has_more: false, facts: [n(1, 1, 'Alpha', {
      repo_name: 'alpha',
      not_included: [{ path: 'kb://eaa1900d37f1/kb/architecture/network/n1.md', reason: 'lens_repo', repo_name: 'beta' }],
    })] };
    const { api } = makeApi({ first: vi.fn(async () => page) });
    render(<FactSetView search={LENS} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    await waitFor(() => expect(sections()).toHaveLength(1));
    expect(rel(sections()[0])).toEqual(['beta kb/architecture/network/n1.md']);
    expect(sections()[0].querySelector('.k-mat-rel .k-mat-src code')?.textContent).toBe('kb/architecture/network/n1.md');
  });

  it('control: cross_kb_off on a repo FactSet keeps its rendering', async () => {
    const page: FactSetPage = { ...P1, has_more: false, facts: [n(1, 1, 'Alpha', {
      not_included: [
        { path: 'kb://0123456789ab/kb/x/t.md', title: 'Titled', id: 't', reason: 'cross_kb_off' },
        { path: 'kb://0123456789ab/kb/x/u.md', reason: 'cross_kb_off' },
      ],
    })] };
    const { api } = makeApi({ first: vi.fn(async () => page) });
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    await waitFor(() => expect(sections()).toHaveLength(1));
    expect(rel(sections()[0])).toEqual(['Titled (cross_kb_off)', 'kb://0123456789ab/kb/x/u.md (cross_kb_off)']);
  });
});

// QA 2: a seek lands with the top sentinel in view; the unit above must not be
// asked for until the reader scrolls. Scrolling up then prepends it (Q-UI9).
//
// Sabotage: drop the armedEpoch check in the observer: the sentinel's first
// intersection after landing calls seek 2.
describe('no eager prepend after a seek (QA 2)', () => {
  it('no loadBefore at landing; the reader\'s scroll up prepends the unit above', async () => {
    const { api, calls } = pagedView(4, 2, 2);
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    await waitFor(() => expect(sections()).toHaveLength(2));
    const userScroll = scrollPane(screen.getByTestId('fs-scroll'));
    seekUnit(3);
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('3'));
    calls.length = 0;
    fireTop(); // the sentinel is in view at landing
    await act(async () => { await new Promise(r => setTimeout(r, 0)); });
    expect(calls).toEqual([]);
    expect(sections()[0].dataset.fsUnit).toBe('3');
    await settle();
    userScroll(300);
    userScroll(100); // up
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('2'));
    expect(calls[0]).toBe('seek 2');
  });
});

// QA 4: the progress line names the unit the reader is on (the one at the top
// of the pane, as the Unit control shows), not the last unit loaded.
//
// Sabotage: render env.unit_to again: it reads "unit 5 of 20" on open.
describe('progress names the unit the reader is on (QA 4)', () => {
  it('open, scroll, seek, and scroll up into a prepended unit', async () => {
    const { api } = pagedView(20, 2, 10); // 5 units on page 1
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    const userScroll = liveLayout(screen.getByTestId('fs-scroll'));
    await waitFor(() => expect(sections()).toHaveLength(10));
    const progress = () => screen.getByTestId('fs-progress').textContent;
    const stepper = () => (screen.getByTestId('fs-unit') as HTMLInputElement).value;
    await waitFor(() => expect(progress()).toBe('unit 1 of 20'));
    expect(stepper()).toBe('1');
    userScroll(400); // unit 3's root (section 4) at the top
    expect(progress()).toBe('unit 3 of 20');
    expect(stepper()).toBe('3');
    seekUnit(10);
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('10'));
    await waitFor(() => expect(progress()).toBe('unit 10 of 20'));
    expect(stepper()).toBe('10');
    await settle();
    userScroll(50);
    userScroll(0); // up: unit 9 is prepended, the place is kept on unit 10
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('9'));
    await waitFor(() => expect(progress()).toBe('unit 10 of 20'));
    userScroll(0); // into unit 9
    expect(progress()).toBe('unit 9 of 20');
    expect(stepper()).toBe('9');
  });
});

// #726 review M1: a seek to a unit root lands at scrollTop 0 (the root sits
// above PATH_H + 8), where a wheel or swipe up scrolls nothing and fires no
// scroll event. That move is the reader's move up all the same.
//
// Sabotage: drop the onWheel handler: no call is made.
describe('a wheel up at the very top after a seek (#726 M1)', () => {
  it('lands at scrollTop 0; one wheel up prepends the unit above with exactly one loadBefore, no scroll down first', async () => {
    const { api, calls } = pagedView(4, 2, 2);
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    await waitFor(() => expect(sections()).toHaveLength(2));
    const box = screen.getByTestId('fs-scroll');
    scrollPane(box);
    seekUnit(3);
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('3'));
    await settle();
    expect(box.scrollTop).toBe(0);
    calls.length = 0;
    fireEvent.wheel(box, { deltaY: -120 });
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('2'));
    expect(calls).toEqual(['seek 2']);
  });

  it('control: a wheel down at the top asks for nothing', async () => {
    const { api, calls } = pagedView(4, 2, 2);
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    await waitFor(() => expect(sections()).toHaveLength(2));
    const box = screen.getByTestId('fs-scroll');
    scrollPane(box);
    seekUnit(3);
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('3'));
    await settle();
    calls.length = 0;
    fireEvent.wheel(box, { deltaY: 120 });
    fireTop();
    await act(async () => { await new Promise(r => setTimeout(r, 0)); });
    expect(calls).toEqual([]);
  });
});

// #726 review M2: only a move UP arms the top sentinel. Scrolling down and
// then loading the next page rebuilds the observer, whose first entry for the
// in-range sentinel must not prepend while the reader reads down.
//
// Sabotage: arm on any reader scroll (drop the `up` test in onScroll): the
// rebuilt observer's entry calls seek 2.
describe('a scroll down never arms the top sentinel (#726 M2)', () => {
  it('land, scroll down 100px, finish a loadMore: no loadBefore', async () => {
    const { api, calls } = pagedView(6, 2, 2);
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    await waitFor(() => expect(sections()).toHaveLength(2));
    const userScroll = scrollPane(screen.getByTestId('fs-scroll'));
    seekUnit(3);
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('3'));
    await settle();
    calls.length = 0;
    userScroll(100); // down
    nearBottom();
    await waitFor(() => expect(sections()).toHaveLength(4));
    expect(calls).toEqual(['next F.4.0']);
    fireTop(); // the rebuilt observer reports the sentinel in range
    await act(async () => { await new Promise(r => setTimeout(r, 0)); });
    expect(calls).toEqual(['next F.4.0']);
    expect(sections()[0].dataset.fsUnit).toBe('3');
  });
});

// #726 browser re-check: a run lands ONCE per landing request. `land` stays
// set for the whole run and byUnitN is rebuilt on every page, so a landing
// effect keyed on it snapped the reader back to the jump target on every
// append and prepend.
//
// Sabotage: re-land on every byUnitN change (drop the landed.current check):
// the append snaps scrollTop to 0 and the prepend lands on 174, not 350.
describe('a landing happens once per request (#726 re-check)', () => {
  it('after a jump, an append does not move scrollTop', async () => {
    const { api, calls } = pagedView(20, 2, 2);
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    const box = screen.getByTestId('fs-scroll');
    const userScroll = liveLayout(box);
    await waitFor(() => expect(sections()).toHaveLength(2));
    seekUnit(10);
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('10'));
    await settle();
    userScroll(250);
    calls.length = 0;
    nearBottom();
    await waitFor(() => expect(sections()).toHaveLength(4));
    expect(calls).toEqual(['next F.11.0']);
    expect(box.scrollTop).toBe(250);
  });

  it('after a jump, a prepend moves scrollTop by exactly the prepended height', async () => {
    const { api, calls } = pagedView(20, 2, 2);
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    const box = screen.getByTestId('fs-scroll');
    const userScroll = liveLayout(box);
    await waitFor(() => expect(sections()).toHaveLength(2));
    seekUnit(10);
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('10'));
    await settle();
    userScroll(250);
    calls.length = 0;
    userScroll(150); // up: unit 9 (two sections, 200px) is prepended
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('9'));
    expect(calls).toEqual(['seek 9']);
    expect(box.scrollTop).toBe(150 + 200);
  });

  it('a link-click seek, followed by an append, does not re-land', async () => {
    // Unit 1's second section is a forward stub to unit 4's root.
    const { api, calls } = pagedView(8, 2, 2, (u, k) => u === 1 && k === 1
      ? { written: false, body: undefined, title: 'U4 S1', anchor_id: 'f-4-1', link: { anchor_id: 'f-4-1', direction: 'forward', unit: 4, number: '4', cited_blob: 'b' } } : {});
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    const box = screen.getByTestId('fs-scroll');
    const userScroll = liveLayout(box);
    await waitFor(() => expect(sections()).toHaveLength(2));
    fireEvent.click(within(sections()[1]).getByTestId('fs-stub-link'));
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('4'));
    await settle();
    userScroll(250);
    calls.length = 0;
    nearBottom();
    await waitFor(() => expect(sections()).toHaveLength(4));
    expect(calls).toEqual(['next F.5.0']);
    expect(box.scrollTop).toBe(250);
  });
});

// #726 browser re-check (point 1): a prepend at scrollTop > 0 shifted the view
// twice, because native scroll anchoring kept the place AND the view added
// the height again; and the head skeleton pushed the run down while loading.
// The view's place-keeping is the only one: overflow-anchor is off on this
// scroller, and the skeleton's height is part of the place-keeping.
//
// jsdom runs no native anchoring, so the rule is asserted as CSS, and the
// manual math with mocked heights: section i sits at 10 + skeleton + 100 i -
// scrollTop, the skeleton (while mounted) is 80px above the run.
//
// Sabotage: (a) drop the skeleton branch of the place-keeping: the anchor
// moves 80px down while the unit above loads; (b) delete the .k-fs-scroll
// rule: the CSS assertion fails.
describe('a prepend keeps the reader\'s anchor steady (#726 point 1)', () => {
  it('the FactSet scroller turns native scroll anchoring off', async () => {
    const { api } = makeApi();
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    await waitFor(() => expect(sections()).toHaveLength(3));
    expect(screen.getByTestId('fs-scroll').classList.contains('k-fs-scroll')).toBe(true);
    expect(materializeCss.replace(/\s+/g, ' ')).toContain('.k-fs-scroll { overflow-anchor: none; }');
  });

  it('at scrollTop > 0 the anchor keeps its offset while the skeleton shows and after the unit is prepended', async () => {
    const { api, calls } = pagedView(20, 2, 2);
    const realSeek = (api.seek as ReturnType<typeof vi.fn>).getMockImplementation() as FactSetApi['seek'];
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    const box = screen.getByTestId('fs-scroll');
    const userScroll = scrollPane(box);
    const SKEL = 80;
    const secs = () => Array.from(document.querySelectorAll('[data-testid="mat-section"]'));
    const skel = () => (document.querySelector('[data-testid="fs-head-loading"]') ? SKEL : 0);
    Object.defineProperty(box, 'scrollHeight', { configurable: true, get: () => skel() + 100 * secs().length });
    vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(function (this: Element) {
      const i = secs().indexOf(this);
      const top = i >= 0 ? 10 + skel() + 100 * i - box.scrollTop : (this as HTMLElement).dataset?.testid === 'fs-top-sentinel' ? 9 - box.scrollTop : 0;
      return { top, bottom: top + (i >= 0 ? 100 : 1), left: 0, right: 0, width: 0, height: 0, x: 0, y: top, toJSON() {} } as DOMRect;
    });
    await waitFor(() => expect(sections()).toHaveLength(2));
    seekUnit(10);
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('10'));
    await settle();
    userScroll(250);
    let release!: () => void;
    (api.seek as ReturnType<typeof vi.fn>).mockImplementation(async (t, c, u, sig) => {
      await new Promise<void>(r => { release = r; });
      return realSeek(t, c, u, sig);
    });
    const anchor = () => document.querySelector('[data-fs-unit="10"][data-mat-n="1"]')!.getBoundingClientRect().top;
    userScroll(150); // up: the unit above starts loading
    const before = anchor();
    await waitFor(() => expect(screen.getByTestId('fs-head-loading')).toBeTruthy());
    expect(Math.abs(anchor() - before)).toBeLessThanOrEqual(1);
    calls.length = 0;
    await act(async () => { release(); });
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('9'));
    expect(screen.queryByTestId('fs-head-loading')).toBeNull();
    expect(Math.abs(anchor() - before)).toBeLessThanOrEqual(1);
    expect(box.scrollTop).toBe(150 + 200); // scrollTop + the prepended height
  });
});

// ── The Back pill and Back to top (user report 2026-11-02) ──────────────────
//
// Unit u's root has a long title; its second section is a FORWARD stub to
// unit u + ahead's root, titled like the target (as a stub is). Page size 2 =
// one unit per page. liveLayout: section i sits at 10 + 100 i - scrollTop,
// and a scroll to a section puts it at PATH_H + 8 = 36.
//
// Sabotage, one run each (table in the PR): (a) Back re-applies the saved
// scrollTop instead of re-seeking when the origin's run was dropped; (b) the
// pill shows the stub's own title (its target's); (c) Back to top sets
// scrollTop 0 without seeking (the reported bug); (d) the Back landing is
// treated as a builds-on landing (the origin stub is followed again); (e) no
// flash on arrival.
const ROOT_TITLE = (u: number) => `Orchestrating many agents across one shared harness, unit ${u}`;
/** stubLabel's rule: cut to 32 characters at a word boundary, then "…". */
const BACK_LABEL = 'Back to Orchestrating many agents…';
function linkedView(units: number, ahead: number) {
  return pagedView(units, 2, 2, (u, k) => k === 0
    ? { title: ROOT_TITLE(u) }
    : {
      written: false, body: undefined, title: `Multi-agent vs single-threaded target ${u + ahead}`, anchor_id: `stub-${u}`,
      link: { anchor_id: `f-${u + ahead}-1`, direction: 'forward', unit: u + ahead, number: String(u + ahead), cited_blob: 'b' },
    });
}
const pill = () => screen.queryByTestId('fs-back-pill');
const progressText = () => screen.getByTestId('fs-progress').textContent;
const unitStepper = () => (screen.getByTestId('fs-unit') as HTMLInputElement).value;
const clickStub = (i: number) => fireEvent.click(within(sections()[i]).getByTestId('fs-stub-link'));

describe('the Back pill (design §5; label ruling 2026-11-02 "back to title")', () => {
  it('a forward link to a LOADED target: it flashes, the pill reads "Back to <short origin title>", Back restores the exact scrollTop', async () => {
    const { api, calls } = linkedView(4, 1);
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    const box = screen.getByTestId('fs-scroll');
    const userScroll = liveLayout(box);
    await waitFor(() => expect(sections()).toHaveLength(2));
    nearBottom();
    await waitFor(() => expect(sections()).toHaveLength(4));
    await settle();
    userScroll(30);
    expect(pill()).toBeNull();
    calls.length = 0;
    clickStub(1); // unit 1's stub -> unit 2's root, loaded (section 2)
    expect(calls).toEqual([]);
    expect(box.scrollTop).toBe(30 + (10 + 200 - 30) - 36);
    expect(sections()[2].dataset.fsFlash).toBe('true');
    expect(pill()!.textContent).toBe(BACK_LABEL);
    fireEvent.click(pill()!);
    expect(box.scrollTop).toBe(30);
    expect(pill()).toBeNull();
    expect(calls).toEqual([]);
    expect(progressText()).toBe('unit 1 of 4');
    expect(unitStepper()).toBe('1');
  });

  it('a forward link to an UNLOADED unit: Back re-seeks the origin unit and restores the offset; a later page does not re-land', async () => {
    const { api, calls } = linkedView(12, 4);
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    const box = screen.getByTestId('fs-scroll');
    const userScroll = liveLayout(box);
    await waitFor(() => expect(sections()).toHaveLength(2));
    await settle();
    userScroll(30); // unit 1's stub (section 1) sits at 10 + 100 - 30 = 80
    calls.length = 0;
    clickStub(1);
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('5'));
    expect(calls).toEqual(['seek 5']);
    await waitFor(() => expect(sections()[0].dataset.fsFlash).toBe('true'));
    expect(pill()!.textContent).toBe(BACK_LABEL);
    calls.length = 0;
    fireEvent.click(pill()!);
    expect(pill()).not.toBeNull(); // kept until the Back has landed
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('1'));
    expect(pill()).toBeNull();
    expect(calls).toEqual(['seek 1']); // the origin stub is not followed again
    expect(box.scrollTop).toBe(30);
    await waitFor(() => expect(progressText()).toBe('unit 1 of 12'));
    expect(unitStepper()).toBe('1');
    await settle();
    userScroll(250);
    calls.length = 0;
    nearBottom();
    await waitFor(() => expect(sections()).toHaveLength(4));
    expect(calls).toEqual(['next F.2.0']);
    expect(box.scrollTop).toBe(250);
  });

  it('after a Back re-seek into unit 3, a prepend shifts scrollTop by exactly the prepended height (no double shift)', async () => {
    const { api, calls } = linkedView(12, 4);
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    const box = screen.getByTestId('fs-scroll');
    const userScroll = liveLayout(box);
    await waitFor(() => expect(sections()).toHaveLength(2));
    seekUnit(3);
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('3'));
    await settle();
    userScroll(30);
    clickStub(1); // unit 3's stub -> unit 7
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('7'));
    fireEvent.click(pill()!);
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('3'));
    expect(box.scrollTop).toBe(30);
    await settle();
    userScroll(250);
    calls.length = 0;
    userScroll(150); // up: unit 2 (two sections, 200px) is prepended
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('2'));
    expect(calls).toEqual(['seek 2']);
    expect(box.scrollTop).toBe(150 + 200);
  });

  it('keeps the LATEST origin only: two links followed, one Back returns to the second origin and the pill goes', async () => {
    const { api } = linkedView(4, 1);
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    const box = screen.getByTestId('fs-scroll');
    const userScroll = liveLayout(box);
    await waitFor(() => expect(sections()).toHaveLength(2));
    nearBottom();
    await waitFor(() => expect(sections()).toHaveLength(4));
    nearBottom();
    await waitFor(() => expect(sections()).toHaveLength(6));
    await settle();
    userScroll(30);
    clickStub(1); // -> unit 2's root at 174
    expect(box.scrollTop).toBe(174);
    clickStub(3); // unit 2's stub (at 10 + 300 - 174 = 136) -> unit 3's root
    expect(box.scrollTop).toBe(174 + (10 + 400 - 174) - 36);
    fireEvent.click(pill()!);
    expect(box.scrollTop).toBe(174);
    expect(pill()).toBeNull();
  });
});

describe('Back to top goes to the start of the FactSet (user report 2026-11-02)', () => {
  it('after a seek and a prepend it seeks to unit 1 and lands there; a second click stays at unit 1; a later page does not re-land', async () => {
    const { api, calls } = pagedView(20, 2, 2);
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    const box = screen.getByTestId('fs-scroll');
    const userScroll = liveLayout(box);
    await waitFor(() => expect(sections()).toHaveLength(2));
    seekUnit(10);
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('10'));
    await settle();
    userScroll(250);
    userScroll(150); // up: unit 9 is prepended, as in the report
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('9'));
    userScroll(700); // far enough down for the button
    calls.length = 0;
    fireEvent.click(screen.getByTestId('fs-top'));
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('1'));
    expect(calls).toEqual(['seek 1']);
    expect(box.scrollTop).toBe(0);
    await waitFor(() => expect(progressText()).toBe('unit 1 of 20'));
    expect(unitStepper()).toBe('1');
    await settle();
    userScroll(700);
    calls.length = 0;
    fireEvent.click(screen.getByTestId('fs-top'));
    expect(calls).toEqual([]);
    await waitFor(() => expect(box.scrollTop).toBe(0));
    expect(sections()[0].dataset.fsUnit).toBe('1');
    expect(progressText()).toBe('unit 1 of 20');
    userScroll(250);
    calls.length = 0;
    nearBottom();
    await waitFor(() => expect(sections()).toHaveLength(4));
    expect(calls).toEqual(['next F.2.0']);
    expect(box.scrollTop).toBe(250);
  });
});

// #736 review. M1: when the re-seek starts at the origin's unit, the saved
// offset can need content above the origin that the new run does not hold
// yet; it is read (loadBefore) before the offset is applied, never clamped.
// M2: a failed Back seek keeps the pill and the origin: a 404 re-creates the
// FactSet AT the origin, any other failure's Retry retries the Back.
//
// Sabotage, one run each: (h) apply the offset clamped at 0 without reading
// the unit above; (i) recover() ignores the pending Back (re-creates at
// topUnit, the jump's destination); (j) Retry after a failed Back retries the
// old run's tail; (k) the pill does not go on an option change.
describe('the Back pill: exact offset, failures, options (#736 review)', () => {
  it('M1: an origin 120px down with unit 2 above it: Back reads unit 2 back in, then puts the origin at exactly 120px', async () => {
    const { api, calls } = linkedView(12, 4);
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    const box = screen.getByTestId('fs-scroll');
    const userScroll = liveLayout(box);
    await waitFor(() => expect(sections()).toHaveLength(2));
    nearBottom();
    await waitFor(() => expect(sections()).toHaveLength(4));
    nearBottom();
    await waitFor(() => expect(sections()).toHaveLength(6));
    await settle();
    userScroll(390);
    const stubTop = () => document.querySelector('[data-fs-unit="3"][data-mat-n="2"]')!.getBoundingClientRect().top;
    expect(stubTop()).toBe(120);
    clickStub(5); // unit 3's stub -> unit 7
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('7'));
    calls.length = 0;
    fireEvent.click(pill()!);
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('2'));
    await waitFor(() => expect(box.scrollTop).toBe(190));
    expect(stubTop()).toBe(120);
    expect(calls).toEqual(['seek 3', 'seek 2']);
    expect(pill()).toBeNull();
    await waitFor(() => expect(progressText()).toBe('unit 3 of 12'));
    expect(unitStepper()).toBe('3');
  });

  it('M2: a 404 on the Back seek re-creates the FactSet at the ORIGIN unit and restores the offset', async () => {
    const { api, calls } = linkedView(12, 4);
    const realSeek = (api.seek as ReturnType<typeof vi.fn>).getMockImplementation() as FactSetApi['seek'];
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    const box = screen.getByTestId('fs-scroll');
    const userScroll = liveLayout(box);
    await waitFor(() => expect(sections()).toHaveLength(2));
    seekUnit(3);
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('3'));
    await settle();
    userScroll(30); // unit 3's stub at 80
    clickStub(1);
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('7'));
    let failed = false;
    (api.seek as ReturnType<typeof vi.fn>).mockImplementation(async (t, c, u, sig) => {
      if (u === 3 && !failed) { failed = true; calls.push('seek 3 (404)'); throw new FsError('gone', 404); }
      return realSeek(t, c, u, sig);
    });
    calls.length = 0;
    fireEvent.click(pill()!);
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('3'));
    expect(calls).toEqual(['seek 3 (404)', 'first', 'seek 3']);
    expect(box.scrollTop).toBe(30);
    expect(pill()).toBeNull();
    expect(screen.getByTestId('fs-notice').textContent).toBe(NOTICE_REFRESHED);
  });

  it('M2: any other failure keeps the pill, and Retry retries the Back', async () => {
    const { api, calls } = linkedView(12, 4);
    const realSeek = (api.seek as ReturnType<typeof vi.fn>).getMockImplementation() as FactSetApi['seek'];
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    const box = screen.getByTestId('fs-scroll');
    const userScroll = liveLayout(box);
    await waitFor(() => expect(sections()).toHaveLength(2));
    seekUnit(3);
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('3'));
    await settle();
    userScroll(30);
    clickStub(1);
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('7'));
    let failed = false;
    (api.seek as ReturnType<typeof vi.fn>).mockImplementation(async (t, c, u, sig) => {
      if (u === 3 && !failed) { failed = true; calls.push('seek 3 (500)'); throw new FsError('boom', 500); }
      return realSeek(t, c, u, sig);
    });
    calls.length = 0;
    fireEvent.click(pill()!);
    await screen.findByTestId('fs-tail-retry');
    expect(pill()!.textContent).toBe(BACK_LABEL);
    fireEvent.click(screen.getByTestId('fs-tail-retry'));
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('3'));
    expect(calls).toEqual(['seek 3 (500)', 'seek 3']);
    expect(box.scrollTop).toBe(30);
    expect(pill()).toBeNull();
  });

  it('an origin past the first page of its unit (2.7): Back seeks, reads on to it, and restores its offset', async () => {
    const { api, calls } = pagedView(6, 8, 5, (u, k) => u === 2 && k === 6
      ? { written: false, body: undefined, title: 'U5 S1', anchor_id: 'stub-2-7', link: { anchor_id: 'f-5-1', direction: 'forward', unit: 5, number: '5', cited_blob: 'b' } } : {});
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    const box = screen.getByTestId('fs-scroll');
    const userScroll = liveLayout(box);
    await waitFor(() => expect(sections()).toHaveLength(5));
    nearBottom();
    await waitFor(() => expect(sections()).toHaveLength(10));
    nearBottom();
    await waitFor(() => expect(sections()).toHaveLength(15));
    await settle();
    userScroll(1300); // 2.7 (section 14) at 110
    clickStub(14);
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('5'));
    expect(pill()!.textContent).toBe('Back to U2 S1');
    calls.length = 0;
    fireEvent.click(pill()!);
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('2'));
    expect(calls).toEqual(['seek 2', 'next F.2.5']);
    expect(box.scrollTop).toBe(500);
    expect(document.querySelector('[data-fs-unit="2"][data-mat-n="7"]')!.getBoundingClientRect().top).toBe(110);
  });

  it('a builds-on link to a written section flashes it and shows the pill; an option change takes the pill away', async () => {
    const onOptionsChange = vi.fn();
    const { api } = pagedView(2, 3, 6, (u, k) => u === 1 && k === 0
      ? { title: ROOT_TITLE(1), builds_on: [{ n: 3, title: 'U1 S3', id: 'u1s3', anchor_id: 'f-1-3' }] } : {});
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={onOptionsChange} onBack={() => {}} api={api} />);
    liveLayout(screen.getByTestId('fs-scroll'));
    await waitFor(() => expect(sections()).toHaveLength(6));
    fireEvent.click(within(sections()[0]).getByTestId('mat-builds-toggle'));
    fireEvent.click(within(sections()[0]).getByTestId('mat-builds-link'));
    expect(sections()[2].dataset.fsFlash).toBe('true');
    expect(pill()!.textContent).toBe(BACK_LABEL);
    fireEvent.click(screen.getByTestId('fs-unpin'));
    expect(onOptionsChange).toHaveBeenCalled();
    expect(pill()).toBeNull();
  });
});

// #736 re-verify, Low A: a Back restore waiting on the unit above must not
// pull the reader back once they have navigated elsewhere (an in-run link);
// Low B: nor once they wheel or swipe themselves.
//
// Sabotage, one run each: (m) takeOrigin no longer clears the pending
// restore: the prepend lands the reader on the old origin (290), not on the
// link target kept in place (474); (n) the wheel no longer abandons it: 290,
// not 300.
describe('a pending Back restore yields to the reader (#736 re-verify)', () => {
  // Three sections a unit, one unit a page: the root builds on section 3
  // (written, f-u-3); section 2 is a forward stub to unit u + 4.
  function threeView() {
    return pagedView(12, 3, 3, (u, k) => k === 0
      ? { title: ROOT_TITLE(u), builds_on: [{ n: 3, title: `U${u} S3`, id: `u${u}s3`, anchor_id: `f-${u}-3` }] }
      : k === 1
        ? { written: false, body: undefined, title: `Multi-agent vs single-threaded target ${u + 4}`, anchor_id: `stub-${u}`,
            link: { anchor_id: `f-${u + 4}-1`, direction: 'forward', unit: u + 4, number: String(u + 4), cited_blob: 'b' } }
        : {});
  }
  async function backWaitingOnUnit2() {
    const { api, calls } = threeView();
    const realSeek = (api.seek as ReturnType<typeof vi.fn>).getMockImplementation() as FactSetApi['seek'];
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    const box = screen.getByTestId('fs-scroll');
    const userScroll = liveLayout(box);
    await waitFor(() => expect(sections()).toHaveLength(3));
    nearBottom();
    await waitFor(() => expect(sections()).toHaveLength(6));
    nearBottom();
    await waitFor(() => expect(sections()).toHaveLength(9));
    await settle();
    userScroll(590); // unit 3's stub (section 7) at 120: Back will need unit 2 above it
    clickStub(7);
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('7'));
    let release!: () => void;
    (api.seek as ReturnType<typeof vi.fn>).mockImplementation(async (t, c, u, sig) => {
      if (u === 2) { calls.push('seek 2 (held)'); await new Promise<void>(r => { release = r; }); }
      return realSeek(t, c, u, sig);
    });
    calls.length = 0;
    fireEvent.click(pill()!);
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('3'));
    await waitFor(() => expect(calls).toEqual(['seek 3', 'seek 2 (held)'])); // waiting on unit 2
    expect(box.scrollTop).toBe(0);
    return { box, release: () => release() };
  }

  it('Low A: an in-run link followed while Back waits keeps the reader on the link target when unit 2 lands', async () => {
    const { box, release } = await backWaitingOnUnit2();
    fireEvent.click(within(sections()[0]).getByTestId('mat-builds-toggle'));
    fireEvent.click(within(sections()[0]).getByTestId('mat-builds-link')); // -> 3.3, loaded
    expect(box.scrollTop).toBe(10 + 200 - 36);
    await act(async () => { release(); });
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('2'));
    await settle();
    expect(box.scrollTop).toBe(174 + 300); // the place kept, no pull back to the origin
    expect(document.querySelector('[data-anchor="f-3-3"]')!.getBoundingClientRect().top).toBe(36);
  });

  it('Low B: the reader\'s own wheel while Back waits abandons the restore', async () => {
    const { box, release } = await backWaitingOnUnit2();
    fireEvent.wheel(box, { deltaY: 120 });
    await act(async () => { release(); });
    await waitFor(() => expect(sections()[0].dataset.fsUnit).toBe('2'));
    await settle();
    expect(box.scrollTop).toBe(0 + 300); // only the place-keeping, no restore to 290
  });
});


// ── Ribbon, sticky path and the Recent-beyond-the-FactSet notice (rulings
// 2026-11-03: book design §5 / prototype items 2 and 5; Recent beyond 500 (c)) ──
//
// Sabotage, one run each (table in the PR): (a) the unit root keeps its number
// in the heading; (b) the ribbon has a fixed width; (c) the path drops the unit
// root when it truncates; (d) the path's middle truncation drops the current
// section; (e) the notice shows whenever a list total is known.

const LONG = (k: number) => `Section ${k} with a rather long title that runs past thirty characters`;
const ROOT_LONG = 'Orchestrating many agents across one shared harness';
/** Unit 1: a root and a chain five levels deep (each section the child of the
 *  one before), then unit 2's root. */
function deepPage(): FactSetPage {
  const chain = [2, 3, 4, 5, 6].map((idx, i) => n(1, idx, LONG(idx), {
    anchor_id: `f-deep-${idx}`, tree_depth: i + 1, parent: idx - 1, number: [1, ...Array(i + 1).fill(1)].join('.'),
  }));
  return { ...env(2, 2, "D.3.0", false), facts: [n(1, 1, ROOT_LONG), ...chain, n(2, 1, 'Second unit root')] };
}
const pathItems = () => screen.queryAllByTestId('fs-path-item').map(b => b.textContent);
const ROOT28 = `${ROOT_LONG.slice(0, PATH_UNIT_CHARS - 1).trimEnd()}…`;
const clip30 = (t: string) => `${t.slice(0, NESTING.PATH_TITLE_CHARS - 1).trimEnd()}…`;
function widthOf(box: HTMLElement, w: number) { Object.defineProperty(box, 'clientWidth', { configurable: true, get: () => w }); }

describe('the unit ribbon (book design §5)', () => {
  it('width grows with the digits: 17px for 1 digit, 23px for 2, 30px for 3 (fits the 34px narrow gutter)', () => {
    expect([ribbonWidth(1), ribbonWidth(13), ribbonWidth(130)]).toEqual([16.6, 23.2, 29.8]);
    expect(Math.round(ribbonWidth(7))).toBe(17);
    expect(Math.round(ribbonWidth(130))).toBe(30);
    expect(ribbonWidth(130)).toBeLessThanOrEqual(34 - 3);
  });

  it('a rendered ribbon carries the width of its digit count, and only unit roots have one', async () => {
    const page: FactSetPage = { ...env(130, 130, "D.131.0", false), unit_from: 13, facts: [
      n(13, 1, 'Thirteen'), n(13, 2, 'Inside 13'), n(130, 1, 'One hundred thirty'),
    ] };
    const { api } = makeApi({ first: vi.fn(async () => page) });
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    await waitFor(() => expect(sections()).toHaveLength(3));
    const ribbons = screen.getAllByTestId('fs-ribbon') as HTMLElement[];
    expect(ribbons.map(r => [r.textContent, r.style.width])).toEqual([['13', '23.2px'], ['130', '29.8px']]);
    expect(within(sections()[1]).queryByTestId('fs-ribbon')).toBeNull();
    // A section inside the unit keeps its number in its heading.
    expect(within(sections()[1]).getByTestId('mat-num').textContent?.trim()).toBe('13.1');
  });

  it('CSS: the 2px unit rule is gone; the narrow gutter leaves room for "130"', () => {
    expect(materializeCss).not.toMatch(/\.k-fs-unit\s*\{[^}]*border-top:\s*2px/);
    expect(materializeCss).toMatch(/@media \(max-width: 1100px\) \{ \.k-fs-unit > \.k-mat-gut \{ padding-right: 3px; \} \}/);
  });

  it('on a lens the repo name stays on the unit root, first on the meta line under the title-only heading', async () => {
    const lensP1: FactSetPage = { ...P1, facts: [n(1, 1, 'Alpha', { repo_name: 'alpha' }), n(1, 2, 'Shared')], has_more: false };
    const { api } = makeApi({ first: vi.fn(async () => lensP1) });
    render(<FactSetView search={LENS} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    await waitFor(() => expect(sections()).toHaveLength(2));
    const root = sections()[0];
    expect(within(root).getByTestId('fs-ribbon').textContent).toBe('1');
    expect(within(root).getByTestId('mat-meta').firstElementChild).toBe(within(root).getByTestId('fs-repo-name'));
    expect(root.querySelector('.k-mat-h')!.textContent).toBe('Alpha');
  });
});

describe('the sticky section path (book design §5 / prototype item 5)', () => {
  async function openDeep(width: number) {
    const { api } = makeApi({ first: vi.fn(async () => deepPage()) });
    render(<FactSetView search={REPO} options={DEFAULT_OPTIONS} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    const box = screen.getByTestId('fs-scroll');
    widthOf(box, width);
    const userScroll = liveLayout(box);
    await waitFor(() => expect(sections()).toHaveLength(7));
    await settle();
    return userScroll;
  }

  it('at a unit root the path is the root alone, clipped to PATH_UNIT_CHARS, after "unit X of N" on the same line', async () => {
    const userScroll = await openDeep(1400);
    userScroll(0);
    const path = screen.getByTestId('fs-path');
    expect(within(path).getByTestId('fs-progress').textContent).toBe('unit 1 of 2');
    expect(pathItems()).toEqual([ROOT28]);
    expect(pathItems()[0]!.length).toBeLessThanOrEqual(PATH_UNIT_CHARS);
    expect(screen.getByTestId('fs-path-item').getAttribute('title')).toBe(ROOT_LONG);
    // The progress is not repeated in the bar.
    expect(screen.getAllByTestId('fs-progress')).toHaveLength(1);
  });

  it('at a deep section: root first, every ancestor, the section last (current), titles clipped to 30', async () => {
    const userScroll = await openDeep(1400);
    userScroll(260); // section 3 (tree depth 3) is at the top
    const items = pathItems();
    expect(items).toEqual([ROOT28, `1.1 ${clip30(LONG(2))}`, `1.1.1 ${clip30(LONG(3))}`, `1.1.1.1 ${clip30(LONG(4))}`]);
    expect(screen.getAllByTestId('fs-path-item').at(-1)!.className).toBe('cur');
    expect(screen.queryByTestId('fs-path-gap')).toBeNull();
    expect(within(screen.getByTestId('fs-path')).getByTestId('fs-progress').textContent).toBe('unit 1 of 2');
  });

  it('a narrower pane drops only the ancestors between the root and the current section, as one "…" gap', async () => {
    const userScroll = await openDeep(900); // budget (900 - 90) / 6.3 = 128 chars
    userScroll(460); // section 5 (tree depth 5) at the top: 6 items
    expect(pathItems()).toEqual([ROOT28, clip30(LONG(5)), clip30(LONG(6))]);
    const nav = screen.getByTestId('fs-path');
    const kids = Array.from(nav.querySelectorAll('[data-testid="fs-path-item"], [data-testid="fs-path-gap"]'));
    expect(kids.map(k => k.getAttribute('data-testid'))).toEqual(['fs-path-item', 'fs-path-gap', 'fs-path-item', 'fs-path-item']);
    // The narrowest pane still keeps the root and the current section.
    widthOf(screen.getByTestId('fs-scroll'), 200);
    fireEvent(window, new Event('resize'));
    await waitFor(() => expect(pathItems()).toEqual([ROOT28, clip30(LONG(6))]));
    expect(screen.getAllByTestId('fs-path-gap')).toHaveLength(1);
  });

  it('a path item scrolls to its section and records no Back origin', async () => {
    const userScroll = await openDeep(1400);
    userScroll(260);
    const box = screen.getByTestId('fs-scroll');
    fireEvent.click(screen.getAllByTestId('fs-path-item')[1]); // 1.1, section index 1
    // Section i sits at 10 + 100 i - scrollTop; a scroll puts it at PATH_H + 8 = 36.
    expect(box.scrollTop).toBe(10 + 100 - 36);
    expect(screen.queryByTestId('fs-back-pill')).toBeNull();
  });
});

describe('a Recent list longer than the FactSet (ruling 2026-11-03 (c))', () => {
  async function openWith(listTotal: number | undefined, units: number) {
    const { api } = makeApi({ first: vi.fn(async () => ({ ...env(units, 1, "D.2.0", false), facts: [n(1, 1, 'Alpha')] })) });
    render(<FactSetView search={LENS} options={DEFAULT_OPTIONS} listTotal={listTotal} onOptionsChange={() => {}} onBack={() => {}} api={api} />);
    await waitFor(() => expect(sections()).toHaveLength(1));
  }
  it('says how many of the list it shows when the list total is greater than the units', async () => {
    await openWith(1919, 500);
    expect(screen.getByTestId('fs-more-notice').textContent).toBe('Showing the first 500 of 1,919 results.');
  });
  it('says nothing when the FactSet holds the whole list', async () => {
    await openWith(500, 500);
    expect(screen.queryByTestId('fs-more-notice')).toBeNull();
  });
  it('says nothing when there is no list total (a relevance list)', async () => {
    await openWith(undefined, 500);
    expect(screen.queryByTestId('fs-more-notice')).toBeNull();
  });
});
