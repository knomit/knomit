import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, cleanup, fireEvent, within } from '@testing-library/react';
import { useReducer } from 'react';
import { RightPanel } from './RightPanel';
import { ChatSidePanel } from './ChatSidePanel';
import { FactView } from './FactView';
import { useFactEdges } from './useFactEdges';
import { init } from './state';
import type { AppState } from './state';
import { panelReducer } from './chatPanelState';
import type { BrowseFactRequest, PanelEntry } from './chatPanelState';
import type { RepoRef } from './chatFactLinks';
import { installFakeEventSource, uninstallFakeEventSource } from './testEventSource';
import { stubFleetFetch } from './fleetTestFetch';
import materializeCss from './materialize.css?raw';

// Browse and the chat's side panel mount ONE fact view (FactView). These render
// the same fact in both, through what each really mounts, and compare what is on
// screen: the only differences allowed are the × and the retract button.
// SABOTAGE: render anything else for a fact in the panel; drop hideRetract; drop
// bandAction; send onOpenPanel from Browse.

vi.mock('./FactView', async importOriginal => {
  const actual = await importOriginal<typeof import('./FactView')>();
  return { ...actual, FactView: vi.fn(actual.FactView) };
});

const BRANCH = '/api/v1/repos/r1/branches/main';
const PATH = 'kb/decisions/one.md';
const AT = 'abc1234' + 'f'.repeat(33);
const OLDER = '1234567' + 'e'.repeat(33);
const NEWEST = '7654321' + 'd'.repeat(33);
const FACT = {
  path: PATH, title: 'One decision', type: 'observation', kind: 'epistemic', origin: 'authored',
  body: 'We chose **SSE**.', domain: ['web'], confidence: 0.9, sources: 2, entities: ['Server'],
  motifs: ['a-shape'],
  refs: ['kb/decisions/two.md'],
};
const EDGES = {
  incoming: { _embedded: { refs: [{ path: 'a.md', title: 'A', commit: 'c1' }] } },
  outgoing: { _embedded: { refs: [{ path: 'kb/decisions/two.md', title: 'Two', commit: 'c3' }] } },
};
function routes(base: string, fact: object) {
  return {
    [`GET ${base}/facts/${PATH}`]: { body: fact },
    [`GET ${base}/facts/${PATH}/incoming`]: { body: EDGES.incoming },
    [`GET ${base}/facts/${PATH}/outgoing`]: { body: EDGES.outgoing },
  };
}
const common = {
  [`GET ${BRANCH}/facts/${PATH}/commits`]: { body: { _embedded: { commits: [{ commit: NEWEST }, { commit: AT }, { commit: OLDER }] } } },
  [`GET ${BRANCH}`]: { body: { head: NEWEST, branch: 'main', writable: true, ontology_root: 'kb', index_state: 'ready' } },
  [`GET ${BRANCH}/status`]: { body: { head: NEWEST, branch: 'main', writable: true, ontology_root: 'kb', index_state: 'ready' } },
};

function Browse({ commit }: { commit?: string }) {
  const state: AppState = {
    ...init, context: { kind: 'repo', repo: 'r1' }, repo: 'r1', branch: 'main', factPath: PATH,
    asOf: commit ? { mode: 'history', commit } : { mode: 'live' }, headCommit: '', branchWritable: true,
  };
  const edges = useFactEdges(state);
  return <RightPanel state={state} dispatch={() => {}} onScrub={() => {}} onHopRef={() => {}} onHopEdge={() => {}} repoNames={{}} incoming={edges.incoming} outgoing={edges.outgoing} edgesError={edges.error} refCommits={edges.refCommits} />;
}

function Panel({ entry, onBrowse, repos, serverReadOnly, onScreen, running = false }: { entry: PanelEntry; onBrowse?: (r: BrowseFactRequest) => void; repos?: RepoRef[]; serverReadOnly?: boolean; onScreen?: boolean; running?: boolean }) {
  const [panel, dispatch] = useReducer(panelReducer, { entry });
  // Off screen is display:none on an ancestor, as App draws a chat behind Browse.
  return <div style={onScreen === false ? { display: 'none' } : undefined}><ChatSidePanel chatId="c" panel={panel} dispatch={dispatch} running={running} width={470} onSplitterDown={() => {}} onBrowse={onBrowse} repos={repos} serverReadOnly={serverReadOnly} onScreen={onScreen} /></div>;
}

