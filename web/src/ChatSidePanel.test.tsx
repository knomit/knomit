import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { render, screen, waitFor, cleanup, fireEvent, within, act } from '@testing-library/react';
import { useEffect, useReducer } from 'react';
import { ChatFacet } from './ChatFacet';
import { ChatSidePanel } from './ChatSidePanel';
import { clampPanelWidth, initialPanelState, panelReducer, PANEL_WIDTH_KEY, PANEL_DEFAULT_WIDTH, PANEL_MAX_FRACTION, PANEL_MIN_WIDTH, usePanelWidth } from './chatPanelState';
import type { PanelEntry, PanelState } from './chatPanelState';
import { installFakeEventSource, uninstallFakeEventSource } from './testEventSource';
import { stubFleetFetch, problem } from './fleetTestFetch';
import type { ChatSnapshot } from './chatApi';

const CHAT = { id: 'chat-a', provider: 'anthropic', model: 'claude-test-x' };
const BASE = '/api/v1/chats/chat-a';
const FACT_BASE = '/api/v1/repos/r1/branches/main/facts';

const R1: PanelEntry = { kind: 'result', callId: 'tu_1' };
const R2: PanelEntry = { kind: 'result', callId: 'tu_2' };
const F1: PanelEntry = { kind: 'fact', repo: 'r1', branch: 'main', path: 'kb/decisions/web/one.md' };

const FACT = {
  path: 'kb/decisions/web/one.md', title: 'One decision', type: 'observation', kind: 'epistemic', body: 'We chose **SSE**.',
  domain: ['web'], confidence: 0.9, sources: 2, entities: [], refs: [], commit_hash: 'abc1234', commit_date: '2026-11-01T10:00:00Z',
};

const calls = {
  tu_1: { name: 'knomit_query', arguments: { text: 'first' }, result: 'RESULT-ONE', is_error: false, elided: false },
  // A materialize as the model really sends it: `file`, `audience` and the binding handle; no repo.
  tu_2: { name: 'knomit_materialize', arguments: { file: 'kb/decisions/web/two.md', audience: 'agent', binding: 'bnd_xyz' }, result: JSON.stringify({ text: '# Two\n', has_more: false }), is_error: false, elided: false },
};

function routes() {
  return {
    [`GET ${BASE}/calls/tu_1`]: { body: calls.tu_1 },
    [`GET ${BASE}/calls/tu_2`]: { body: calls.tu_2 },
    [`GET ${FACT_BASE}/kb/decisions/web/one.md`]: { body: FACT },
    [`GET ${FACT_BASE}/kb/decisions/web/one.md/incoming`]: { body: { _embedded: { refs: [{ path: 'a.md', title: 'A', commit: 'c1' }, { path: 'b.md', title: 'B', commit: 'c2' }] } } },
    [`GET ${FACT_BASE}/kb/decisions/web/one.md/outgoing`]: { body: { _embedded: { refs: [{ path: 'c.md', title: 'C', commit: 'c3' }] } } },
    [`GET ${FACT_BASE}/kb/decisions/web/one.md/commits`]: { body: { _embedded: { commits: [{ commit: 'c9' }, { commit: 'c8' }, { commit: 'c7' }] } } },
  };
}

/** The panel with its own history, driven through the panel API (the way fact links will). */
let api: { open: (e: PanelEntry) => void; state: () => PanelState };
function Harness({ running = false, onBrowse, bound = '' }: { running?: boolean; onBrowse?: (r: string, p: string) => void; bound?: string }) {
  const [panel, dispatch] = useReducer(panelReducer, initialPanelState);
  useEffect(() => { api = { open: entry => dispatch({ type: 'open', entry }), state: () => panel }; });
  const w = usePanelWidth();
  return <ChatSidePanel chatId="chat-a" panel={panel} dispatch={dispatch} running={running} width={w.width} onSplitterDown={w.onSplitterDown} onOpenInBrowse={onBrowse} boundFor={() => bound} />;
}

beforeEach(() => { installFakeEventSource(); localStorage.clear(); });
afterEach(() => { cleanup(); uninstallFakeEventSource(); vi.unstubAllGlobals(); });

const panelW = () => screen.getByTestId('chat-side-panel').style.getPropertyValue('--k-panel-w');
const crumb = () => screen.getByTestId('chat-panel-crumb').textContent;
const footer = () => screen.getByTestId('chat-panel-footer').textContent;

