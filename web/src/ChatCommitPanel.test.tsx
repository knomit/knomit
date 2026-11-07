import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent, cleanup, within } from '@testing-library/react';
import { ChatFacet } from './ChatFacet';
import { CommitHash } from './TimelineNav';
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

function mount(routes: Parameters<typeof stubFleetFetch>[0] = {}, onOpenInBrowse = vi.fn()) {
  const fx = stubFleetFetch({
    [`GET ${BASE}`]: { body: snapshot() },
    'POST /api/v1/facts/resolve': { body: [{ handle: 'abc1234', id: '', title: '', type: '', repo: 'agentic-engineering', branch: 'agent/test', path: '', excerpt: '', confidence: 0, commit: FULL }] },
    [`GET ${REPO}/commits/${FULL}`]: { body: DETAIL },
    ...routes,
  });
  const utils = render(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} onOpenInBrowse={onOpenInBrowse} repos={[]} />);
  return { fx, onOpenInBrowse, ...utils };
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
    expect(within(panel).getByTestId('chat-commit-message')).toHaveTextContent('learn(gate): three facts');
    expect(within(panel).getByTestId('timeline-author')).toHaveTextContent('Ada');
    expect(within(panel).getByTestId('chat-commit-date')).toHaveAttribute('title', '2026-11-01T10:00:00Z');
    // The hash IS the timeline's component.
    const ref = render(<CommitHash commit={FULL} />).container.firstElementChild!;
    expect(within(panel).getByTestId('chat-commit-hash').innerHTML).toBe(ref.outerHTML);
    // Panel chrome: the existing crumb; Browse has no repo-at-commit view, so no way there.
    expect(screen.getByTestId('chat-panel-crumb')).toHaveTextContent('agentic-engineering / abc1234');
    expect(screen.queryByTestId('chat-panel-browse')).toBeNull();
    expect(screen.getByTestId('chat-panel-footer')).toHaveTextContent('1 of 1');
    expect(screen.getByTestId('chat-facet')).toBeInTheDocument();
    // The chip stays highlighted while its commit is on screen.
    expect(screen.getByTestId('commit-link')).toHaveAttribute('data-open', 'true');
    expect(within(panel).getByTestId('commit-files-label')).toHaveTextContent('Files affected · 3');
    expect(within(panel).getAllByTestId('timeline-file-row').map(r => r.getAttribute('data-action'))).toEqual(['added', 'modified', 'deleted']);
  });

  it('a fact row opens the fact as it read at the commit, in the same panel, and Back returns to the commit', async () => {
    const { fx } = mount({
      [`GET ${REPO}/commits/${FULL}/facts/${CHANGED}`]: { body: factAt(CHANGED, 'The changed fact', 'THE OLD TEXT', FULL) },
      [`GET ${REPO}/facts/${CHANGED}`]: { body: factAt(CHANGED, 'The changed fact', 'THE NEW TEXT', NEWER) },
      [`GET ${REPO}/commits/${FULL}/facts/${CHANGED}/incoming`]: { body: { _embedded: { refs: [] } } },
      [`GET ${REPO}/commits/${FULL}/facts/${CHANGED}/outgoing`]: { body: { _embedded: { refs: [] } } },
      [`GET ${REPO}/facts/${CHANGED}/commits`]: { body: { _embedded: { commits: [{ commit: NEWER }, { commit: FULL }, { commit: OLDER }] } } },
    });
    await clickChip();
    await screen.findByTestId('chat-commit');
    fireEvent.click(rowFor('The changed fact'));
    const fact = await screen.findByTestId('chat-fact');
    expect(fact).toHaveTextContent('THE OLD TEXT');
    expect(fact).not.toHaveTextContent('THE NEW TEXT');
    expect(fx.calls.some(c => c.route === `GET ${REPO}/facts/${CHANGED}`)).toBe(false);
    // The band says which version: this commit, the second of three, not v3.
    expect(within(fact).getByTestId('chat-fact-commit')).toHaveTextContent('abc1234');
    expect(await within(fact).findByTestId('chat-fact-version')).toHaveTextContent('v2');
    expect(screen.getByTestId('chat-panel-footer')).toHaveTextContent('2 of 2');
    expect(screen.queryByTestId('chat-fact-retracted')).toBeNull();
    // The panel's existing Back.
    fireEvent.click(screen.getByTestId('chat-panel-back'));
    expect(await screen.findByTestId('chat-commit')).toBeInTheDocument();
    expect(screen.queryByTestId('chat-fact')).toBeNull();
  });

  it('Open in Browse from a fact read at a commit names that commit; a fact link\'s does not', async () => {
    const { onOpenInBrowse } = mount({
      [`GET ${REPO}/commits/${FULL}/facts/${CHANGED}`]: { body: factAt(CHANGED, 'The changed fact', 'x', FULL) },
    });
    await clickChip();
    await screen.findByTestId('chat-commit');
    fireEvent.click(rowFor('The changed fact'));
    fireEvent.click(await screen.findByTestId('chat-panel-browse'));
    expect(onOpenInBrowse).toHaveBeenCalledWith('agentic-engineering', CHANGED, FULL);
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
    const fact = await screen.findByTestId('chat-fact');
    expect(fact).toHaveTextContent('WHAT IT SAID');
    expect(urls()).toContain(`${REPO}/commits/${FULL}/facts/${GONE}?fallback=before`);
    expect(urls().filter(u => u.includes(`/commits/${FULL}/facts/${GONE}/`) && /incoming|outgoing/.test(u)).every(u => u.endsWith('?fallback=before'))).toBe(true);
    expect(within(fact).getByTestId('chat-fact-retracted')).toHaveTextContent('retracted at abc1234');
    // The band names the version on screen, the one before the retraction.
    expect(within(fact).getByTestId('chat-fact-commit')).toHaveTextContent('1234567');
    expect(await within(fact).findByTestId('chat-fact-version')).toHaveTextContent('v1');
  });

  it('a commit with no facts shows only the commit: no label and no rows, whatever else the server listed', async () => {
    mount({
      [`GET ${REPO}/commits/${FULL}`]: { body: { ...DETAIL, files: [
        { path: 'kb/.drafts/x.md', action: 'added' },
        { path: 'kb/notes.txt', action: 'added' },
      ] } },
    });
    await clickChip();
    const panel = await screen.findByTestId('chat-commit');
    expect(panel).toHaveTextContent('learn(gate): three facts');
    expect(screen.queryByTestId('commit-files-label')).toBeNull();
    expect(screen.queryByTestId('timeline-file-row')).toBeNull();
  });

  it('a very large commit lists every fact in a box that scrolls, its rows cut rather than wrapped', async () => {
    const long = 'x'.repeat(400);
    const files = Array.from({ length: 500 }, (_, i) => ({ path: `kb/decisions/${long}/${i}.md`, action: 'modified', title: `Fact ${i} ${long}` }));
    mount({ [`GET ${REPO}/commits/${FULL}`]: { body: { ...DETAIL, files } } });
    await clickChip();
    const panel = await screen.findByTestId('chat-commit');
    expect(panel.style.overflowY).toBe('auto');
    expect(panel.style.minHeight).toBe('0px');
    expect(screen.getByTestId('commit-files-label')).toHaveTextContent('Files affected · 500');
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

  it('a fact read at the commit that fails shows the error line, and Back still returns to the commit', async () => {
    mount({ [`GET ${REPO}/commits/${FULL}/facts/${CHANGED}`]: problem(404, 'not_found', 'no fact at that path') });
    await clickChip();
    await screen.findByTestId('chat-commit');
    fireEvent.click(rowFor('The changed fact'));
    expect(await screen.findByTestId('chat-fact-error')).toHaveTextContent('no fact at that path');
    fireEvent.click(screen.getByTestId('chat-panel-back'));
    expect(await screen.findByTestId('chat-commit')).toBeInTheDocument();
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

  it('Esc closes it and the chip reopens it without growing the history', async () => {
    mount();
    await clickChip();
    await screen.findByTestId('chat-commit');
    fireEvent.keyDown(window, { key: 'Escape' });
    await waitFor(() => expect(screen.queryByTestId('chat-side-panel')).toBeNull());
    expect(screen.getByTestId('commit-link')).not.toHaveAttribute('data-open');
    fireEvent.click(screen.getByTestId('commit-link'));
    await screen.findByTestId('chat-commit');
    expect(screen.getByTestId('chat-panel-footer')).toHaveTextContent('1 of 1');
  });
});
