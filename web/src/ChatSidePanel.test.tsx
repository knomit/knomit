import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { render, screen, waitFor, cleanup, fireEvent, within, act } from '@testing-library/react';
import { useEffect, useReducer } from 'react';
import { ChatFacet } from './ChatFacet';
import { ChatSidePanel } from './ChatSidePanel';
import { clampPanelWidth, initialPanelState, panelReducer, PANEL_WIDTH_KEY, PANEL_DEFAULT_WIDTH, PANEL_MAX_FRACTION, PANEL_MIN_WIDTH, usePanelWidth } from './chatPanelState';
import type { BrowseFactRequest, PanelEntry, PanelState } from './chatPanelState';
import { installFakeEventSource, uninstallFakeEventSource } from './testEventSource';
import { stubFleetFetch, problem } from './fleetTestFetch';
import type { ChatSnapshot } from './chatApi';
import { ChatToolGroup } from './ChatToolGroup';
import type { ToolCall } from './chatState';
import sidePanelSrc from './ChatSidePanel.tsx?raw';
import resultPanelSrc from './ChatResultPanel.tsx?raw';

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
  tu_2: { name: 'knomit_materialize', title: 'Materialize', arguments: { file: 'kb/decisions/web/two.md', audience: 'agent', binding: 'bnd_xyz' }, result: JSON.stringify({ text: '# Two\n', has_more: false }), is_error: false, elided: false },
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
function Harness({ running = false, onBrowse, bound = '', callFor }: { running?: boolean; onBrowse?: (r: BrowseFactRequest) => void; bound?: string; callFor?: (id: string) => ToolCall | undefined }) {
  const [panel, dispatch] = useReducer(panelReducer, initialPanelState);
  useEffect(() => { api = { open: entry => dispatch({ type: 'open', entry }), state: () => panel }; });
  const w = usePanelWidth();
  return <ChatSidePanel chatId="chat-a" panel={panel} dispatch={dispatch} running={running} width={w.width} onSplitterDown={w.onSplitterDown} onBrowse={onBrowse} boundFor={() => bound} callFor={callFor} />;
}

beforeEach(() => { installFakeEventSource(); localStorage.clear(); });
afterEach(() => { cleanup(); uninstallFakeEventSource(); vi.unstubAllGlobals(); });

const panelW = () => screen.getByTestId('chat-side-panel').style.getPropertyValue('--k-panel-w');
const crumb = () => screen.getByTestId('chat-panel-crumb').textContent;
const footer = () => screen.getByTestId('chat-panel-footer').textContent;