const settled = async (root: HTMLElement) => {
  const view = await within(root).findByTestId('fact-view');
  await within(view).findByTestId('version-walker');
  await waitFor(() => expect(within(view).getByTestId('connections-in')).toHaveTextContent('1'));
  await within(view).findByTestId('fact-title');
  return view;
};
/** The view's DOM without the two controls that are allowed to differ. */
const comparable = (view: HTMLElement) => {
  const copy = view.cloneNode(true) as HTMLElement;
  copy.querySelectorAll('[data-testid="chat-panel-close"],[data-testid="retract-btn"]').forEach(n => n.remove());
  // Attributes sorted: their order is the order React happened to set them in.
  const ser = (n: Node): string => {
    if (n.nodeType !== Node.ELEMENT_NODE) return n.textContent ?? '';
    const el = n as Element;
    const attrs = [...el.attributes].map(a => `${a.name}=${JSON.stringify(a.value)}`).sort().join(' ');
    return `<${el.tagName} ${attrs}>${[...el.childNodes].map(ser).join('')}</${el.tagName}>`;
  };
  return ser(copy);
};
const inBrowse = (commit?: string) => render(<Browse commit={commit} />).container;
const inPanel = (entry: Omit<Extract<PanelEntry, { kind: 'fact' }>, 'kind' | 'repo' | 'branch' | 'path'> = {}, onBrowse?: (r: BrowseFactRequest) => void) =>
  render(<Panel onBrowse={onBrowse} entry={{ kind: 'fact', repo: 'r1', branch: 'main', path: PATH, ...entry }} />).container;

beforeEach(() => { installFakeEventSource(); localStorage.clear(); window.history.replaceState(null, '', '/'); vi.mocked(FactView).mockClear(); });
afterEach(() => { cleanup(); uninstallFakeEventSource(); vi.unstubAllGlobals(); delete document.documentElement.dataset.matFocus; });

