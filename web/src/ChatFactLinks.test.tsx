import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, act, fireEvent, cleanup, within } from '@testing-library/react';
import { ChatFacet } from './ChatFacet';
import { installFakeEventSource, uninstallFakeEventSource, chatStream } from './testEventSource';
import { resetFactCache } from './chatFactLinks';
import { stubFleetFetch } from './fleetTestFetch';
import type { ChatSnapshot } from './chatApi';
import { typeStyles, repoHue } from './utils';
import { PANEL_OPEN_IN_BROWSE } from './chatCopy';
import { TypeIcon } from './icons';
import { CommitHash } from './TimelineNav';

const CHAT = { id: 'chat-a', provider: 'anthropic', model: 'claude-test-x' };
const BASE = '/api/v1/chats/chat-a';
const RESOLVE = 'POST /api/v1/facts/resolve';
const FACT_BASE = '/api/v1/repos/agentic-engineering/branches/agent:test/facts';

const POLICY = {
  handle: '3fc77970', id: '3fc77970', title: 'Agent config files are executable; treat them as code', type: 'policy',
  repo: 'agentic-engineering', branch: 'agent/test', path: 'kb/invariants/security/3fc77970.md',
  excerpt: 'Files an agent loads on start change what it can do.', confidence: 0.92,
  kind: 'pragmatic', origin: 'authored', sources: 4,
};
const OBS = (id: string, title: string) => ({
  handle: id, id, title, type: 'observation', repo: 'agentic-engineering', branch: 'agent/test',
  path: `kb/incidents/${id}.md`, excerpt: 'x', confidence: 0.8,
});

const ANSWER = [
  'The first practice is `3fc77970`; the commit `deadbeef` is not a fact.',
  '',
  'Practices live in `agentic-engineering`.',
  '',
  '| practice | incidents |',
  '|---|---|',
  '| `3fc77970` | `aaaaaaa1`, `aaaaaaa2`, `aaaaaaa3` |',
].join('\n');

const snapshot = (items: ChatSnapshot['items']): ChatSnapshot => ({
  ...CHAT, running: false, grants: [], pending_approvals: [], items,
});
const TWO_TURNS = snapshot([
  { kind: 'user', text: 'rank the practices' },
  { kind: 'assistant', text: ANSWER },
  { kind: 'user', text: 'and the first one?' },
  { kind: 'assistant', text: 'It is `kb/invariants/security/3fc77970.md`.' },
]);

const resolved = (body: unknown) => {
  const handles = (body as { handles: string[] }).handles;
  const known: Record<string, object> = {
    '3fc77970': POLICY,
    'kb/invariants/security/3fc77970.md': { ...POLICY, handle: 'kb/invariants/security/3fc77970.md' },
    aaaaaaa1: OBS('aaaaaaa1', 'Cursor marketplace extensions run on install'),
    aaaaaaa2: OBS('aaaaaaa2', 'Storm-0940 ARTEX'),
    aaaaaaa3: OBS('aaaaaaa3', 'Meta internal leak'),
  };
  return { body: handles.filter(h => known[h]).map(h => known[h]) };
};

function mount(snap: ChatSnapshot = TWO_TURNS, onBrowseFact = vi.fn()) {
  const fx = stubFleetFetch({
    [`GET ${BASE}`]: { body: snap },
    [RESOLVE]: resolved,
    [`GET ${FACT_BASE}/${POLICY.path}`]: { body: { path: POLICY.path, title: POLICY.title, type: 'policy', kind: 'pragmatic', body: 'Review them like code.', domain: [], confidence: 0.92, sources: 4, entities: [], refs: [] } },
  });
  const repos = [{ name: 'agentic-engineering', id: '87b70fb1aeb1' }];
  const utils = render(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} onBrowseFact={onBrowseFact} repos={repos} />);
  return { fx, onBrowseFact, ...utils };
}
const resolveCalls = (fx: ReturnType<typeof stubFleetFetch>) => fx.calls.filter(c => c.route === RESOLVE);

beforeEach(() => { installFakeEventSource(); resetFactCache(); });
afterEach(() => { cleanup(); uninstallFakeEventSource(); vi.unstubAllGlobals(); vi.useRealTimers(); });

const settle = () => new Promise(r => setTimeout(r, 30));