describe('panelReducer', () => {
  const run = (...as: Parameters<typeof panelReducer>[1][]) => as.reduce(panelReducer, initialPanelState);
  it('is closed and empty to begin with; open pushes and shows the entry', () => {
    expect(initialPanelState).toEqual({ entries: [], index: -1, open: false });
    const s = run({ type: 'open', entry: R1 });
    expect(s).toEqual({ entries: [R1], index: 0, open: true });
  });
  it('back and forward walk mixed facts and results; opening from the middle drops what was ahead', () => {
    let s = run({ type: 'open', entry: R1 }, { type: 'open', entry: F1 }, { type: 'open', entry: R2 });
    expect(s.entries).toEqual([R1, F1, R2]);
    s = [{ type: 'back' }, { type: 'back' }].reduce((a, x) => panelReducer(a, x as never), s);
    expect(s.index).toBe(0);
    expect(panelReducer(s, { type: 'back' }).index).toBe(0); // no further back
    s = panelReducer(s, { type: 'forward' });
    expect(s.entries[s.index]).toEqual(F1);
    // From the middle, a new open replaces the future: R2 is gone.
    s = panelReducer(s, { type: 'open', entry: { kind: 'result', callId: 'tu_9' } });
    expect(s.entries).toEqual([R1, F1, { kind: 'result', callId: 'tu_9' }]);
    expect(s.index).toBe(2);
    expect(panelReducer(s, { type: 'forward' }).index).toBe(2); // nothing ahead
  });
  it('close keeps the history; reopening the same entry does not push it again', () => {
    let s = run({ type: 'open', entry: R1 }, { type: 'open', entry: F1 }, { type: 'close' });
    expect(s.open).toBe(false);
    expect(s.entries).toEqual([R1, F1]);
    s = panelReducer(s, { type: 'open', entry: F1 });
    expect(s).toEqual({ entries: [R1, F1], index: 1, open: true });
    // Opening what is showing is a no-op.
    expect(panelReducer(s, { type: 'open', entry: F1 })).toBe(s);
  });
});