describe('a fact in Browse and in the chat panel', () => {
  it('live: the same component, the same DOM', async () => {
    stubFleetFetch({ ...common, ...routes(BRANCH, { ...FACT, as_of: { commit: NEWEST, date: '2026-11-01T10:00:00Z' } }) });
    const b = comparable(await settled(inBrowse()));
    cleanup();
    const mounted = vi.mocked(FactView).mock.calls.length;
    expect(mounted).toBeGreaterThan(0);
    const p = comparable(await settled(inPanel()));
    expect(vi.mocked(FactView).mock.calls.length).toBeGreaterThan(mounted);
    expect(p).toBe(b);
    expect(p).toContain('We chose');
  });

  it('at a commit: the same component in its as-of mode, the same DOM', async () => {
    const at = { ...FACT, body: 'THE OLD TEXT', as_of: { commit: AT, date: '2026-10-01T10:00:00Z' } };
    stubFleetFetch({ ...common, ...routes(`${BRANCH}/commits/${AT}`, at) });
    const b = comparable(await settled(inBrowse(AT)));
    cleanup();
    const p = comparable(await settled(inPanel({ commit: AT })));
    expect(p).toBe(b);
    expect(p).toContain('THE OLD TEXT');
    expect(p).not.toContain('retracted-version-badge');
  });

  it('a fact the commit retracted: both read its last version and carry the badge', async () => {
    const last = { ...FACT, body: 'WHAT IT SAID', as_of: { commit: OLDER, date: '2026-09-01T10:00:00Z' } };
    const fx = stubFleetFetch({ ...common, ...routes(`${BRANCH}/commits/${AT}`, last) });
    const b = await settled(inBrowse(AT));
    expect(within(b).getByTestId('retracted-version-badge')).toHaveTextContent('retracted at abc1234');
    const bHtml = comparable(b);
    cleanup();
    const p = await settled(inPanel({ commit: AT }));
    expect(comparable(p)).toBe(bHtml);
    expect(within(p).getByTestId('retracted-version-badge')).toHaveTextContent('retracted at abc1234');
    expect(fx.calls.some(c => c.route === `GET ${BRANCH}/commits/${AT}/facts/${PATH}`)).toBe(true);
  });

  // The user's one exception, with the ×: no retract in the chat.
  it('Browse has the retract button, the panel does not; the panel has the ×, Browse does not', async () => {
    stubFleetFetch({ ...common, ...routes(BRANCH, { ...FACT, as_of: { commit: NEWEST, date: '2026-11-01T10:00:00Z' } }) });
    const b = await settled(inBrowse());
    expect(within(b).getByTestId('retract-btn')).toBeInTheDocument();
    expect(within(b).queryByTestId('chat-panel-close')).toBeNull();
    cleanup();
    const p = await settled(inPanel());
    expect(within(p).queryByTestId('retract-btn')).toBeNull();
    const close = within(p).getByTestId('chat-panel-close');
    // In the band's action group, beside materialize.
    expect(within(p).getByTestId('fact-band')).toContainElement(close);
    expect(close.parentElement).toContainElement(within(p).getByTestId('materialize-btn'));
  });

  it('every control is there in the panel: version, cited by, cites, motif, materialize, tags, refs', async () => {
    stubFleetFetch({ ...common, ...routes(BRANCH, { ...FACT, as_of: { commit: NEWEST, date: '2026-11-01T10:00:00Z' } }) });
    const p = within(await settled(inPanel()));
    for (const id of ['version-walker', 'connections-in', 'connections-out', 'motif-cell', 'materialize-btn', 'fact-version-date']) {
      expect(p.getByTestId(id)).toBeInTheDocument();
    }
  });

  it('what navigates goes to Browse on that fact: version, cited by, cites, motif, a tag, a ref', async () => {
    stubFleetFetch({ ...common, ...routes(BRANCH, {
      ...FACT, as_of: { commit: NEWEST, date: '2026-11-01T10:00:00Z' },
    }) });
    const onBrowse = vi.fn();
    const p = within(await settled(inPanel({}, onBrowse)));
    const base = { repo: 'r1', branch: 'main', path: PATH };
    fireEvent.click(p.getByTestId('version-walker'));
    expect(onBrowse).toHaveBeenLastCalledWith({ ...base, commit: NEWEST }); // live: the newest version, as Browse's walker does
    fireEvent.click(p.getByTestId('connections-in'));
    expect(onBrowse).toHaveBeenLastCalledWith({ ...base, panel: { connections: 'in' } });
    fireEvent.click(p.getByTestId('connections-out'));
    expect(onBrowse).toHaveBeenLastCalledWith({ ...base, panel: { connections: 'out' } });
    fireEvent.click(p.getByTestId('motif-cell'));
    expect(onBrowse).toHaveBeenLastCalledWith({ ...base, panel: { motif: 'a-shape' } });
    fireEvent.click(p.getByText('web'));
    expect(onBrowse).toHaveBeenLastCalledWith({ ...base, filter: { category: 'domain', value: 'web' } });
    fireEvent.click(p.getByText(/kb\/decisions\/two\.md/));
    expect(onBrowse).toHaveBeenLastCalledWith({ ...base, path: 'kb/decisions/two.md', commit: 'c3' });
  });

  it('Browse keeps its panels in place: the same cells open under the band there', async () => {
    stubFleetFetch({ ...common, ...routes(BRANCH, { ...FACT, as_of: { commit: NEWEST, date: '2026-11-01T10:00:00Z' } }) });
    const b = within(await settled(inBrowse()));
    fireEvent.click(b.getByTestId('connections-in'));
    expect(b.getByTestId('connections-in').getAttribute('aria-expanded')).toBe('true');
  });

  it('materialize opens in place in the panel, and leaves the URL alone', async () => {
    stubFleetFetch({ ...common, ...routes(BRANCH, { ...FACT, as_of: { commit: NEWEST, date: '2026-11-01T10:00:00Z' } }) });
    const before = window.location.search;
    const p = within(await settled(inPanel()));
    fireEvent.click(p.getByTestId('materialize-btn'));
    await waitFor(() => expect(screen.queryByTestId('fact-view')).toBeNull());
    expect(window.location.search).toBe(before);
  });

  // At the commit that retracted the fact (a version the timeline lists) the
  // view shows its last version (fallback, as_of = OLDER). Materialize reads
  // THAT version: at the anchor the fact does not exist and the read 404s.
  // The page URL is not given the other commit: Browse writes only the
  // materialize options, and its anchor stays the retract commit.
  // SABOTAGE: materialize at the anchor (AT).
  it.each([
    ['Browse', () => inBrowse(AT)],
    ['the panel', () => inPanel({ commit: AT })],
  ])('in %s, materialize at a retract commit reads the last live version', async (_where, mount) => {
    const last = { ...FACT, body: 'WHAT IT SAID', as_of: { commit: OLDER, date: '2026-09-01T10:00:00Z' } };
    const fx = stubFleetFetch({ ...common, ...routes(`${BRANCH}/commits/${AT}`, last) });
    const v = within(await settled(mount()));
    fireEvent.click(v.getByTestId('materialize-btn'));
    const pages = `GET ${BRANCH}/commits/${OLDER}/facts/${PATH}/materialize/pages`;
    await waitFor(() => expect(fx.calls.map(c => c.route)).toContain(pages));
    expect(fx.calls.some(c => c.route.includes(`/commits/${AT}/facts/${PATH}/materialize`))).toBe(false);
    expect(window.location.search).not.toContain(OLDER);
  });

  it('at a commit where the fact exists, materialize reads at that commit', async () => {
    const at = { ...FACT, as_of: { commit: AT, date: '2026-10-01T10:00:00Z' } };
    const fx = stubFleetFetch({ ...common, ...routes(`${BRANCH}/commits/${AT}`, at) });
    const b = within(await settled(inBrowse(AT)));
    fireEvent.click(b.getByTestId('materialize-btn'));
    await waitFor(() => expect(fx.calls.map(c => c.route)).toContain(`GET ${BRANCH}/commits/${AT}/facts/${PATH}/materialize/pages`));
  });

  it('Browse\'s materialize still writes the URL', async () => {
    stubFleetFetch({ ...common, ...routes(BRANCH, { ...FACT, as_of: { commit: NEWEST, date: '2026-11-01T10:00:00Z' } }) });
    const b = within(await settled(inBrowse()));
    fireEvent.click(b.getByTestId('materialize-btn'));
    await waitFor(() => expect(window.location.search).not.toBe(''));
  });
});

