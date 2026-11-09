import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent, cleanup, within, act } from '@testing-library/react';
import { ChatFacet } from './ChatFacet';
import { CommitBlock } from './TimelineNav';
import { resetFactCache } from './chatFactLinks';
import { installFakeEventSource, uninstallFakeEventSource } from './testEventSource';
import { problem, stubFleetFetch } from './fleetTestFetch';
import type { ChatSnapshot } from './chatApi';

// A real commit chip, clicked in a real ChatFacet: only fetch is stubbed. The
// branch comes from the resolver's answer, so these routes prove which branch
// the panel reads; an unrouted read answers 404 and fails the assertion.

const CHAT = { id: 'chat-a', provider: 'anthropic', model: 'claude-test-x' };
const BASE = '/api/v1/chats/chat-a';
const REPO = '/api/v1/repos/agentic-engineering/branches/agent:test';
const FULL = 'abc1234' + 'f'.repeat(33);
const OLDER = '1234567' + 'e'.repeat(33);
const NEWER = '7654321' + 'd'.repeat(33);
const CHANGED = 'kb/decisions/changed/aaaaaaa1.md';
const GONE = 'kb/decisions/gone/aaaaaaa2.md';
const DETAIL = {
  commit: FULL, date: '2026-11-01T10:00:00Z', message: 'learn(gate): three facts', operation: 'learn',
  author: { name: 'Ada', email: 'ada@example.com' },
  files: [
    { path: 'kb/decisions/added/aaaaaaa0.md', action: 'added', title: 'The added fact' },
    { path: CHANGED, action: 'modified', title: 'The changed fact' },
    { path: GONE, action: 'deleted', title: 'The retracted fact' },
  ],
};
const factAt = (path: string, title: string, body: string, commit: string) => ({
  path, title, type: 'observation', kind: 'epistemic', body, domain: [], confidence: 0.9, sources: 1, entities: [], refs: [],
  as_of: { commit, date: '2026-10-20T10:00:00Z' },
});

const snapshot = (): ChatSnapshot => ({
  ...CHAT, running: false, grants: [], pending_approvals: [], items: [
    { kind: 'user', text: 'q' },
    { kind: 'tool_call', id: 'tu_1', name: 'knomit_query', state: 'done', arguments: { text: 'x', repo: 'agentic-engineering' } },
    { kind: 'assistant', text: 'Fixed in `abc1234`.' },
  ],
});

const twoChips = (): ChatSnapshot => ({
  ...snapshot(), items: [
    { kind: 'user', text: 'q' },
    { kind: 'assistant', text: 'Fixed in `abc1234`, then in `9999999`.' },
  ],
});

function mount(routes: Parameters<typeof stubFleetFetch>[0] = {}, onBrowseFact = vi.fn()) {
  const fx = stubFleetFetch({
    [`GET ${BASE}`]: { body: snapshot() },
    'POST /api/v1/facts/resolve': { body: [{ handle: 'abc1234', id: '', title: '', type: '', repo: 'agentic-engineering', branch: 'agent/test', path: '', excerpt: '', confidence: 0, commit: FULL }] },
    [`GET ${REPO}/commits/${FULL}`]: { body: DETAIL },
    ...routes,
  });
  const utils = render(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} onBrowseFact={onBrowseFact} repos={[]} />);
  return { fx, onBrowseFact, ...utils };
}

const clickChip = async () => fireEvent.click(await screen.findByTestId('commit-link'));
const rows = () => screen.getAllByTestId('timeline-file-row');
const rowFor = (title: string) => rows().find(r => r.textContent?.includes(title))!;
const urls = () => (fetch as unknown as { mock: { calls: unknown[][] } }).mock.calls.map(c => String(c[0]));

beforeEach(() => { installFakeEventSource(); resetFactCache(); localStorage.clear(); });
afterEach(() => { cleanup(); uninstallFakeEventSource(); vi.unstubAllGlobals(); });

