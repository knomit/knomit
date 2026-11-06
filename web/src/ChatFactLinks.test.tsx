import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, act, fireEvent, cleanup, within } from '@testing-library/react';
import { ChatFacet } from './ChatFacet';
import { installFakeEventSource, uninstallFakeEventSource } from './testEventSource';
import { stubFleetFetch } from './fleetTestFetch';
import type { ChatSnapshot } from './chatApi';
import { typeStyles, repoHue } from './utils';
import { PANEL_OPEN_IN_BROWSE } from './chatCopy';

const CHAT = { id: 'chat-a', provider: 'anthropic', model: 'claude-test-x' };
const BASE = '/api/v1/chats/chat-a';
const RESOLVE = 'POST /api/v1/facts/resolve';
const FACT_BASE = '/api/v1/repos/agentic-engineering/branches/agent:test/facts';

const POLICY = {
  handle: '3fc77970', id: '3fc77970', title: 'Agent config files are executable; treat them as code', type: 'policy',
  repo: 'agentic-engineering', branch: 'agent/test', path: 'kb/invariants/security/3fc77970.md',
  excerpt: 'Files an agent loads on start change what it can do.', confidence: 0.92,
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

function mount(snap: ChatSnapshot = TWO_TURNS, onOpenInBrowse = vi.fn()) {
  const fx = stubFleetFetch({
    [`GET ${BASE}`]: { body: snap },
    [RESOLVE]: resolved,
    [`GET ${FACT_BASE}/${POLICY.path}`]: { body: { path: POLICY.path, title: POLICY.title, type: 'policy', kind: 'pragmatic', body: 'Review them like code.', domain: [], confidence: 0.92, sources: 4, entities: [], refs: [] } },
  });
  const repos = [{ name: 'agentic-engineering', id: '87b70fb1aeb1' }];
  const utils = render(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} onOpenInBrowse={onOpenInBrowse} repos={repos} />);
  return { fx, onOpenInBrowse, ...utils };
}
const resolveCalls = (fx: ReturnType<typeof stubFleetFetch>) => fx.calls.filter(c => c.route === RESOLVE);

beforeEach(() => { installFakeEventSource(); });
afterEach(() => { cleanup(); uninstallFakeEventSource(); vi.unstubAllGlobals(); vi.useRealTimers(); });

describe('chat fact links', () => {
  it('resolves each finished turn\'s candidates in ONE call per turn, and never again for the same text', async () => {
    const { fx } = mount();
    await screen.findAllByTestId('fact-link');
    await waitFor(() => expect(resolveCalls(fx)).toHaveLength(2));
    expect(resolveCalls(fx).map(c => c.body)).toEqual([
      // deadbeef looks like a short id, so it is asked about; the server omits it.
      { handles: ['3fc77970', 'deadbeef', 'aaaaaaa1', 'aaaaaaa2', 'aaaaaaa3'] },
      { handles: ['kb/invariants/security/3fc77970.md'] },
    ]);
    // A re-render with the same transcript (a reconnect's snapshot) asks nothing more.
    fireEvent.change(screen.getByTestId('chat-input'), { target: { value: 'x' } });
    await new Promise(r => setTimeout(r, 30));
    expect(resolveCalls(fx)).toHaveLength(2);
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
    const { onOpenInBrowse } = mount();
    const turn1 = await screen.findByTestId('chat-turn-1');
    const chip = await within(turn1).findByTestId('fact-link');
    expect(screen.queryByTestId('chat-side-panel')).toBeNull();

    fireEvent.click(chip, { metaKey: true });
    expect(onOpenInBrowse).toHaveBeenCalledWith('agentic-engineering', POLICY.path);
    expect(screen.queryByTestId('chat-side-panel')).toBeNull();

    fireEvent.click(chip);
    const panel = await screen.findByTestId('chat-side-panel');
    expect(await within(panel).findByTestId('chat-fact-title')).toHaveTextContent(POLICY.title);
    expect(chip).toHaveAttribute('data-open', 'true');
    // The same fact cited in the other turn is highlighted as well: it is what is open.
    const other = within(screen.getByTestId('chat-turn-0')).getAllByTestId('fact-link')[0];
    expect(other).toHaveAttribute('data-open', 'true');
    // Closing the panel takes the highlight away.
    fireEvent.click(within(panel).getByTestId('chat-panel-close'));
    expect(chip).not.toHaveAttribute('data-open');
  });

  it('hovering about 300ms shows the card: type, repo, confidence, title, excerpt, path and Open in Browse', async () => {
    const { onOpenInBrowse } = mount();
    const turn1 = await screen.findByTestId('chat-turn-1');
    const chip = await within(turn1).findByTestId('fact-link');
    vi.useFakeTimers();
    fireEvent.mouseEnter(chip);
    act(() => { vi.advanceTimersByTime(200); });
    expect(screen.queryByTestId('fact-card')).toBeNull();
    act(() => { vi.advanceTimersByTime(120); });
    const card = screen.getByTestId('fact-card');
    expect(within(card).getByTestId('fact-card-type')).toHaveTextContent('policy');
    expect(within(card).getByTestId('repo-link')).toHaveTextContent('agentic-engineering');
    // The confidence is the number only: its "confidence" label is unapproved copy.
    expect(within(card).getByTestId('fact-card-confidence')).toHaveTextContent('0.92');
    expect(within(card).getByTestId('fact-card-title')).toHaveTextContent(POLICY.title);
    expect(within(card).getByTestId('fact-card-excerpt')).toHaveTextContent(POLICY.excerpt);
    expect(within(card).getByTestId('fact-card-path')).toHaveTextContent(POLICY.path);
    fireEvent.click(within(card).getByText(PANEL_OPEN_IN_BROWSE));
    expect(onOpenInBrowse).toHaveBeenCalledWith('agentic-engineering', POLICY.path);
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