describe('ChatSidePanel', () => {
  it('renders nothing until something is opened', () => {
    stubFleetFetch(routes());
    render(<Harness />);
    expect(screen.queryByTestId('chat-side-panel')).toBeNull();
  });

  it('a fact shows Browse\'s band (type, confidence, sources, path, cited by / cites, version) and the body', async () => {
    stubFleetFetch(routes());
    render(<Harness />);
    act(() => api.open(F1));
    await screen.findByTestId('chat-fact-title');
    expect(screen.getByTestId('chat-fact-title')).toHaveTextContent('One decision');
    expect(screen.getByTestId('fact-type-badge').textContent?.trim()).toBe('observation');
    expect(screen.getByTestId('fact-confidence').textContent?.replace(/\s+/g, ' ').trim()).toBe('conf 0.90');
    expect(screen.getByTestId('fact-meta').textContent?.replace(/\s+/g, ' ').trim()).toBe('observation·conf 0.90·2 sources');
    expect(screen.getByTestId('fact-band-path')).toHaveTextContent('kb/decisions/web/one.md');
    await waitFor(() => expect(screen.getByTestId('chat-fact-cited-by')).toHaveTextContent('cited by 2'));
    expect(screen.getByTestId('chat-fact-cites')).toHaveTextContent('cites 1');
    await waitFor(() => expect(screen.getByTestId('chat-fact-version')).toHaveTextContent('v3'));
    expect(screen.getByTestId('fact-body').querySelector('strong')).toHaveTextContent('SSE');
    expect(crumb()).toBe('r1 / kb / decisions / web');
  });

  it('a tool result has Result and Arguments tabs, not two stacked blocks', async () => {
    stubFleetFetch(routes());
    render(<Harness />);
    act(() => api.open(R1));
    expect(await screen.findByTestId('chat-result-text')).toHaveTextContent('RESULT-ONE');
    expect(screen.getByTestId('chat-result-tab-result')).toHaveAttribute('aria-selected', 'true');
    expect(screen.getByTestId('chat-result-tab-result')).toHaveTextContent('Result');
    expect(screen.getByTestId('chat-result-tab-arguments')).toHaveTextContent('Arguments');
    expect(screen.queryByTestId('chat-result-args')).toBeNull();
    fireEvent.click(screen.getByTestId('chat-result-tab-arguments'));
    expect(screen.getByTestId('chat-result-args')).toHaveTextContent('"text": "first"');
    expect(screen.queryByTestId('chat-result-text')).toBeNull();
    expect(screen.getByTestId('chat-result-tab-arguments')).toHaveAttribute('aria-selected', 'true');
    fireEvent.click(screen.getByTestId('chat-result-tab-result'));
    expect(screen.getByTestId('chat-result-text')).toHaveTextContent('RESULT-ONE');
  });

  it('draws Esc and the back key as keycaps and right-aligns the count (mockup .kbdh)', async () => {
    stubFleetFetch(routes());
    render(<Harness bound="r1" />);
    act(() => api.open(R1));
    await screen.findByTestId('chat-result-text');
    const f = screen.getByTestId('chat-panel-footer');
    const caps = [...f.querySelectorAll('span')].filter(e => e.style.border.includes('solid')).map(e => e.textContent);
    expect(caps).toEqual(['Esc', '\u2318[']);
    const count = [...f.querySelectorAll('span')].find(e => e.textContent === '1 of 1 opened')!;
    expect(count.style.marginLeft).toBe('auto');
    // The header arrows are the mockup's 26px buttons holding a 13px stroke icon.
    const back = screen.getByTestId('chat-panel-back');
    expect(back.style.width).toBe('26px');
    expect(back.querySelector('svg')).toHaveAttribute('width', '13');
  });

  it('back and forward move across a mixed history, with the footer counting N of M', async () => {
    stubFleetFetch(routes());
    render(<Harness bound="r1" />);
    act(() => api.open(R1));
    await screen.findByTestId('chat-result-text');
    expect(footer()).toBe('Esc close · ⌘[ back · 1 of 1 opened');
    expect(screen.getByTestId('chat-panel-back')).toBeDisabled();
    act(() => api.open(F1));
    await screen.findByTestId('chat-fact-title');
    act(() => api.open(R2));
    await screen.findByTestId('chat-result-fact');
    expect(footer()).toBe('Esc close · ⌘[ back · 3 of 3 opened');
    expect(screen.getByTestId('chat-panel-forward')).toBeDisabled();
    // The result's breadcrumb comes from its arguments.
    await waitFor(() => expect(crumb()).toBe('r1 / kb / decisions / web'));

    fireEvent.click(screen.getByTestId('chat-panel-back'));
    await screen.findByTestId('chat-fact-title');
    expect(footer()).toBe('Esc close · ⌘[ back · 2 of 3 opened');
    fireEvent.click(screen.getByTestId('chat-panel-back'));
    expect(await screen.findByTestId('chat-result-text')).toHaveTextContent('RESULT-ONE');
    expect(footer()).toBe('Esc close · ⌘[ back · 1 of 3 opened');
    fireEvent.click(screen.getByTestId('chat-panel-forward'));
    await screen.findByTestId('chat-fact-title');
    expect(footer()).toBe('Esc close · ⌘[ back · 2 of 3 opened');
  });

  it('⌘[ goes back, from anywhere, and the browser\'s own history key is suppressed', async () => {
    stubFleetFetch(routes());
    render(<Harness />);
    act(() => api.open(R1));
    await screen.findByTestId('chat-result-text');
    act(() => api.open(F1));
    await screen.findByTestId('chat-fact-title');
    const ev = new KeyboardEvent('keydown', { key: '[', metaKey: true, bubbles: true, cancelable: true });
    act(() => { document.body.dispatchEvent(ev); });
    expect(ev.defaultPrevented).toBe(true);
    expect(await screen.findByTestId('chat-result-text')).toHaveTextContent('RESULT-ONE');
    expect(footer()).toBe('Esc close · ⌘[ back · 1 of 2 opened');
    // A bare [ does nothing.
    fireEvent.keyDown(document.body, { key: '[' });
    expect(footer()).toBe('Esc close · ⌘[ back · 1 of 2 opened');
  });

  it('× closes it; Open in Browse names the repo and the fact path, for a fact and for a materialize result', async () => {
    stubFleetFetch(routes());
    const onBrowse = vi.fn();
    render(<Harness bound="r1" onBrowse={onBrowse} />);
    act(() => api.open(F1));
    await screen.findByTestId('chat-fact-title');
    fireEvent.click(screen.getByTestId('chat-panel-browse'));
    expect(onBrowse).toHaveBeenLastCalledWith('r1', 'kb/decisions/web/one.md');
    act(() => api.open(R2));
    await screen.findByTestId('chat-result-fact');
    await waitFor(() => expect(screen.getByTestId('chat-panel-browse')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('chat-panel-browse'));
    expect(onBrowse).toHaveBeenLastCalledWith('r1', 'kb/decisions/web/two.md');
    expect(screen.getByTestId('chat-panel-browse')).toHaveTextContent('Open in Browse');
    fireEvent.click(screen.getByTestId('chat-panel-close'));
    expect(screen.queryByTestId('chat-side-panel')).toBeNull();
    expect(api.state().entries).toEqual([F1, R2]);
  });

  it('is 470px wide by default and remembers a dragged width', async () => {
    stubFleetFetch(routes());
    expect(PANEL_DEFAULT_WIDTH).toBe(470);
    const first = render(<Harness />);
    act(() => api.open(R1));
    await screen.findByTestId('chat-result-text');
    expect(panelW()).toBe('470px');
    // Dragging the splitter left by 60px widens it by 60px, and the width is stored on release.
    const splitter = screen.getByTestId('chat-panel-splitter');
    fireEvent.mouseDown(splitter, { clientX: 1000 });
    fireEvent.mouseMove(document, { clientX: 940 });
    expect(panelW()).toBe('530px');
    expect(localStorage.getItem(PANEL_WIDTH_KEY)).toBeNull(); // not on every frame
    fireEvent.mouseUp(document);
    expect(localStorage.getItem(PANEL_WIDTH_KEY)).toBe('530');
    first.unmount();
    // A fresh mount starts at the remembered width.
    render(<Harness />);
    act(() => api.open(R1));
    await screen.findByTestId('chat-result-text');
    expect(panelW()).toBe('530px');
  });
});

// ── in the chat page ──

const snap = (over: Partial<ChatSnapshot> = {}): ChatSnapshot => ({
  ...CHAT, running: false, grants: [], pending_approvals: [],
  items: [
    { kind: 'user', text: 'what repos are there?' },
    { kind: 'tool_call', id: 'tu_1', name: 'knomit_query', state: 'done' },
    { kind: 'tool_call', id: 'tu_2', name: 'knomit_materialize', state: 'done' },
    { kind: 'assistant', text: 'There is **one** repo.' },
  ],
  ...over,
});

function mountChat(snapshot: ChatSnapshot) {
  const fx = stubFleetFetch({
    ...routes(),
    [`GET ${BASE}`]: { body: snapshot },
    [`POST ${BASE}/cancel`]: { body: {} },
  });
  render(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} />);
  return fx;
}
const cancels = (fx: ReturnType<typeof stubFleetFetch>) => fx.calls.filter(c => c.route === `POST ${BASE}/cancel`);
async function openStep(id: string) {
  await screen.findByTestId('chat-turn-0');
  if (!screen.queryByTestId(`chat-tool-select-${id}`)) fireEvent.click(screen.getByTestId('chat-tool-group-toggle'));
  fireEvent.click(screen.getByTestId(`chat-tool-select-${id}`));
}

