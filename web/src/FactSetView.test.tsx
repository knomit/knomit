import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { FactSetView, NOTICE_REFRESHED } from './FactSetView';
import { CHANGED_SINCE_CITED, TIP_DIFFERENT_VERSION, TIP_RULE1 } from './materializeParts';
import { DEFAULT_OPTIONS } from './materializeApi';
import { FsError } from './factsetApi';
import type { FactSetApi, FactSetLink, FactSetNode, FactSetPage, FsSearch } from './factsetApi';
import type { SaveOutcome } from './saveFile';

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
  it('units: numbered root headings with their own rule; server numbers; tree-depth ladder', async () => {
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
    expect(roots.map(s => within(s).getByTestId('mat-num').textContent?.trim())).toEqual(['1.', '2.']);
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