// The one place the panel can write: the editor of a fact that cannot be parsed. It
// follows the same gate as Browse, from what the panel learns about the repo and branch.
// SABOTAGE: treat every branch as writable; ignore a subscription; ignore a read-only server.
describe('the panel\'s write gate', () => {
  const entry: PanelEntry = { kind: 'fact', repo: 'r1', branch: 'main', path: PATH };
  const broken = { ...FACT, parse_error: 'bad yaml', body: 'raw', as_of: { commit: NEWEST, date: '2026-11-01T10:00:00Z' } };
  const statusRoute = (writable: boolean) => ({ [`GET ${BRANCH}`]: { body: { head: NEWEST, branch: 'main', writable, ontology_root: 'kb', index_state: 'ready' } } });
  const settle = () => new Promise(r => setTimeout(r, 30));

  it('a writable branch of an ordinary repo can save, and the save goes to that branch', async () => {
    const fx = stubFleetFetch({ ...common, ...routes(BRANCH, broken), ...statusRoute(true), [`PUT ${BRANCH}/facts/${PATH}`]: { body: FACT } });
    render(<Panel entry={entry} />);
    const save = await screen.findByTestId('fact-save-btn');
    await waitFor(() => expect(save).toBeEnabled());
    fireEvent.click(save);
    await waitFor(() => expect(fx.calls.some(c => c.route === `PUT ${BRANCH}/facts/${PATH}`)).toBe(true));
  });
  it('a branch the server says cannot be written cannot save', async () => {
    stubFleetFetch({ ...common, ...routes(BRANCH, broken), ...statusRoute(false) });
    render(<Panel entry={entry} />);
    const save = await screen.findByTestId('fact-save-btn');
    await settle();
    expect(save).toBeDisabled();
  });
  it('a subscription cannot save, whatever its branch says', async () => {
    stubFleetFetch({ ...common, ...routes(BRANCH, broken), ...statusRoute(true) });
    render(<Panel entry={entry} repos={[{ name: 'r1', mode: 'subscribe' }]} />);
    const save = await screen.findByTestId('fact-save-btn');
    await settle();
    expect(save).toBeDisabled();
  });
  it('a read-only server cannot save', async () => {
    stubFleetFetch({ ...common, ...routes(BRANCH, broken), ...statusRoute(true) });
    render(<Panel entry={entry} serverReadOnly />);
    const save = await screen.findByTestId('fact-save-btn');
    await settle();
    expect(save).toBeDisabled();
  });
  it('a fact read at a commit cannot save', async () => {
    const atRoutes = routes(`${BRANCH}/commits/${AT}`, broken);
    stubFleetFetch({ ...common, ...atRoutes, ...statusRoute(true) });
    render(<Panel entry={{ ...entry, commit: AT }} />);
    const save = await screen.findByTestId('fact-save-btn');
    await settle();
    expect(save).toBeDisabled();
  });
});