describe('the side panel in the chat page', () => {
  it('is absent until a step is clicked, then opens beside the chat; the clicked step stays highlighted', async () => {
    mountChat(snap());
    await screen.findByTestId('chat-turn-0');
    expect(screen.queryByTestId('chat-side-panel')).toBeNull();
    expect(screen.getByTestId('chat-columns').querySelector('.k-chat-result')).toBeNull();
    await openStep('tu_1');
    expect(await screen.findByTestId('chat-result-text')).toHaveTextContent('RESULT-ONE');
    expect(screen.getByTestId('chat-tool-select-tu_1')).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByTestId('chat-tool-select-tu_2')).toHaveAttribute('aria-pressed', 'false');
    expect(panelW()).toBe('470px');
    // Another step moves the highlight and the panel; the history now has two.
    fireEvent.click(screen.getByTestId('chat-tool-select-tu_2'));
    await screen.findByTestId('chat-result-fact');
    expect(screen.getByTestId('chat-tool-select-tu_2')).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByTestId('chat-tool-select-tu_1')).toHaveAttribute('aria-pressed', 'false');
    expect(footer()).toBe('Esc close · ⌘[ back · 2 of 2 opened');
  });

  it('the chat column reflows beside the panel instead of being covered', async () => {
    mountChat(snap());
    await openStep('tu_1');
    await screen.findByTestId('chat-side-panel');
    const columns = screen.getByTestId('chat-columns');
    const main = columns.querySelector('.k-chat-main') as HTMLElement;
    const panel = screen.getByTestId('chat-side-panel');
    // Siblings in one flex row, the chat first, the panel after the splitter; the chat is still there.
    expect(panel.parentElement).toBe(columns);
    expect(main.parentElement).toBe(columns);
    expect(main).toContainElement(screen.getByTestId('chat-panel'));
    expect(main.getAttribute('data-panel')).toBe('open');
    expect(main.className).toContain('k-chat-main--narrow');
    expect(main.compareDocumentPosition(panel) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    // Nothing positions the panel over the column: the stylesheet gives the column
    // the leftover width and the panel a fixed share of the row.
    const css = readFileSync(join(process.cwd(), 'src/App.css'), 'utf8');
    const rule = (sel: string) => css.split('\n').find(l => l.startsWith(`${sel} {`)) ?? '';
    expect(rule('.k-chat-main')).toContain('flex: 1 1 0');
    expect(rule('.k-chat-main')).toContain('min-width: 0');
    expect(rule('.k-chat-result')).toContain('flex-shrink: 0');
    expect(rule('.k-chat-result')).not.toMatch(/position:\s*(absolute|fixed)/);
    // At a phone width the panel stacks above the chat at full width: the later
    // base rule's max-width (60%) would otherwise win (seen in the browser: 225px of 375).
    const base = css.indexOf('.k-chat-result { width: var(--k-panel-w, 470px); max-width: 60%;');
    const narrow = css.indexOf('.k-chat-result { width: 100%; max-width: 100%;');
    expect(base).toBeGreaterThan(-1);
    expect(narrow).toBeGreaterThan(base); // after the base rule, so it wins by order
    expect(css.slice(css.indexOf('/* The chat: a centered column'), css.indexOf('.k-chat-user {'))).not.toContain('!important');
    // One cap: the stylesheet's max-width is the constant the stored width is clamped to.
    expect(Number(/\.k-chat-result \{[^}]*max-width: (\d+)%/.exec(css)![1])).toBe(PANEL_MAX_FRACTION * 100);
    // Fact links in tables cap at 150px in the narrowed column, and an open one is marked.
    expect(css).toMatch(/\.k-chat-main--narrow table \.fp \{ max-width: 150px; \}/);
    expect(css).toMatch(/\.fp\.open \{ border-color: #6ea8fe; \}/);
    // Closing it gives the column back its marker.
    fireEvent.click(screen.getByTestId('chat-panel-close'));
    expect(main.getAttribute('data-panel')).toBe('closed');
    expect(main.className).not.toContain('narrow');
  });

  it('× closes it, and the step is no longer highlighted', async () => {
    mountChat(snap());
    await openStep('tu_1');
    await screen.findByTestId('chat-result-text');
    fireEvent.click(screen.getByTestId('chat-panel-close'));
    expect(screen.queryByTestId('chat-side-panel')).toBeNull();
    expect(screen.getByTestId('chat-tool-select-tu_1')).toHaveAttribute('aria-pressed', 'false');
  });

  it('Esc closes it when nothing is running and the composer is not focused', async () => {
    const fx = mountChat(snap());
    await openStep('tu_1');
    await screen.findByTestId('chat-result-text');
    fireEvent.keyDown(document.body, { key: 'Escape' });
    expect(screen.queryByTestId('chat-side-panel')).toBeNull();
    expect(cancels(fx)).toHaveLength(0);
  });

  it('Esc in the composer does not close it', async () => {
    const fx = mountChat(snap());
    await openStep('tu_1');
    await screen.findByTestId('chat-result-text');
    const input = screen.getByTestId('chat-input');
    input.focus();
    fireEvent.keyDown(input, { key: 'Escape' });
    expect(screen.getByTestId('chat-side-panel')).toBeInTheDocument();
    expect(cancels(fx)).toHaveLength(0);
  });

  it('Esc while a turn runs is Stop and leaves the panel open; the next Esc closes it', async () => {
    const fx = mountChat(snap({ running: true, items: [
      { kind: 'user', text: 'q' },
      { kind: 'tool_call', id: 'tu_1', name: 'knomit_query', state: 'done' },
    ] }));
    await screen.findByTestId('chat-stop');
    await openStep('tu_1');
    await screen.findByTestId('chat-result-text');
    fireEvent.keyDown(document.body, { key: 'Escape' });
    await waitFor(() => expect(cancels(fx)).toHaveLength(1));
    expect(screen.getByTestId('chat-side-panel')).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByTestId('chat-stop')).toBeNull());
    // The turn has ended: now Esc is the panel's.
    fireEvent.keyDown(document.body, { key: 'Escape' });
    expect(screen.queryByTestId('chat-side-panel')).toBeNull();
    expect(cancels(fx)).toHaveLength(1);
  });

  it('a hidden chat (Settings tab) ignores Esc', async () => {
    stubFleetFetch({ ...routes(), [`GET ${BASE}`]: { body: snap() } });
    const { rerender } = render(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} />);
    await openStep('tu_1');
    await screen.findByTestId('chat-result-text');
    rerender(<ChatFacet chat={CHAT} facet="settings" onGone={vi.fn()} />);
    fireEvent.keyDown(document.body, { key: 'Escape' });
    expect(within(screen.getByTestId('chat-facet')).getByTestId('chat-side-panel')).toBeInTheDocument();
  });
});