describe('chat fact links', () => {
  it('resolves every finished turn in ONE call per mount, and never again for the same text', async () => {
    const { fx } = mount();
    await waitFor(() => expect(screen.getAllByTestId('fact-link')).toHaveLength(4));
    expect(resolveCalls(fx).map(c => c.body)).toEqual([
      // deadbeef looks like a short id, so it is asked about; the server omits it.
      { handles: ['3fc77970', 'deadbeef', 'aaaaaaa1', 'aaaaaaa2', 'aaaaaaa3', 'kb/invariants/security/3fc77970.md'], prefer: [] },
    ]);
    // A re-render with the same transcript (a reconnect's snapshot) asks nothing more.
    fireEvent.change(screen.getByTestId('chat-input'), { target: { value: 'x' } });
    await settle();
    expect(resolveCalls(fx)).toHaveLength(1);
  });

  it('a remount asks nothing it already resolved: the answers are kept per handle and preference', async () => {
    const first = mount();
    await waitFor(() => expect(screen.getAllByTestId('fact-link')).toHaveLength(4));
    first.unmount();
    const { fx } = mount();
    await waitFor(() => expect(screen.getAllByTestId('fact-link')).toHaveLength(4));
    await settle();
    expect(resolveCalls(fx)).toHaveLength(0);
  });

  it('a turn that finishes later gets one call, for its own new handles only', async () => {
    const live = { ...snapshot([
      { kind: 'user', text: 'q1' }, { kind: 'assistant', text: 'see `3fc77970`' },
      { kind: 'user', text: 'q2' }, { kind: 'assistant', text: 'and `aaaaaaa1`, `3fc77970`' },
    ]), running: true };
    const { fx } = mount(live);
    await screen.findByTestId('fact-link');
    await settle();
    expect(resolveCalls(fx).map(c => c.body)).toEqual([{ handles: ['3fc77970'], prefer: [] }]);
    fx.set(`GET ${BASE}`, { body: { ...live, running: false } });
    act(() => { chatStream('chat-a')!.emit('turn_done', { turn: 2 }); });
    await waitFor(() => expect(resolveCalls(fx)).toHaveLength(2));
    expect(resolveCalls(fx)[1].body).toEqual({ handles: ['aaaaaaa1'], prefer: [] });
    await waitFor(() => expect(screen.getAllByTestId('fact-link')).toHaveLength(3));
  });

  it('prefers the repos the turn worked in: its calls\' repo or lens, then the binding in force', async () => {
    const { fx } = mount(snapshot([
      { kind: 'user', text: 'bind' },
      { kind: 'tool_call', id: 'tu_1', name: 'knomit_bind', state: 'done', arguments: { lens: 'all' }, in_base: true, target: 'all' },
      { kind: 'assistant', text: 'bound' },
      { kind: 'user', text: 'q' },
      { kind: 'tool_call', id: 'tu_2', name: 'knomit_query', state: 'done', arguments: { text: 'x', repo: 'agentic-engineering' }, in_base: true, target: 'agentic-engineering' },
      { kind: 'tool_call', id: 'tu_3', name: 'knomit_materialize', state: 'done', arguments: { file: 'kb://87b70fb1aeb1/kb/a/b.md', binding: 'h1' }, in_base: true },
      { kind: 'assistant', text: 'see `3fc77970`' },
    ]));
    await screen.findByTestId('fact-link');
    expect(resolveCalls(fx).map(c => c.body)).toEqual([
      { handles: ['3fc77970'], prefer: ['87b70fb1aeb1', 'all', 'agentic-engineering'] },
    ]);
  });

  it('a failed bind to a second repo is left out of prefer, so the turn still names one repo', async () => {
    const { fx } = mount(snapshot([
      { kind: 'user', text: 'q' },
      { kind: 'tool_call', id: 'tu_1', name: 'knomit_bind', state: 'done', arguments: { repo: 'agentic-engineering' } },
      { kind: 'tool_call', id: 'tu_2', name: 'knomit_bind', state: 'done', is_error: true, arguments: { repo: 'no-such-repo' } },
      { kind: 'assistant', text: 'see `3fc77970`' },
    ]));
    await screen.findByTestId('fact-link');
    expect(resolveCalls(fx)[0].body).toEqual({ handles: ['3fc77970'], prefer: ['agentic-engineering'] });
  });

  it('a fact the server could not parse (no confidence) shows no confidence in the card, as Browse leaves a missing one out', async () => {
    const { confidence: _c, kind: _k, origin: _o, sources: _s, ...unparsed } = POLICY;
    void _c; void _k; void _o; void _s;
    stubFleetFetch({
      [`GET ${BASE}`]: { body: snapshot([{ kind: 'user', text: 'q' }, { kind: 'assistant', text: 'see `3fc77970`' }]) },
      [RESOLVE]: { body: [{ ...unparsed, excerpt: '' }] },
    });
    render(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} repos={[]} />);
    const chip = await screen.findByTestId('fact-link');
    vi.useFakeTimers();
    fireEvent.mouseEnter(chip);
    act(() => { vi.advanceTimersByTime(320); });
    const meta = within(screen.getByTestId('fact-card')).getByTestId('fact-meta');
    expect(within(meta).getByTestId('fact-type-badge')).toHaveTextContent('policy');
    expect(within(meta).queryByTestId('fact-confidence')).toBeNull();
    expect(meta.textContent).not.toContain('conf');
    vi.useRealTimers();
  });

  it('a commit hash the server resolved in the turn\'s repo is the timeline\'s CommitHash; others stay code', async () => {
    const FULL = 'abc1234' + 'f'.repeat(33);
    const fx = stubFleetFetch({
      [`GET ${BASE}`]: { body: snapshot([
        { kind: 'user', text: 'q' },
        { kind: 'tool_call', id: 'tu_1', name: 'knomit_query', state: 'done', arguments: { text: 'x', repo: 'agentic-engineering' }, in_base: true, target: 'agentic-engineering' },
        { kind: 'assistant', text: 'Fixed in `abc1234`; `1111111` is elsewhere.' },
      ]) },
      [RESOLVE]: (body: unknown) => {
        const b = body as { handles: string[]; prefer: string[] };
        return { body: b.handles.includes('abc1234') && b.prefer[0] === 'agentic-engineering'
          ? [{ handle: 'abc1234', id: '', title: '', type: '', repo: 'agentic-engineering', branch: 'agent/test', path: '', excerpt: '', confidence: 0, commit: FULL }]
          : [] };
      },
    });
    render(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} repos={[]} />);
    const link = await screen.findByTestId('commit-link');
    expect(resolveCalls(fx)[0].body).toEqual({ handles: ['abc1234', '1111111'], prefer: ['agentic-engineering'] });
    expect(within(link).getByTestId('commit-hash')).toHaveTextContent('abc1234');
    // It IS the timeline's component, not a look-alike.
    const ref = render(<CommitHash commit={FULL} />).container.firstElementChild!;
    expect(link.innerHTML).toBe(ref.outerHTML);
    expect(link).toHaveAttribute('title', FULL);
    const codes = [...screen.getByTestId('chat-turn-0').querySelectorAll('code')].map(c => c.textContent);
    expect(codes).toEqual(['1111111']);
  });

  it('a failed resolve leaves the code as code and is not retried, on this mount or the next', async () => {
    const { fx, unmount } = mount();
    fx.set(RESOLVE, { status: 500, body: { detail: 'boom' } });
    await waitFor(() => expect(resolveCalls(fx)).toHaveLength(1));
    await settle();
    expect(screen.queryByTestId('fact-link')).toBeNull();
    expect([...screen.getByTestId('chat-turn-0').querySelectorAll('code')].map(c => c.textContent)).toContain('3fc77970');
    fireEvent.change(screen.getByTestId('chat-input'), { target: { value: 'x' } });
    await settle();
    expect(resolveCalls(fx)).toHaveLength(1);
    unmount();
    const again = mount();
    await screen.findByTestId('chat-turn-0');
    await settle();
    expect(resolveCalls(again.fx)).toHaveLength(0);
  });

  it('a resolved fact is a chip with its type\'s TypeIcon and the title; unresolved code stays code', async () => {
    mount();
    const turn0 = await screen.findByTestId('chat-turn-0');
    const chip = (await within(turn0).findAllByTestId('fact-link'))[0];
    expect(chip).toHaveAttribute('title', POLICY.title);
    expect(chip).toHaveAttribute('data-type', 'policy');
    expect(chip).toHaveTextContent(POLICY.title);
    const tile = within(chip).getByTestId('fact-link-glyph');
    expect(tile.style.background).toBe(hexToRgb(typeStyles.policy.bg));
    const svg = tile.querySelector('svg')!;
    expect(svg.getAttribute('width')).toBe('11');
    expect(svg.getAttribute('stroke')).toBe(typeStyles.policy.color);
    // The glyph IS the policy TypeIcon (not another type's drawn in policy's colour).
    const ref = render(<TypeIcon type="policy" color={typeStyles.policy.color} size={11} />).container.querySelector('svg')!;
    expect(svg.outerHTML).toBe(ref.outerHTML);
    const other = render(<TypeIcon type="observation" color={typeStyles.policy.color} size={11} />).container.querySelector('svg')!;
    expect(svg.outerHTML).not.toBe(other.outerHTML);
    // The commit hash did not resolve: it is still a code span, with its text.
    const codes = [...turn0.querySelectorAll('code')].map(c => c.textContent);
    expect(codes).toContain('deadbeef');
    expect(codes).not.toContain('3fc77970');
  });

  it('a bare repo name in code is a repo chip with the repo\'s colour dot', async () => {
    mount();
    const chip = await screen.findByTestId('repo-link');
    expect(chip).toHaveTextContent('agentic-engineering');
    expect(within(chip).getByTestId('repo-link-dot').style.background).toBe(hexToRgb(repoHue('agentic-engineering')));
  });

  it('several facts in one table cell show the first link, then +N', async () => {
    mount();
    const turn0 = await screen.findByTestId('chat-turn-0');
    await within(turn0).findAllByTestId('fact-link');
    const cell = turn0.querySelectorAll('tbody td')[1] as HTMLElement;
    const links = within(cell).getAllByTestId('fact-link');
    expect(links).toHaveLength(1);
    expect(links[0]).toHaveTextContent('Cursor marketplace extensions run on install');
    expect(within(cell).getByTestId('fact-link-more')).toHaveTextContent('+2');
    expect(cell.textContent).not.toContain(',');
  });

  it('click opens the fact in the side panel and the chip stays highlighted; Cmd-click goes to Browse', async () => {
    const { onBrowseFact } = mount();
    const turn1 = await screen.findByTestId('chat-turn-1');
    const chip = await within(turn1).findByTestId('fact-link');
    expect(screen.queryByTestId('chat-side-panel')).toBeNull();

    fireEvent.click(chip, { metaKey: true });
    expect(onBrowseFact).toHaveBeenCalledWith('chat-a', { repo: 'agentic-engineering', path: POLICY.path });
    expect(screen.queryByTestId('chat-side-panel')).toBeNull();

    fireEvent.click(chip);
    const panel = await screen.findByTestId('chat-side-panel');
    expect(await within(panel).findByTestId('fact-title')).toHaveTextContent(POLICY.title);
    expect(chip).toHaveAttribute('data-open', 'true');
    // The same fact cited in the other turn is highlighted as well: it is what is open.
    const other = within(screen.getByTestId('chat-turn-0')).getAllByTestId('fact-link')[0];
    expect(other).toHaveAttribute('data-open', 'true');
    // Closing the panel takes the highlight away.
    fireEvent.click(within(panel).getByTestId('chat-panel-close'));
    expect(chip).not.toHaveAttribute('data-open');
  });

  it('hovering about 300ms shows the card: type, repo, confidence, title, excerpt, path and Open in Browse', async () => {
    const { onBrowseFact } = mount();
    const turn1 = await screen.findByTestId('chat-turn-1');
    const chip = await within(turn1).findByTestId('fact-link');
    vi.useFakeTimers();
    fireEvent.mouseEnter(chip);
    act(() => { vi.advanceTimersByTime(200); });
    expect(screen.queryByTestId('fact-card')).toBeNull();
    act(() => { vi.advanceTimersByTime(120); });
    const card = screen.getByTestId('fact-card');
    // The meta is Browse's own FactMetaLine (the user's rule: reuse), with
    // Browse's words: the kind flag, the type, origin, "conf", sources, the
    // repo drawn as Browse draws a mount.
    const meta = within(card).getByTestId('fact-meta');
    expect(within(meta).getByTestId('fact-kind-badge')).toHaveTextContent('pragmatic');
    expect(within(meta).getByTestId('fact-type-badge')).toHaveTextContent('policy');
    expect(within(meta).getByTestId('fact-origin-badge')).toHaveTextContent('authored');
    expect(within(meta).getByTestId('fact-confidence')).toHaveTextContent('conf 0.92');
    expect(meta).toHaveTextContent('4 sources');
    expect(within(meta).getByTestId('source-badge')).toHaveAttribute('data-repo', 'agentic-engineering');
    expect(within(card).getByTestId('fact-card-title')).toHaveTextContent(POLICY.title);
    expect(within(card).getByTestId('fact-card-excerpt')).toHaveTextContent(POLICY.excerpt);
    // The path is Browse's band path row.
    expect(within(card).getByTestId('fact-band-path')).toHaveTextContent(POLICY.path);
    fireEvent.click(within(card).getByText(PANEL_OPEN_IN_BROWSE));
    expect(onBrowseFact).toHaveBeenCalledWith('chat-a', { repo: 'agentic-engineering', path: POLICY.path });
    vi.useRealTimers();
  });

  it('a turn still streaming is not resolved: its text is still growing', async () => {
    const { fx } = mount({ ...snapshot([
      { kind: 'user', text: 'q' },
      { kind: 'assistant', text: 'see `3fc77970`' },
    ]), running: true });
    const turn = await screen.findByTestId('chat-turn-0');
    await within(turn).findByText('3fc77970');
    await new Promise(r => setTimeout(r, 30));
    expect(resolveCalls(fx)).toHaveLength(0);
    expect(screen.queryByTestId('fact-link')).toBeNull();
  });
});

function hexToRgb(hex: string): string {
  let h = hex.slice(1);
  if (h.length === 3) h = [...h].map(c => c + c).join('');
  const n = parseInt(h, 16);
  return `rgb(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255})`;
}
