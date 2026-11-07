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
import { installFakeEventSource, uninstallFakeEventSource } from './testEventSource';
import { stubFleetFetch } from './fleetTestFetch';

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

function Panel({ entry, onBrowse }: { entry: PanelEntry; onBrowse?: (r: BrowseFactRequest) => void }) {
  const [panel, dispatch] = useReducer(panelReducer, { entry });
  return <ChatSidePanel chatId="c" panel={panel} dispatch={dispatch} running={false} width={470} onSplitterDown={() => {}} onBrowse={onBrowse} />;
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
afterEach(() => { cleanup(); uninstallFakeEventSource(); vi.unstubAllGlobals(); });

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

  it('Browse\'s materialize still writes the URL', async () => {
    stubFleetFetch({ ...common, ...routes(BRANCH, { ...FACT, as_of: { commit: NEWEST, date: '2026-11-01T10:00:00Z' } }) });
    const b = within(await settled(inBrowse()));
    fireEvent.click(b.getByTestId('materialize-btn'));
    await waitFor(() => expect(window.location.search).not.toBe(''));
  });
});