describe('a commit chip opens the commit in the side panel', () => {
  it('shows message, author, time and hash, reads the resolver\'s branch, and stays in the chat', async () => {
    mount();
    expect(screen.queryByTestId('chat-side-panel')).toBeNull();
    await clickChip();
    const panel = await screen.findByTestId('chat-commit');
    expect(panel).toHaveTextContent('learn(gate): three facts');
    expect(within(panel).getByTestId('timeline-author')).toHaveTextContent('Ada');
    expect(within(panel).getByTestId('commit-hash')).toHaveTextContent('abc1234');
    // The block IS the timeline's: rendered with the same inputs it is the same DOM.
    const noop = () => {};
    const ref = render(<CommitBlock commit={FULL} operation="learn" date={DETAIL.date} message={DETAIL.message} active onRowClick={noop}
      detail={DETAIL} detailOpen filesLabel="Facts affected" files={DETAIL.files} onOpenFile={noop} />).container;
    expect(panel.innerHTML).toBe(ref.innerHTML);
    // Panel chrome: the existing crumb; Browse has no repo-at-commit view, so no way there.
    expect(screen.getByTestId('chat-panel-crumb')).toHaveTextContent('agentic-engineering / abc1234');
    expect(screen.queryByTestId('chat-panel-browse')).toBeNull();
    expect(screen.queryByTestId('chat-panel-back')).toBeNull();
    expect(screen.getByTestId('chat-facet')).toBeInTheDocument();
    // The chip stays highlighted while its commit is on screen.
    expect(screen.getByTestId('commit-link')).toHaveAttribute('data-open', 'true');
    expect(within(panel).getByTestId('commit-files-label')).toHaveTextContent('Facts affected · 3');
    expect(within(panel).getAllByTestId('timeline-file-row').map(r => r.getAttribute('data-action'))).toEqual(['added', 'modified', 'deleted']);
  });

  it('a fact row opens the fact as it read at the commit, in the same panel, and its chip returns to the commit', async () => {
    const { fx } = mount({
      [`GET ${REPO}/commits/${FULL}/facts/${CHANGED}`]: { body: factAt(CHANGED, 'The changed fact', 'THE OLD TEXT', FULL) },
      [`GET ${REPO}/facts/${CHANGED}`]: { body: factAt(CHANGED, 'The changed fact', 'THE NEW TEXT', NEWER) },
      [`GET ${REPO}/commits/${FULL}/facts/${CHANGED}/incoming`]: { body: { _embedded: { refs: [{ path: 'a.md', title: 'A', commit: 'c1' }, { path: 'b.md', title: 'B', commit: 'c2' }] } } },
      [`GET ${REPO}/commits/${FULL}/facts/${CHANGED}/outgoing`]: { body: { _embedded: { refs: [{ path: 'c.md', title: 'C', commit: 'c3' }] } } },
      // HEAD has other edges: a read there would show them, not just lose the counts.
      [`GET ${REPO}/facts/${CHANGED}/incoming`]: { body: { _embedded: { refs: [{ path: 'x.md', title: 'X', commit: 'c4' }] } } },
      [`GET ${REPO}/facts/${CHANGED}/outgoing`]: { body: { _embedded: { refs: [] } } },
      [`GET ${REPO}/facts/${CHANGED}/commits`]: { body: { _embedded: { commits: [{ commit: NEWER }, { commit: FULL }, { commit: OLDER }] } } },
    });
    await clickChip();
    await screen.findByTestId('chat-commit');
    fireEvent.click(rowFor('The changed fact'));
    const fact = await screen.findByTestId('fact-view');
    expect(fact).toHaveTextContent('THE OLD TEXT');
    expect(fact).not.toHaveTextContent('THE NEW TEXT');
    expect(fx.calls.some(c => c.route === `GET ${REPO}/facts/${CHANGED}`)).toBe(false);
    // The band's history button is there, and counts nothing (no vN).
    expect((await within(fact).findByTestId('version-walker')).textContent).toBe('');
    // The edge counts are this commit's too.
    await waitFor(() => expect(within(fact).getByTestId('connections-in')).toHaveTextContent('2'));
    expect(within(fact).getByTestId('connections-out')).toHaveTextContent('1');
    expect(screen.queryByTestId('retracted-version-badge')).toBeNull();
    // No back control: the commit's chip returns.
    expect(screen.queryByTestId('chat-panel-back')).toBeNull();
    fireEvent.click(screen.getByTestId('commit-link'));
    expect(await screen.findByTestId('chat-commit')).toBeInTheDocument();
    expect(screen.queryByTestId('fact-view')).toBeNull();
  });

  it('from a fact read at a commit, the version lands at that commit; a pick from cited by or cites lands at its edge\'s pin, on its branch', async () => {
    const { onBrowseFact } = mount({
      [`GET ${REPO}/commits/${FULL}/facts/${CHANGED}`]: { body: factAt(CHANGED, 'The changed fact', 'x', FULL) },
      [`GET ${REPO}/commits/${FULL}/facts/${CHANGED}/incoming`]: { body: { _embedded: { refs: [{ path: 'a.md', title: 'A', commit: 'c1' }] } } },
      [`GET ${REPO}/commits/${FULL}/facts/${CHANGED}/outgoing`]: { body: { _embedded: { refs: [{ path: 'c.md', title: 'C', commit: 'c3' }] } } },
      [`GET ${REPO}/facts/${CHANGED}/commits`]: { body: { _embedded: { commits: [{ commit: NEWER }, { commit: FULL }] } } },
    });
    await clickChip();
    await screen.findByTestId('chat-commit');
    fireEvent.click(rowFor('The changed fact'));
    await screen.findByTestId('fact-view');
    const base = { repo: 'agentic-engineering', branch: 'agent/test', path: CHANGED, commit: FULL };
    fireEvent.click(await screen.findByTestId('version-walker'));
    expect(onBrowseFact).toHaveBeenLastCalledWith('chat-a', base); // that commit, not the newest
    await waitFor(() => expect(screen.getByTestId('connections-in')).toHaveTextContent('1'));
    // The menus open in the panel; only a pick goes to Browse.
    const calls = onBrowseFact.mock.calls.length;
    fireEvent.click(screen.getByTestId('connections-in'));
    expect(onBrowseFact).toHaveBeenCalledTimes(calls);
    fireEvent.click(within(screen.getByTestId('connections-panel')).getByText('A'));
    expect(onBrowseFact).toHaveBeenLastCalledWith('chat-a', { ...base, path: 'a.md', commit: 'c1' });
    fireEvent.click(screen.getByTestId('connections-out'));
    fireEvent.click(within(screen.getByTestId('connections-panel')).getByText('C'));
    expect(onBrowseFact).toHaveBeenLastCalledWith('chat-a', { ...base, path: 'c.md', commit: 'c3' });
  });

  it('a fact the commit retracted opens its last version with ?fallback=before and says it was retracted, not a dead panel', async () => {
    mount({
      // The plain read is 404 by design; only the fallback read answers (stub: one route, so the URL is asserted below).
      [`GET ${REPO}/commits/${FULL}/facts/${GONE}`]: { body: factAt(GONE, 'The retracted fact', 'WHAT IT SAID', OLDER) },
      [`GET ${REPO}/commits/${FULL}/facts/${GONE}/incoming`]: { body: { _embedded: { refs: [] } } },
      [`GET ${REPO}/commits/${FULL}/facts/${GONE}/outgoing`]: { body: { _embedded: { refs: [] } } },
      [`GET ${REPO}/facts/${GONE}/commits`]: { body: { _embedded: { commits: [{ commit: FULL, operation: 'retract' }, { commit: OLDER }] } } },
    });
    await clickChip();
    await screen.findByTestId('chat-commit');
    const row = rowFor('The retracted fact');
    expect(row).toBeEnabled();
    fireEvent.click(row);
    const fact = await screen.findByTestId('fact-view');
    expect(fact).toHaveTextContent('WHAT IT SAID');
    expect(urls()).toContain(`${REPO}/commits/${FULL}/facts/${GONE}?fallback=before`);
    expect(urls().filter(u => u.includes(`/commits/${FULL}/facts/${GONE}/`) && /incoming|outgoing/.test(u)).every(u => u.endsWith('?fallback=before'))).toBe(true);
    // Browse's own badge names the commit that retracted it.
    expect(within(fact).getByTestId('retracted-version-badge')).toHaveTextContent('retracted at abc1234');
    // The history button is there, a bare history icon (no vN).
    expect((await within(fact).findByTestId('version-walker')).textContent).toBe('');
  });

  it('a commit with no facts shows the commit and an empty list, whatever else the server listed, and no error', async () => {
    mount({
      [`GET ${REPO}/commits/${FULL}`]: { body: { ...DETAIL, files: [
        { path: 'kb/.drafts/x.md', action: 'added' },
        { path: 'kb/notes.txt', action: 'added' },
      ] } },
    });
    await clickChip();
    const panel = await screen.findByTestId('chat-commit');
    expect(panel).toHaveTextContent('learn(gate): three facts');
    expect(screen.getByTestId('commit-files-label')).toHaveTextContent('Facts affected · 0');
    expect(screen.queryByTestId('timeline-file-row')).toBeNull();
    expect(screen.queryByTestId('chat-commit-error')).toBeNull();
  });

  it('a very large commit lists every fact in a box that scrolls, its rows cut rather than wrapped', async () => {
    const long = 'x'.repeat(400);
    const files = Array.from({ length: 500 }, (_, i) => ({ path: `kb/decisions/${long}/${i}.md`, action: 'modified', title: `Fact ${i} ${long}` }));
    mount({ [`GET ${REPO}/commits/${FULL}`]: { body: { ...DETAIL, files } } });
    await clickChip();
    const panel = await screen.findByTestId('chat-commit');
    expect(panel.style.overflowY).toBe('auto');
    expect(panel.style.minHeight).toBe('0px');
    expect(screen.getByTestId('commit-files-label')).toHaveTextContent('Facts affected · 500');
    expect(rows()).toHaveLength(500);
    const title = rows()[0].querySelectorAll('span')[2] as HTMLElement;
    expect(title.style.whiteSpace).toBe('nowrap');
    expect(title.style.textOverflow).toBe('ellipsis');
  });

  it('commitDetail failing shows the server\'s sentence in the panel\'s error line, with the chrome still there', async () => {
    mount({ [`GET ${REPO}/commits/${FULL}`]: problem(404, 'commit_not_in_repo', 'this repo does not hold that commit') });
    await clickChip();
    const line = await screen.findByTestId('chat-commit-error');
    expect(line).toHaveTextContent('this repo does not hold that commit');
    expect(line.textContent).not.toContain('/api/v1');
    expect(screen.getByRole('alert')).toBeInTheDocument();
    expect(screen.getByTestId('chat-panel-close')).toBeInTheDocument();
    expect(screen.queryByTestId('chat-commit')).toBeNull();
  });

  it('a fact read at the commit that fails shows Browse\'s error with a visible close, and its chip returns to the commit', async () => {
    mount({ [`GET ${REPO}/commits/${FULL}/facts/${CHANGED}`]: problem(404, 'not_found', 'no fact at that path') });
    await clickChip();
    await screen.findByTestId('chat-commit');
    fireEvent.click(rowFor('The changed fact'));
    expect(await screen.findByText(/no fact at that path/)).toBeInTheDocument();
    expect(screen.getByTestId('chat-panel-close')).toBeInTheDocument();
    fireEvent.click(screen.getByTestId('commit-link'));
    expect(await screen.findByTestId('chat-commit')).toBeInTheDocument();
  });

  // The panel must tell commits apart: otherwise the second chip shows the first.
  it('a second chip in the same repo opens its own commit', async () => {
    const second = '9999999' + 'a'.repeat(33);
    mount({
      [`GET ${BASE}`]: { body: twoChips() },
      'POST /api/v1/facts/resolve': { body: [
        { handle: 'abc1234', id: '', title: '', type: '', repo: 'agentic-engineering', branch: 'agent/test', path: '', excerpt: '', confidence: 0, commit: FULL },
        { handle: '9999999', id: '', title: '', type: '', repo: 'agentic-engineering', branch: 'agent/test', path: '', excerpt: '', confidence: 0, commit: second },
      ] },
      [`GET ${REPO}/commits/${second}`]: { body: { ...DETAIL, commit: second, message: 'the second commit' } },
    });
    await screen.findAllByTestId('commit-link');
    fireEvent.click(screen.getAllByTestId('commit-link')[0]);
    expect(await screen.findByTestId('chat-commit')).toHaveTextContent('learn(gate): three facts');
    fireEvent.click(screen.getAllByTestId('commit-link')[1]);
    await waitFor(() => expect(screen.getByTestId('chat-commit')).toHaveTextContent('the second commit'));
    expect(screen.getByTestId('chat-panel-crumb')).toHaveTextContent('agentic-engineering / 9999999');
  });

  // The stale guards. `hold` is the commit whose read waits; everything else answers at once.
  const SECOND = '9999999' + 'a'.repeat(33);
  function twoCommits(hold: string, second: { status?: number; body: unknown }) {
    let release: () => void = () => {};
    stubFleetFetch({
      [`GET ${BASE}`]: { body: twoChips() },
      'POST /api/v1/facts/resolve': { body: [
        { handle: 'abc1234', id: '', title: '', type: '', repo: 'agentic-engineering', branch: 'agent/test', path: '', excerpt: '', confidence: 0, commit: FULL },
        { handle: '9999999', id: '', title: '', type: '', repo: 'agentic-engineering', branch: 'agent/test', path: '', excerpt: '', confidence: 0, commit: SECOND },
      ] },
      [`GET ${REPO}/commits/${FULL}`]: { body: DETAIL },
      [`GET ${REPO}/commits/${SECOND}`]: second,
    });
    const inner = fetch as unknown as (input: string, init?: RequestInit) => Promise<Response>;
    vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
      if (String(input).endsWith(`/commits/${hold}`)) await new Promise<void>(r => { release = r; });
      return inner(input, init);
    }));
    render(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} repos={[]} />);
    return { release: () => act(async () => { release(); await new Promise(r => setTimeout(r, 10)); }) };
  }
  const SECOND_BODY = { ...DETAIL, commit: SECOND, message: 'the second commit' };

  it('a slow answer for the previous commit never shows for the one now open', async () => {
    const { release } = twoCommits(FULL, { body: SECOND_BODY });
    await screen.findAllByTestId('commit-link');
    fireEvent.click(screen.getAllByTestId('commit-link')[0]);
    expect(await screen.findByTestId('chat-commit-loading')).toBeInTheDocument();
    fireEvent.click(screen.getAllByTestId('commit-link')[1]);
    expect(await screen.findByTestId('chat-commit')).toHaveTextContent('the second commit');
    await release();
    expect(screen.getByTestId('chat-commit')).toHaveTextContent('the second commit');
  });

  it('while the next commit loads, the previous one is not left on screen', async () => {
    const { release } = twoCommits(SECOND, { body: SECOND_BODY });
    await screen.findAllByTestId('commit-link');
    fireEvent.click(screen.getAllByTestId('commit-link')[0]);
    expect(await screen.findByTestId('chat-commit')).toHaveTextContent('learn(gate): three facts');
    fireEvent.click(screen.getAllByTestId('commit-link')[1]);
    expect(await screen.findByTestId('chat-commit-loading')).toBeInTheDocument();
    expect(screen.queryByTestId('chat-commit')).toBeNull();
    await release();
    expect(screen.getByTestId('chat-commit')).toHaveTextContent('the second commit');
  });

  it('a slow answer for the previous commit does not wipe the error of the one now open', async () => {
    const { release } = twoCommits(FULL, problem(404, 'commit_not_in_repo', 'that commit is not here'));
    await screen.findAllByTestId('commit-link');
    fireEvent.click(screen.getAllByTestId('commit-link')[0]);
    fireEvent.click(screen.getAllByTestId('commit-link')[1]);
    expect(await screen.findByTestId('chat-commit-error')).toHaveTextContent('that commit is not here');
    await release();
    expect(screen.getByTestId('chat-commit-error')).toHaveTextContent('that commit is not here');
    expect(screen.queryByTestId('chat-commit-loading')).toBeNull();
  });

  it('the chip returns from a fact to the same place in a long list, without reading the commit again', async () => {
    const files = Array.from({ length: 300 }, (_, i) => ({ path: `kb/decisions/f${i}.md`, action: 'modified', title: `Fact ${i}` }));
    const { fx } = mount({
      [`GET ${REPO}/commits/${FULL}`]: { body: { ...DETAIL, files } },
      [`GET ${REPO}/commits/${FULL}/facts/kb/decisions/f250.md`]: { body: factAt('kb/decisions/f250.md', 'Fact 250', 'x', FULL) },
    });
    await clickChip();
    const panel = await screen.findByTestId('chat-commit');
    panel.scrollTop = 4321;
    fireEvent.scroll(panel);
    fireEvent.click(rowFor('Fact 250'));
    await screen.findByTestId('fact-view');
    fireEvent.click(screen.getByTestId('commit-link'));
    const back = await screen.findByTestId('chat-commit');
    expect(back).not.toBe(panel);
    expect(back.scrollTop).toBe(4321);
    expect(fx.calls.filter(c => c.route === `GET ${REPO}/commits/${FULL}`)).toHaveLength(1);
  });

  it('a failed read is not remembered: opening the commit again asks again', async () => {
    const { fx } = mount({ [`GET ${REPO}/commits/${FULL}`]: problem(503, 'unavailable', 'not readable now') });
    await clickChip();
    await screen.findByTestId('chat-commit-error');
    fx.set(`GET ${REPO}/commits/${FULL}`, { body: DETAIL });
    fireEvent.click(screen.getByTestId('chat-panel-close'));
    await waitFor(() => expect(screen.queryByTestId('chat-side-panel')).toBeNull());
    fireEvent.click(screen.getByTestId('commit-link'));
    expect(await screen.findByTestId('chat-commit')).toHaveTextContent('learn(gate): three facts');
  });

  it('a reload starts with the panel closed, as for a fact or a tool call', async () => {
    const first = mount();
    await clickChip();
    await screen.findByTestId('chat-commit');
    first.unmount();
    mount();
    await screen.findByTestId('commit-link');
    expect(screen.queryByTestId('chat-side-panel')).toBeNull();
  });

  // kb/decisions/web/chat/side-panel/c12285c0: Stop always wins, Focus included.
  // SABOTAGE: useMatFocus takes Esc without asking (yieldsEsc): Focus goes off, no cancel.
  describe('Esc in the panel\'s materialize Focus', () => {
    const inFocus = async (running: boolean) => {
      const fx = mount({
        [`GET ${BASE}`]: { body: { ...snapshot(), running, items: [...snapshot().items, ...(running ? [{ kind: 'user', text: 'and?' } as const] : [])] } },
        [`POST ${BASE}/cancel`]: { body: {} },
        [`GET ${REPO}/commits/${FULL}/facts/${CHANGED}`]: { body: factAt(CHANGED, 'The changed fact', 'x', FULL) },
      }).fx;
      await clickChip();
      await screen.findByTestId('chat-commit');
      fireEvent.click(rowFor('The changed fact'));
      fireEvent.click(await screen.findByTestId('materialize-btn'));
      await screen.findByTestId('mat-actions');
      expect(document.documentElement.dataset.matFocus).toBe('1');
      return fx;
    };
    afterEach(() => { delete document.documentElement.dataset.matFocus; });
    const cancels = (fx: ReturnType<typeof stubFleetFetch>) => fx.calls.filter(c => c.route === `POST ${BASE}/cancel`);

    it('with a turn running it is Stop, and Focus stays', async () => {
      const fx = await inFocus(true);
      // Spent, so the panel's listener leaves it alone: a real browser has already
      // rendered running:false by then. SABOTAGE: ChatPanel does not preventDefault.
      expect(fireEvent.keyDown(document.body, { key: 'Escape' })).toBe(false);
      await waitFor(() => expect(cancels(fx)).toHaveLength(1));
      expect(document.documentElement.dataset.matFocus).toBe('1');
      expect(screen.getByTestId('chat-side-panel')).toBeInTheDocument();
    });
    it('with no turn running it leaves Focus, then closes the panel', async () => {
      const fx = await inFocus(false);
      fireEvent.keyDown(document.body, { key: 'Escape' });
      await waitFor(() => expect(document.documentElement.dataset.matFocus).toBeUndefined());
      expect(screen.getByTestId('chat-side-panel')).toBeInTheDocument();
      fireEvent.keyDown(document.body, { key: 'Escape' });
      await waitFor(() => expect(screen.queryByTestId('chat-side-panel')).toBeNull());
      expect(cancels(fx)).toHaveLength(0);
    });
  });

  it('Esc closes it and the chip reopens it', async () => {
    mount();
    await clickChip();
    await screen.findByTestId('chat-commit');
    fireEvent.keyDown(window, { key: 'Escape' });
    await waitFor(() => expect(screen.queryByTestId('chat-side-panel')).toBeNull());
    expect(screen.getByTestId('commit-link')).not.toHaveAttribute('data-open');
    fireEvent.click(screen.getByTestId('commit-link'));
    await screen.findByTestId('chat-commit');
  });
});