describe('the side panel: target, width clamp, errors, keys', () => {
  it('Open in Browse for a real materialize (file + audience + binding, no repo) uses the bound target', async () => {
    stubFleetFetch(routes());
    const onBrowse = vi.fn();
    render(<Harness bound="r1" onBrowse={onBrowse} />);
    act(() => api.open(R2));
    await screen.findByTestId('chat-result-fact');
    await waitFor(() => expect(screen.getByTestId('chat-panel-browse')).toBeInTheDocument());
    expect(crumb()).toBe('r1 / kb / decisions / web');
    fireEvent.click(screen.getByTestId('chat-panel-browse'));
    expect(onBrowse).toHaveBeenCalledTimes(1);
    expect(onBrowse).toHaveBeenLastCalledWith('r1', 'kb/decisions/web/two.md');
  });

  it('a kb://<store>/ file is passed whole, and its breadcrumb drops the store', async () => {
    stubFleetFetch({ ...routes(), [`GET ${BASE}/calls/tu_3`]: { body: { ...calls.tu_2, arguments: { file: 'kb://abc123def456/kb/x/y/z.md', audience: 'agent' } } } });
    const onBrowse = vi.fn();
    render(<Harness bound="all" onBrowse={onBrowse} />);
    act(() => api.open({ kind: 'result', callId: 'tu_3' }));
    await waitFor(() => expect(screen.getByTestId('chat-panel-browse')).toBeInTheDocument());
    expect(crumb()).toBe('all / kb / x / y');
    fireEvent.click(screen.getByTestId('chat-panel-browse'));
    expect(onBrowse).toHaveBeenLastCalledWith('all', 'kb://abc123def456/kb/x/y/z.md');
  });

  it('a bind result is crumbed by the target it names (name, like lens/repo), not the binding in force before it', async () => {
    stubFleetFetch({ [`GET ${BASE}/calls/tu_b`]: { body: { name: 'knomit_bind', arguments: { name: 'B' }, result: 'ok', is_error: false, elided: false } } });
    render(<Harness bound="A" />); // the binding in force BEFORE the call is A
    act(() => api.open({ kind: 'result', callId: 'tu_b' }));
    await screen.findByTestId('chat-result-text');
    expect(crumb()).toBe('B / knomit: Knomit bind');
  });

  it('the footer items are the mockup items: keycap, word, keycap, word, count, with no extra flex items for the separators', async () => {
    stubFleetFetch(routes());
    render(<Harness bound="r1" />);
    act(() => api.open(R1));
    await screen.findByTestId('chat-result-text');
    const f = screen.getByTestId('chat-panel-footer');
    expect(f.children).toHaveLength(5);
    expect(f.textContent).toBe('Esc close \u00b7 \u2318[ back \u00b7 1 of 1 opened');
  });

  it('a result with neither a target nor a binding offers no Open in Browse', async () => {
    stubFleetFetch(routes());
    render(<Harness bound="" onBrowse={vi.fn()} />);
    act(() => api.open(R2));
    await screen.findByTestId('chat-result-fact');
    expect(screen.queryByTestId('chat-panel-browse')).toBeNull();
    expect(crumb()).toBe('knomit: Knomit materialize / kb / decisions / web');
  });

  it('clampPanelWidth: the minimum, the share of the container, and no bound without a container', () => {
    expect(PANEL_MIN_WIDTH).toBe(320);
    expect(PANEL_MAX_FRACTION).toBe(0.6);
    expect(clampPanelWidth(100, 1000)).toBe(320);
    expect(clampPanelWidth(450, 1000)).toBe(450);
    expect(clampPanelWidth(900, 1000)).toBe(600);
    expect(clampPanelWidth(900, 400)).toBe(320); // a small container never goes below the minimum
    expect(clampPanelWidth(900, 0)).toBe(900);
  });

  it('a drag is clamped to 60% of the chat body, and starts from the width shown, not the stored one', async () => {
    stubFleetFetch(routes());
    localStorage.setItem(PANEL_WIDTH_KEY, '864');
    render(<Harness />);
    act(() => api.open(R1));
    await screen.findByTestId('chat-result-text');
    const splitter = screen.getByTestId('chat-panel-splitter');
    const panel = screen.getByTestId('chat-side-panel');
    Object.defineProperty(splitter.parentElement!, 'clientWidth', { configurable: true, value: 1000 });
    // The stylesheet is showing 600 although 864 is stored: dragging 50px right narrows from 600, not 864.
    Object.defineProperty(panel, 'offsetWidth', { configurable: true, value: 600 });
    fireEvent.mouseDown(splitter, { clientX: 1000 });
    fireEvent.mouseMove(document, { clientX: 1050 });
    expect(panelW()).toBe('550px');
    // Far to the left: at most 60% of 1000.
    fireEvent.mouseMove(document, { clientX: -2000 });
    expect(panelW()).toBe('600px');
    // Far to the right: at least the minimum.
    fireEvent.mouseMove(document, { clientX: 5000 });
    expect(panelW()).toBe('320px');
    fireEvent.mouseUp(document);
    expect(localStorage.getItem(PANEL_WIDTH_KEY)).toBe('320');
  });

  it('a fact that cannot be read shows the server\'s sentence in the blocked notice style, not the request URL', async () => {
    stubFleetFetch({
      ...routes(),
      [`GET ${FACT_BASE}/kb/gone.md`]: problem(404, 'not_found', 'no such fact'),
    });
    render(<Harness />);
    act(() => api.open({ kind: 'fact', repo: 'r1', branch: 'main', path: 'kb/gone.md' }));
    const err = await screen.findByTestId('chat-fact-error');
    expect(err.textContent).toBe('no such fact');
    expect(err.getAttribute('data-tone')).toBe('blocked');
    expect(err.textContent).not.toContain('/api/v1');
    expect(err.getAttribute('title')).toBeNull();
    expect(screen.getByRole('alert')).toContainElement(err);
    expect(screen.queryByTestId('chat-fact')).toBeNull();
  });

  it('Esc while a turn runs and focus is inside the side panel is Stop too, and the panel stays', async () => {
    const fx = stubFleetFetch({
      ...routes(),
      [`GET ${BASE}`]: { body: snap({ running: true, items: [{ kind: 'user', text: 'q' }, { kind: 'tool_call', id: 'tu_1', name: 'knomit_query', state: 'done' }] }) },
      [`POST ${BASE}/cancel`]: { body: {} },
    });
    render(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} />);
    await screen.findByTestId('chat-stop');
    await openStep('tu_1');
    await screen.findByTestId('chat-result-text');
    const tab = screen.getByTestId('chat-result-tab-arguments');
    tab.focus();
    expect(document.activeElement).toBe(tab);
    fireEvent.keyDown(tab, { key: 'Escape' });
    await waitFor(() => expect(cancels(fx)).toHaveLength(1));
    expect(screen.getByTestId('chat-side-panel')).toBeInTheDocument();
  });
});