describe('the ×', () => {
  it('is there while the fact loads, before there is a band', async () => {
    let release: () => void = () => {};
    stubFleetFetch({ ...common, ...routes(BRANCH, { ...FACT, as_of: { commit: NEWEST, date: '2026-11-01T10:00:00Z' } }) });
    const inner = fetch as unknown as (input: string, init?: RequestInit) => Promise<Response>;
    vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
      if (String(input).endsWith(`/facts/${PATH}`)) await new Promise<void>(r => { release = r; });
      return inner(input, init);
    }));
    render(<Panel entry={{ kind: 'fact', repo: 'r1', branch: 'main', path: PATH }} />);
    expect(await screen.findByText('Loading...')).toBeInTheDocument();
    expect(screen.queryByTestId('fact-band')).toBeNull();
    expect(screen.getByTestId('chat-panel-close')).toBeInTheDocument();
    release();
    await screen.findByTestId('fact-band');
  });
  it('is in the materialize view\'s bar, and it closes the panel, Focus with it', async () => {
    stubFleetFetch({ ...common, ...routes(BRANCH, { ...FACT, as_of: { commit: NEWEST, date: '2026-11-01T10:00:00Z' } }) });
    const p = within(await settled(inPanel()));
    fireEvent.click(p.getByTestId('materialize-btn'));
    const actions = await screen.findByTestId('mat-actions');
    expect(document.documentElement.dataset.matFocus).toBe('1');
    fireEvent.click(within(actions).getByTestId('chat-panel-close'));
    await waitFor(() => expect(screen.queryByTestId('chat-side-panel')).toBeNull());
    expect(document.documentElement.dataset.matFocus).toBeUndefined();
  });
  it('Browse\'s materialize view has none', async () => {
    stubFleetFetch({ ...common, ...routes(BRANCH, { ...FACT, as_of: { commit: NEWEST, date: '2026-11-01T10:00:00Z' } }) });
    const b = within(await settled(inBrowse()));
    fireEvent.click(b.getByTestId('materialize-btn'));
    const actions = await screen.findByTestId('mat-actions');
    expect(within(actions).queryByTestId('chat-panel-close')).toBeNull();
  });
});

// A chat stays mounted behind Browse. SABOTAGE: useMatFocus ignores `hidden`.
describe('the panel\'s Focus while its chat is not on screen', () => {
  const entry: PanelEntry = { kind: 'fact', repo: 'r1', branch: 'main', path: PATH };
  const inFocus = async () => {
    stubFleetFetch({ ...common, ...routes(BRANCH, { ...FACT, as_of: { commit: NEWEST, date: '2026-11-01T10:00:00Z' } }) });
    const utils = render(<Panel entry={entry} />);
    fireEvent.click(within(await settled(utils.container)).getByTestId('materialize-btn'));
    await screen.findByTestId('mat-actions');
    expect(document.documentElement.dataset.matFocus).toBe('1');
    return utils;
  };
  it('has no effect on the shell, and comes back with the chat', async () => {
    const { rerender } = await inFocus();
    rerender(<Panel entry={entry} onScreen={false} />);
    expect(document.documentElement.dataset.matFocus).toBeUndefined();
    rerender(<Panel entry={entry} onScreen />);
    expect(document.documentElement.dataset.matFocus).toBe('1');
  });
  it('does not take Esc', async () => {
    const { rerender } = await inFocus();
    rerender(<Panel entry={entry} onScreen={false} />);
    const shell = vi.fn();
    window.addEventListener('keydown', shell);
    fireEvent.keyDown(window, { key: 'Escape' });
    window.removeEventListener('keydown', shell);
    expect(shell).toHaveBeenCalledOnce();
    // It was not spent on the hidden view: Focus is still on when the chat returns.
    rerender(<Panel entry={entry} onScreen />);
    expect(document.documentElement.dataset.matFocus).toBe('1');
  });
  it('fills the window in a chat, through the same attribute', () => {
    const rule = materializeCss.slice(materializeCss.indexOf("html[data-mat-focus]"));
    for (const sel of ['.k-chat-main', '.k-chat-result', "[data-testid='chat-panel-splitter']", "[data-testid='chat-panel-footer']"]) expect(rule).toContain(`html[data-mat-focus] ${sel}`);
    // Undisplayed, ChatPanel would think its chat is off screen and Esc could not Stop.
    expect(rule).not.toMatch(/\.k-chat-main[^{]*\{[^}]*display: none/);
  });
});