describe('panelReducer', () => {
  const run = (...as: Parameters<typeof panelReducer>[1][]) => as.reduce(panelReducer, initialPanelState);
  it('is closed to begin with; open shows the entry, a second replaces it, close empties it', () => {
    expect(initialPanelState).toEqual({ entry: null });
    expect(run({ type: 'open', entry: R1 })).toEqual({ entry: R1 });
    expect(run({ type: 'open', entry: R1 }, { type: 'open', entry: F1 })).toEqual({ entry: F1 });
    expect(run({ type: 'open', entry: R1 }, { type: 'close' })).toEqual({ entry: null });
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
    expect(await screen.findByTestId('fact-title')).toHaveTextContent('One decision');
    expect(screen.getByTestId('fact-type-badge').textContent?.trim()).toBe('observation');
    expect(screen.getByTestId('fact-confidence').textContent?.replace(/\s+/g, ' ').trim()).toBe('conf 0.90');
    expect(screen.getByTestId('fact-meta').textContent?.replace(/\s+/g, ' ').trim()).toBe('observation·conf 0.90·2 sources');
    expect(screen.getByTestId('fact-band-path')).toHaveTextContent('kb/decisions/web/one.md');
    await waitFor(() => expect(screen.getByTestId('connections-in')).toHaveTextContent('2'));
    expect(screen.getByTestId('connections-out')).toHaveTextContent('1');
    // The history button counts nothing: a bare history icon, named by its aria-label.
    await waitFor(() => expect(screen.getByTestId('version-walker').textContent).toBe(''));
    expect(screen.getByTestId('version-walker')).toHaveAccessibleName('Open history');
    expect(screen.getByTestId('fact-body').querySelector('strong')).toHaveTextContent('SSE');
    // The band is the chrome: there is no row above it, and the × sits in the
    // panel's top-right corner, not in the band's actions beside history.
    expect(screen.queryByTestId('chat-panel-header')).toBeNull();
    expect(within(screen.getByTestId('fact-corner-action')).getByTestId('chat-panel-close')).toBeInTheDocument();
    expect(within(screen.getByTestId('fact-band-actions')).queryByTestId('chat-panel-close')).toBeNull();
    expect(screen.queryByTestId('retract-btn')).toBeNull();
  });

  // SABOTAGE: drop the window keydown listener, or the corner × -> red.
  it('a fact\'s corner × closes the panel, and so does Esc', async () => {
    stubFleetFetch(routes());
    const { unmount } = render(<Harness />);
    act(() => api.open(F1));
    await screen.findByTestId('fact-title');
    fireEvent.click(within(screen.getByTestId('fact-corner-action')).getByTestId('chat-panel-close'));
    expect(screen.queryByTestId('chat-side-panel')).toBeNull();
    act(() => api.open(F1));
    await screen.findByTestId('fact-title');
    expect(screen.getByTestId('chat-side-panel')).toBeInTheDocument();
    fireEvent.keyDown(document.body, { key: 'Escape' });
    expect(screen.queryByTestId('chat-side-panel')).toBeNull();
    unmount();
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

  it('draws Esc as a keycap, and has no back, forward, back key or count (mockup .kbdh)', async () => {
    stubFleetFetch(routes());
    render(<Harness bound="r1" />);
    act(() => api.open(R1));
    await screen.findByTestId('chat-result-text');
    const f = screen.getByTestId('chat-panel-footer');
    expect([...f.querySelectorAll('span')].filter(e => e.style.border.includes('solid')).map(e => e.textContent)).toEqual(['Esc']);
    expect(footer()).toBe('Esc close');
    expect(screen.queryByTestId('chat-panel-back')).toBeNull();
    expect(screen.queryByTestId('chat-panel-forward')).toBeNull();
    // The close is the mockup's 26px button holding a 13px stroke icon, in the header.
    const close = within(screen.getByTestId('chat-panel-header')).getByTestId('chat-panel-close');
    expect(close.style.width).toBe('26px');
    expect(close.querySelector('svg')).toHaveAttribute('width', '13');
  });

  it('⌘[ is no longer the panel\'s: it does not touch the panel or take the key', async () => {
    stubFleetFetch(routes());
    render(<Harness />);
    act(() => api.open(R1));
    await screen.findByTestId('chat-result-text');
    const ev = new KeyboardEvent('keydown', { key: '[', metaKey: true, bubbles: true, cancelable: true });
    act(() => { document.body.dispatchEvent(ev); });
    expect(ev.defaultPrevented).toBe(false);
    expect(screen.getByTestId('chat-result-text')).toBeInTheDocument();
  });

  it('a new entry replaces what was on screen; × closes it', async () => {
    stubFleetFetch(routes());
    render(<Harness bound="r1" />);
    act(() => api.open(F1));
    await screen.findByTestId('fact-title');
    act(() => api.open(R2));
    await screen.findByTestId('chat-result-fact');
    expect(screen.queryByTestId('fact-title')).toBeNull();
    await waitFor(() => expect(crumb()).toBe('r1 / kb / decisions / web'));
    fireEvent.click(screen.getByTestId('chat-panel-close'));
    expect(screen.queryByTestId('chat-side-panel')).toBeNull();
  });

  it('the panel has no Open in Browse, on a fact or on a materialize result', async () => {
    stubFleetFetch(routes());
    render(<Harness bound="r1" onBrowse={vi.fn()} />);
    act(() => api.open(F1));
    await screen.findByTestId('fact-title');
    expect(screen.queryByTestId('chat-panel-browse')).toBeNull();
    act(() => api.open(R2));
    await screen.findByTestId('chat-result-fact');
    await waitFor(() => expect(crumb()).toBe('r1 / kb / decisions / web'));
    expect(screen.queryByTestId('chat-panel-browse')).toBeNull();
    expect(document.body.textContent).not.toContain('Open in Browse');
  });

  it('the band\'s controls send the reader to Browse: the version, and a pick from cited by or cites (their menus open here)', async () => {
    stubFleetFetch(routes());
    const onBrowse = vi.fn();
    render(<Harness onBrowse={onBrowse} />);
    act(() => api.open(F1));
    await screen.findByTestId('fact-title');
    const base = { repo: 'r1', branch: 'main', path: 'kb/decisions/web/one.md' };
    fireEvent.click(await screen.findByTestId('version-walker'));
    expect(onBrowse).toHaveBeenLastCalledWith({ ...base, commit: 'c9' }); // Browse's walker opens the newest version
    await waitFor(() => expect(screen.getByTestId('connections-in')).toHaveTextContent('2'));
    onBrowse.mockClear();
    fireEvent.click(screen.getByTestId('connections-in'));
    expect(onBrowse).not.toHaveBeenCalled();
    fireEvent.click(within(screen.getByTestId('connections-panel')).getByText('B'));
    expect(onBrowse).toHaveBeenLastCalledWith({ ...base, path: 'b.md', commit: 'c2' });
    fireEvent.click(screen.getByTestId('connections-out'));
    fireEvent.click(within(screen.getByTestId('connections-panel')).getByText('C'));
    expect(onBrowse).toHaveBeenLastCalledWith({ ...base, path: 'c.md', commit: 'c3' });
  });

  it('the title flows up into the band once it has scrolled away, with Browse\'s own observer', async () => {
    const seen: ((e: { isIntersecting: boolean }[]) => void)[] = [];
    vi.stubGlobal('IntersectionObserver', class { constructor(cb: (e: { isIntersecting: boolean }[]) => void) { seen.push(cb); } observe() {} disconnect() {} });
    stubFleetFetch(routes());
    render(<Harness />);
    act(() => api.open(F1));
    await screen.findByTestId('fact-title');
    expect(screen.queryByTestId('fact-band-title')).toBeNull();
    expect(screen.getByTestId('fact-band-path')).toBeInTheDocument();
    // The observer is built in a passive effect, which may run after the title is found.
    await waitFor(() => expect(seen.length).toBeGreaterThan(0));
    act(() => seen[seen.length - 1]([{ isIntersecting: false }]));
    expect(screen.getByTestId('fact-band-title')).toHaveTextContent('One decision');
    expect(screen.queryByTestId('fact-band-path')).toBeNull();
  });

  it('materialize is Browse\'s control and view, at the commit the fact was read at, and its back returns to the fact', async () => {
    const FULL = 'abc1234' + 'f'.repeat(33);
    const AT = '/api/v1/repos/r1/branches/main/commits/' + FULL + '/facts/kb/decisions/web/one.md';
    // An anchored read that did not fall back answers with the anchor as its
    // commit (readFact: head = the anchor), as the server does.
    stubFleetFetch({ ...routes(), [`GET ${AT}`]: { body: { ...FACT, commit_hash: FULL } } });
    render(<Harness />);
    act(() => api.open({ ...F1, commit: FULL } as PanelEntry));
    await screen.findByTestId('fact-title');
    fireEvent.click(screen.getByTestId('materialize-btn'));
    expect(await screen.findByTestId('materialize-view')).toBeInTheDocument();
    await waitFor(() => expect((fetch as unknown as { mock: { calls: unknown[][] } }).mock.calls.some(c => String(c[0]).includes(`/commits/${FULL}/facts/kb/decisions/web/one.md/materialize`))).toBe(true));
    fireEvent.click(screen.getByTestId('mat-back'));
    expect(await screen.findByTestId('fact-title')).toBeInTheDocument();
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
    // Another step moves the highlight and the panel.
    fireEvent.click(screen.getByTestId('chat-tool-select-tu_2'));
    await screen.findByTestId('chat-result-fact');
    expect(screen.getByTestId('chat-tool-select-tu_2')).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByTestId('chat-tool-select-tu_1')).toHaveAttribute('aria-pressed', 'false');
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
  it('a materialize result (file + audience + binding, no repo) is crumbed by the bound target and its path', async () => {
    stubFleetFetch(routes());
    render(<Harness bound="r1" />);
    act(() => api.open(R2));
    await screen.findByTestId('chat-result-fact');
    await waitFor(() => expect(crumb()).toBe('r1 / kb / decisions / web'));
  });

  it('a kb://<store>/ file\'s breadcrumb drops the store', async () => {
    stubFleetFetch({ ...routes(), [`GET ${BASE}/calls/tu_3`]: { body: { ...calls.tu_2, arguments: { file: 'kb://abc123def456/kb/x/y/z.md', audience: 'agent' } } } });
    render(<Harness bound="all" />);
    act(() => api.open({ kind: 'result', callId: 'tu_3' }));
    await waitFor(() => expect(crumb()).toBe('all / kb / x / y'));
  });

  it('a bind result is crumbed by the target it names (name, like lens/repo), not the binding in force before it', async () => {
    // The call endpoint carries the title too, for a call the transcript no longer holds.
    stubFleetFetch({ [`GET ${BASE}/calls/tu_b`]: { body: { name: 'knomit_bind', title: 'Bind', key_arg: 'B', arguments: { name: 'B' }, result: 'ok', is_error: false, elided: false } } });
    render(<Harness bound="A" />); // the binding in force BEFORE the call is A
    act(() => api.open({ kind: 'result', callId: 'tu_b' }));
    await screen.findByTestId('chat-result-text');
    expect(crumb()).toBe('B / Bind');
  });

  it('a result with neither a target nor a binding is crumbed by its label', async () => {
    stubFleetFetch(routes());
    render(<Harness bound="" />);
    act(() => api.open(R2));
    await screen.findByTestId('chat-result-fact');
    await waitFor(() => expect(crumb()).toBe('Materialize / kb / decisions / web'));
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

  it('a fact that cannot be read shows Browse\'s error, with the × still there', async () => {
    stubFleetFetch({
      ...routes(),
      [`GET ${FACT_BASE}/kb/gone.md`]: problem(404, 'not_found', 'no such fact'),
    });
    render(<Harness />);
    act(() => api.open({ kind: 'fact', repo: 'r1', branch: 'main', path: 'kb/gone.md' }));
    expect(await screen.findByText(/no such fact/)).toBeInTheDocument();
    expect(screen.getByTestId('chat-panel-close')).toBeInTheDocument();
    expect(screen.queryByTestId('fact-view')).toBeNull();
  });

  // The panel's own Esc guards, on their own. SABOTAGE: remove either check.
  it('a key something else already took leaves the panel open', async () => {
    stubFleetFetch(routes());
    render(<Harness />);
    act(() => api.open(R1));
    await screen.findByTestId('chat-side-panel');
    const taker = (e: KeyboardEvent) => e.preventDefault();
    window.addEventListener('keydown', taker, true);
    fireEvent.keyDown(document.body, { key: 'Escape' });
    window.removeEventListener('keydown', taker, true);
    expect(screen.getByTestId('chat-side-panel')).toBeInTheDocument();
    fireEvent.keyDown(document.body, { key: 'Escape' });
    await waitFor(() => expect(screen.queryByTestId('chat-side-panel')).toBeNull());
  });
  it('Esc with a turn running leaves the panel open, whatever the chat column does with it', async () => {
    stubFleetFetch(routes());
    render(<Harness running />);
    act(() => api.open(R1));
    await screen.findByTestId('chat-side-panel');
    fireEvent.keyDown(document.body, { key: 'Escape' });
    expect(screen.getByTestId('chat-side-panel')).toBeInTheDocument();
  });

  // kb/decisions/web/chat/side-panel/c12285c0: Stop always wins. SABOTAGE: any focus
  // outside the chat refuses it; a field or dialog out there no longer holds its Esc.
  describe('Esc with a turn running and focus outside the chat', () => {
    const running = async () => {
      const fx = stubFleetFetch({
        ...routes(),
        [`GET ${BASE}`]: { body: snap({ running: true, items: [{ kind: 'user', text: 'q' }] }) },
        [`POST ${BASE}/cancel`]: { body: {} },
      });
      render(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} repos={[]} />);
      await screen.findByTestId('chat-facet');
      await act(async () => { await new Promise(r => setTimeout(r, 30)); });
      return fx;
    };
    const stops = (fx: ReturnType<typeof stubFleetFetch>) => fx.calls.filter(c => c.route === `POST ${BASE}/cancel`);
    const outside = (html: string) => { const box = document.createElement('div'); box.innerHTML = html; document.body.appendChild(box); return box; };
    let box: HTMLElement | null = null;
    afterEach(() => { box?.remove(); box = null; });

    it('on a plain control (the gear) it is Stop', async () => {
      const fx = await running();
      box = outside('<button>gear</button>');
      const gear = box.querySelector('button')!;
      gear.focus();
      expect(document.activeElement).toBe(gear);
      fireEvent.keyDown(gear, { key: 'Escape' });
      await waitFor(() => expect(stops(fx)).toHaveLength(1));
    });
    it('in a field it is the field\'s', async () => {
      const fx = await running();
      box = outside('<input />');
      const input = box.querySelector('input')!;
      input.focus();
      fireEvent.keyDown(input, { key: 'Escape' });
      await new Promise(r => setTimeout(r, 20));
      expect(stops(fx)).toHaveLength(0);
    });
    it('in a dialog it is the dialog\'s', async () => {
      const fx = await running();
      box = outside('<div role="dialog"><button>ok</button></div>');
      const ok = box.querySelector('button')!;
      ok.focus();
      fireEvent.keyDown(ok, { key: 'Escape' });
      await new Promise(r => setTimeout(r, 20));
      expect(stops(fx)).toHaveLength(0);
    });
    it('in a menu it is the menu\'s', async () => {
      const fx = await running();
      box = outside('<div role="menu"><button role="menuitem">item</button></div>');
      const item = box.querySelector('button')!;
      item.focus();
      expect(document.activeElement).toBe(item);
      fireEvent.keyDown(item, { key: 'Escape' });
      await new Promise(r => setTimeout(r, 20));
      expect(stops(fx)).toHaveLength(0);
    });
    // The chat stays mounted behind Browse, hidden. SABOTAGE: remove the shown() check in ChatPanel.
    it('on a chat that is not on screen it is not Stop: the hidden turn keeps running', async () => {
      const fx = stubFleetFetch({
        ...routes(),
        [`GET ${BASE}`]: { body: snap({ running: true, items: [{ kind: 'user', text: 'q' }] }) },
        [`POST ${BASE}/cancel`]: { body: {} },
      });
      render(<div style={{ display: 'none' }}><ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} repos={[]} /></div>);
      await screen.findByTestId('chat-stop');
      await act(async () => { await new Promise(r => setTimeout(r, 30)); });
      expect(fireEvent.keyDown(document.body, { key: 'Escape' })).toBe(true);
      await new Promise(r => setTimeout(r, 20));
      expect(stops(fx)).toHaveLength(0);
      expect(screen.getByTestId('chat-stop')).toBeInTheDocument();
    });
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

// The panel is the details behind the row that was clicked: its header is the
// row itself (same dot, label, argument, size and time, same success or failure
// state), drawn by the same code, never a second wording of the tool.
describe('a tool call\'s panel header is the row it opens from', () => {
  const okBind: ToolCall = { id: 'tu_ok', name: 'knomit_bind', title: 'Bind', keyArg: 'alpha', state: 'done', isError: false, args: { name: 'alpha' }, durationMs: 3 };
  const badBind: ToolCall = { id: 'tu_bad', name: 'knomit_bind', title: 'Bind', keyArg: 'demo-kb', state: 'done', isError: true, args: { name: 'demo-kb' }, durationMs: 1 };
  const other: ToolCall = { id: 'tu_x', name: 'acme_frobnicate', state: 'done', isError: false, args: { q: 1 }, durationMs: 1200 };
  const loader: ToolCall = { id: 'tu_l', name: 'load_tools', title: 'Load tools', keyArg: 'knomit_query, knomit_bind', state: 'done', isError: false, loaded: ['knomit_query', 'knomit_bind'] };
  const served = (c: ToolCall) => ({ body: { name: c.name, arguments: c.args ?? {}, result: c.isError ? 'no lens named "demo-kb"' : 'ok', is_error: c.isError, elided: false } });

  /** The transcript row and the panel, side by side, for the same call. */
  async function both(c: ToolCall) {
    stubFleetFetch({ [`GET ${BASE}/calls/${c.id}`]: served(c) });
    render(<>
      <ChatToolGroup lead={[]} calls={[c]} approvals={{}} selectedId={c.id} onSelect={vi.fn()} onAnswer={vi.fn()} />
      <Harness bound="r1" callFor={id => (id === c.id ? c : undefined)} />
    </>);
    fireEvent.click(screen.getByTestId('chat-tool-group-toggle'));
    act(() => api.open({ kind: 'result', callId: c.id }));
    await screen.findByTestId('chat-result-tab-result');
    const row = screen.getByTestId(`chat-tool-row-${c.id}`);
    const head = screen.getByTestId('chat-result-title');
    return { row, head };
  }
  const dotOf = (el: HTMLElement) => el.querySelector('[data-call-part="dot"]') as HTMLElement;
  const labelOf = (el: HTMLElement) => el.querySelector('[data-call-part="label"]') as HTMLElement;
  const lineOf = (el: HTMLElement) => el.querySelector('[data-call-part="line"]') as HTMLElement;
  const looks = (el: HTMLElement) => { const s = getComputedStyle(el); return [s.width, s.height, s.backgroundColor, s.borderTopColor, s.borderRadius, s.color, s.fontSize, s.fontWeight].join('|'); };

  for (const [name, c] of [['a succeeded bind', okBind], ['a failed bind', badBind], ['a tool the page has no wording for', other], ['the loader', loader]] as const) {
    it(`${name}: the header has the row's dot, label, line and state`, async () => {
      const { row, head } = await both(c);
      expect(dotOf(head)).not.toBeNull();
      expect(looks(dotOf(head))).toBe(looks(dotOf(row)));
      expect(labelOf(head).textContent).toBe(labelOf(row).textContent);
      expect(lineOf(head).textContent).toBe(lineOf(row).textContent);
      expect(looks(lineOf(head))).toBe(looks(lineOf(row)));
      expect(head.getAttribute('data-error')).toBe(row.getAttribute('data-error'));
      // The breadcrumb names the call by the same label.
      expect(crumb()?.endsWith(labelOf(row).textContent ?? '\u0000')).toBe(true);
    });
  }

  it('a failed call\'s header is drawn failed and a succeeded one is not', async () => {
    const { head } = await both(badBind);
    expect(head).toHaveAttribute('data-error', 'true');
    const failed = looks(lineOf(head));
    cleanup();
    const ok = await both(okBind);
    expect(ok.head.hasAttribute('data-error')).toBe(false);
    expect(looks(lineOf(ok.head))).not.toBe(failed);
  });

  it('a tool the page has no wording for is labelled from its own name, in row and header alike', async () => {
    const { row, head } = await both(other);
    expect(labelOf(row)).toHaveTextContent('acme_frobnicate');
    expect(labelOf(head)).toHaveTextContent('acme_frobnicate');
  });

  it('without the transcript row (a call the page no longer has), the header is still the row drawn from the loaded call', async () => {
    stubFleetFetch({ [`GET ${BASE}/calls/${badBind.id}`]: served(badBind) });
    render(<Harness bound="r1" />);
    act(() => api.open({ kind: 'result', callId: badBind.id }));
    const head = await screen.findByTestId('chat-result-title');
    expect(labelOf(head)).not.toBeNull();
    expect(head).toHaveAttribute('data-error', 'true');
  });

  it('the panels word no tool themselves: no tool label or verb is written in their source', () => {
    for (const src of [sidePanelSrc, resultPanelSrc]) {
      expect(src).not.toMatch(/\btoolRowLabel\b|\btoolVerb\b|knomit: Knomit|'Bound|'Searched|'Read'|'Bind'|'Query'/);
    }
  });
});

describe('the activity line of a group with a failed bind', () => {
  it('says nothing for the failed bind, and the call after it searched where the chat was bound', () => {
    const calls: ToolCall[] = [
      { id: 'b1', name: 'knomit_bind', title: 'Bind', state: 'done', isError: true, args: { name: 'nosuch-kb' } },
      { id: 'q1', name: 'knomit_query', title: 'Query', state: 'done', isError: false, args: { binding: 'h', text: 'x' }, inBase: true },
    ];
    render(<ChatToolGroup lead={[]} calls={calls} approvals={{}} selectedId={null} bound="r1" onSelect={vi.fn()} onAnswer={vi.fn()} />);
    expect(screen.getByTestId('chat-activity-summary').textContent).toBe('Query in r1');
  });
});
