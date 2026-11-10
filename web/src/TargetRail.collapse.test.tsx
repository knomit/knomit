import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, within, cleanup } from '@testing-library/react';
import { TargetRail } from './TargetRail';
import type { RepoInfo, Lens, ArchivedRepo } from './api';
import type { ChatInfo } from './chatApi';
import { RAIL_COLLAPSED_KEY, loadRailCollapsed, saveRailCollapsed } from './repoSelection';
import { __resetRepoCreatesForTest } from './useRepoCreates';

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>();
  return { ...actual, api: { ...actual.api, listRepoCreates: vi.fn().mockResolvedValue([]) } };
});

// Collapsible rail sections (variant C, 2026-11-09): a section header is its
// own toggle, a collapsed one shows its count (and, for Chats, the attention
// dot), the state is remembered per viewer, the search and the `+` both open
// what they need to show.

const repos: RepoInfo[] = [
  { name: 'ops-kb', uid: 'u-ops' },
  { name: 'core-kb', uid: 'u-core' },
  { name: 'b', uid: 'u-b' },
  { name: 'ghost', uid: 'u-ghost', state: 'missing', detail: 'database file not found' },
  // The fleet repository is the Fleet row, not a repository: never counted here.
  { name: 'fleet-kb', uid: 'u-fleet', fleet: true },
];
const lenses: Lens[] = [
  { name: 'ops-desk', write: { uid: 'u-ops', name: 'ops-kb' }, reads: [{ uid: 'u-ops', name: 'ops-kb' }] },
  { name: 'dev', write: { uid: 'u-core', name: 'core-kb' }, reads: [{ uid: 'u-core', name: 'core-kb' }] },
];
const archived: ArchivedRepo[] = [
  { id: 'q1', name: 'old-notes' }, { id: 'q2', name: 'scratch' }, { id: 'q3', name: 'demo-kb' },
] as ArchivedRepo[];
const providers = [{ name: 'prov-a', kind: 'anthropic', models: [] }];
const chat = (id: string, i: number, over: Partial<ChatInfo> = {}): ChatInfo => ({
  id, provider: 'prov-a', model: 'm', name: 'Model M', title: `title ${id}`, state: 'idle', turns: 1,
  last_activity: `2026-11-06T10:00:0${i}Z`, started_at: '2026-11-06T09:00:00Z', ...over,
});
const sevenChats = ['c1', 'c2', 'c3', 'c4', 'c5', 'c6', 'c7'].map((id, i) => chat(id, i));

function renderRail(over: Partial<React.ComponentProps<typeof TargetRail>> = {}) {
  return render(
    <TargetRail repos={repos} lenses={lenses} archived={archived} context={{ kind: 'repo', repo: 'b' }} currentRepo="b"
      remoteError="" serverReadOnly={false} onSelect={vi.fn()} chatProviders={providers} chatsAvailable
      chats={sevenChats} {...over} />,
  );
}

const toggle = (s: string) => screen.getByTestId(`rail-${s}-toggle`);
const repoNames = () => screen.queryAllByTestId(/^rail-repo-name-/).map(el => el.textContent);
const lensNames = () => screen.queryAllByTestId(/^rail-lens-name-/).map(el => el.textContent);
const chatTitles = () => screen.queryAllByTestId(/^rail-chat-title-/).map(e => e.textContent);

beforeEach(() => { __resetRepoCreatesForTest(); localStorage.clear(); });
afterEach(() => { cleanup(); vi.restoreAllMocks(); localStorage.clear(); });

describe('rail sections: the header is the toggle', () => {
  it('the icon and the title are ONE button with aria-expanded; the + is a separate button; no chevron', () => {
    renderRail();
    for (const s of ['chats', 'repos', 'lenses', 'fleet']) {
      const t = toggle(s);
      expect(t.tagName).toBe('BUTTON');
      expect(t).toHaveAttribute('aria-expanded', 'true');
      // The icon and nothing else drawn: exactly one svg, the section glyph.
      expect(t.querySelectorAll('svg')).toHaveLength(1);
      expect(within(t).getByTestId(`rail-${s}-title`)).toBeInTheDocument();
    }
    expect(toggle('repos')).toHaveTextContent(/^Repositories$/);
    // The + is a sibling, not inside the toggle.
    expect(toggle('repos').contains(screen.getByTestId('rail-new-repo'))).toBe(false);
    expect(toggle('repos').parentElement).toBe(screen.getByTestId('rail-new-repo').parentElement);
  });

  it('clicking a header hides that section\'s rows and only those; clicking again shows them; aria-expanded follows', () => {
    renderRail();
    expect(repoNames()).toEqual(['b', 'core-kb', 'ghost', 'ops-kb']);
    fireEvent.click(toggle('repos'));
    expect(toggle('repos')).toHaveAttribute('aria-expanded', 'false');
    expect(repoNames()).toEqual([]);
    // Archived lives under Repositories, so it goes with them.
    expect(screen.queryByTestId('rail-archived')).toBeNull();
    // The other sections are untouched.
    expect(lensNames()).toEqual(['dev', 'ops-desk']);
    expect(chatTitles()).toHaveLength(5);
    expect(toggle('lenses')).toHaveAttribute('aria-expanded', 'true');
    fireEvent.click(toggle('repos'));
    expect(toggle('repos')).toHaveAttribute('aria-expanded', 'true');
    expect(repoNames()).toEqual(['b', 'core-kb', 'ghost', 'ops-kb']);
    expect(screen.getByTestId('rail-archived')).toBeInTheDocument();
  });

  it('collapses Chats, Lenses and Fleet too', () => {
    renderRail({ fleet: { fleet_repo: 'fleet-kb', master: true, local: true } as never });
    fireEvent.click(toggle('chats'));
    expect(chatTitles()).toEqual([]);
    expect(screen.queryByTestId('rail-chats-more')).toBeNull();
    fireEvent.click(toggle('lenses'));
    expect(lensNames()).toEqual([]);
    expect(screen.getByTestId('rail-fleet-row')).toBeInTheDocument();
    fireEvent.click(toggle('fleet'));
    expect(screen.queryByTestId('rail-fleet-row')).toBeNull();
    expect(repoNames()).toEqual(['b', 'core-kb', 'ghost', 'ops-kb']);
  });

  it('the title rests at #9a9a9a open and dims to #6a6a6a collapsed (through --k-rail-title, so :hover can lighten it)', () => {
    renderRail();
    expect(toggle('lenses').style.getPropertyValue('--k-rail-title')).toBe('#9a9a9a');
    // No inline colour on the title: it would outrank the hover rule.
    expect(screen.getByTestId('rail-lenses-title').style.color).toBe('');
    expect(screen.getByTestId('rail-lenses-title')).toHaveClass('k-rail-title');
    expect(toggle('lenses')).toHaveClass('k-rail-toggle');
    fireEvent.click(toggle('lenses'));
    expect(toggle('lenses').style.getPropertyValue('--k-rail-title')).toBe('#6a6a6a');
  });
});

describe('rail sections: the collapsed header\'s count', () => {
  it('shows no count while open', () => {
    renderRail();
    for (const s of ['chats', 'repos', 'lenses', 'fleet']) expect(screen.queryByTestId(`rail-${s}-count`)).toBeNull();
  });

  it('counts every row the section holds, right after the title, 11px #5a5a5a tabular', () => {
    renderRail({ fleet: { fleet_repo: 'fleet-kb', master: true, local: true } as never });
    for (const s of ['chats', 'repos', 'lenses', 'fleet']) fireEvent.click(toggle(s));
    // Seven chats, although only five rows show before "N more".
    expect(screen.getByTestId('rail-chats-count')).toHaveTextContent(/^7$/);
    // Four repositories: the unavailable one counts, the fleet repo and the archived do not.
    expect(screen.getByTestId('rail-repos-count')).toHaveTextContent(/^4$/);
    expect(screen.getByTestId('rail-lenses-count')).toHaveTextContent(/^2$/);
    expect(screen.getByTestId('rail-fleet-count')).toHaveTextContent(/^1$/);
    const count = screen.getByTestId('rail-repos-count');
    expect(count.previousElementSibling).toBe(screen.getByTestId('rail-repos-title'));
    expect(count).toHaveStyle({ fontSize: '11px', color: '#5a5a5a', fontVariantNumeric: 'tabular-nums' });
  });

  it('a section with nothing in it counts 0', () => {
    renderRail({ lenses: [] });
    fireEvent.click(toggle('lenses'));
    expect(screen.getByTestId('rail-lenses-count')).toHaveTextContent(/^0$/);
  });
});

describe('rail sections: the Chats attention dot', () => {
  const waiting = sevenChats.map(c => (c.id === 'c2' ? { ...c, state: 'awaiting_approval' as const } : c));

  it('appears after the count when a HIDDEN chat is awaiting approval, with an accessible name', () => {
    renderRail({ chats: waiting });
    fireEvent.click(toggle('chats'));
    const dot = screen.getByRole('img', { name: 'Waiting for your approval' });
    expect(dot).toBe(screen.getByTestId('rail-chats-attention'));
    expect(dot.previousElementSibling).toBe(screen.getByTestId('rail-chats-count'));
    expect(dot).toHaveStyle({ width: '6px', height: '6px', background: '#e8c27a' });
    expect(dot.style.boxShadow).toBe('0 0 0 2px rgba(232,194,122,.18)');
  });

  it('is absent when no chat is awaiting approval, even collapsed', () => {
    renderRail({ chats: sevenChats.map(c => ({ ...c, state: c.id === 'c3' ? 'running' as const : c.id === 'c4' ? 'error' as const : 'idle' as const })) });
    fireEvent.click(toggle('chats'));
    expect(screen.getByTestId('rail-chats-count')).toHaveTextContent('7');
    expect(screen.queryByTestId('rail-chats-attention')).toBeNull();
  });

  it('is absent while Chats is open: the waiting row shows itself', () => {
    renderRail({ chats: waiting });
    expect(screen.queryByTestId('rail-chats-attention')).toBeNull();
    expect(screen.queryByRole('img', { name: 'Waiting for your approval' })).toBeNull();
  });

  it('never appears on another section', () => {
    renderRail({ chats: waiting });
    for (const s of ['repos', 'lenses', 'fleet']) fireEvent.click(toggle(s));
    expect(screen.queryByTestId('rail-repos-attention')).toBeNull();
    expect(screen.queryByTestId('rail-lenses-attention')).toBeNull();
    expect(screen.queryByTestId('rail-fleet-attention')).toBeNull();
  });
});

describe('rail sections: remembered per viewer', () => {
  it('starts with every section open when nothing is stored', () => {
    renderRail();
    for (const s of ['chats', 'repos', 'lenses', 'fleet']) expect(toggle(s)).toHaveAttribute('aria-expanded', 'true');
  });

  it('a collapse survives a remount (the rail unmounts when hidden)', () => {
    const first = renderRail();
    fireEvent.click(toggle('lenses'));
    fireEvent.click(toggle('chats'));
    expect(JSON.parse(localStorage.getItem(RAIL_COLLAPSED_KEY)!)).toEqual(expect.arrayContaining(['lenses', 'chats']));
    first.unmount();
    renderRail();
    expect(toggle('lenses')).toHaveAttribute('aria-expanded', 'false');
    expect(toggle('chats')).toHaveAttribute('aria-expanded', 'false');
    expect(toggle('repos')).toHaveAttribute('aria-expanded', 'true');
    expect(lensNames()).toEqual([]);
    expect(repoNames()).toEqual(['b', 'core-kb', 'ghost', 'ops-kb']);
  });

  it('reads garbage or unknown names as nothing collapsed', () => {
    localStorage.setItem(RAIL_COLLAPSED_KEY, '{not json');
    const a = renderRail();
    expect(toggle('repos')).toHaveAttribute('aria-expanded', 'true');
    a.unmount();
    localStorage.setItem(RAIL_COLLAPSED_KEY, JSON.stringify(['bogus', 'lenses']));
    renderRail();
    expect(toggle('lenses')).toHaveAttribute('aria-expanded', 'false');
    expect(toggle('repos')).toHaveAttribute('aria-expanded', 'true');
  });

  it('the helpers swallow a throwing storage: load reads nothing collapsed, save does not throw', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => { throw new Error('denied'); });
    const set = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw new Error('denied'); });
    expect(loadRailCollapsed()).toEqual([]);
    expect(() => saveRailCollapsed(['repos'])).not.toThrow();
    // Reached the storage, which threw: the catch is what kept it quiet.
    expect(set).toHaveBeenCalledWith(RAIL_COLLAPSED_KEY, '["repos"]');
  });

  it('still renders and still toggles when storage throws on every read and write', () => {
    const get = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => { throw new Error('denied'); });
    const set = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw new Error('denied'); });
    renderRail();
    expect(get).toHaveBeenCalled();
    expect(toggle('repos')).toHaveAttribute('aria-expanded', 'true');
    fireEvent.click(toggle('repos'));
    expect(set).toHaveBeenCalled();
    expect(toggle('repos')).toHaveAttribute('aria-expanded', 'false');
    expect(repoNames()).toEqual([]);
  });
});

describe('rail sections: the search', () => {
  it('shows a collapsed section\'s matches while it has text, and clearing it collapses the section again', () => {
    renderRail();
    fireEvent.click(toggle('repos'));
    fireEvent.click(toggle('lenses'));
    expect(repoNames()).toEqual([]);
    fireEvent.change(screen.getByTestId('rail-search'), { target: { value: 'ops' } });
    expect(repoNames()).toEqual(['ops-kb']);
    expect(lensNames()).toEqual(['ops-desk']);
    expect(toggle('repos')).toHaveAttribute('aria-expanded', 'true');
    expect(screen.queryByTestId('rail-repos-count')).toBeNull();
    fireEvent.change(screen.getByTestId('rail-search'), { target: { value: '' } });
    expect(repoNames()).toEqual([]);
    expect(lensNames()).toEqual([]);
    expect(toggle('repos')).toHaveAttribute('aria-expanded', 'false');
    // The search never wrote the remembered state.
    expect(JSON.parse(localStorage.getItem(RAIL_COLLAPSED_KEY)!)).toEqual(['repos', 'lenses']);
  });
});

describe('rail sections: the search leaves unfiltered sections alone', () => {
  it('a collapsed Chats keeps its count and dot and shows no rows while searching; a collapsed Fleet stays collapsed', () => {
    const waiting = sevenChats.map(c => (c.id === 'c2' ? { ...c, state: 'awaiting_approval' as const } : c));
    renderRail({ chats: waiting, fleet: { fleet_repo: 'fleet-kb', master: true, local: true } as never });
    fireEvent.click(toggle('chats'));
    fireEvent.click(toggle('fleet'));
    fireEvent.click(toggle('repos'));
    fireEvent.change(screen.getByTestId('rail-search'), { target: { value: 'ops' } });
    // Control: the filtered section reveals its match.
    expect(repoNames()).toEqual(['ops-kb']);
    expect(toggle('chats')).toHaveAttribute('aria-expanded', 'false');
    expect(chatTitles()).toEqual([]);
    expect(screen.getByTestId('rail-chats-count')).toHaveTextContent(/^7$/);
    expect(screen.getByTestId('rail-chats-attention')).toBeInTheDocument();
    expect(toggle('fleet')).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByTestId('rail-fleet-row')).toBeNull();
    expect(screen.getByTestId('rail-fleet-count')).toHaveTextContent(/^1$/);
  });

  it('an OPEN Chats still shows its rows while searching (the search never filters chats)', () => {
    renderRail();
    fireEvent.change(screen.getByTestId('rail-search'), { target: { value: 'zzz' } });
    expect(chatTitles()).toHaveLength(5);
  });
});

describe('rail sections: a draft row appearing by any route', () => {
  const base = { repos, lenses, archived, context: { kind: 'repo' as const, repo: 'b' }, currentRepo: 'b', remoteError: '',
    serverReadOnly: false, onSelect: () => {}, chatProviders: providers, chatsAvailable: true, chats: sevenChats };

  it('opens its collapsed section and stores it open — without a + click — and only that section', () => {
    localStorage.setItem(RAIL_COLLAPSED_KEY, JSON.stringify(['repos', 'chats']));
    const { rerender } = render(<TargetRail {...base} drafts={{ repo: false, lens: false, chat: false }} />);
    expect(toggle('chats')).toHaveAttribute('aria-expanded', 'false');
    rerender(<TargetRail {...base} drafts={{ repo: false, lens: false, chat: true }} draftShown="chat" />);
    expect(screen.getByTestId('rail-draft-chat')).toBeInTheDocument();
    expect(toggle('chats')).toHaveAttribute('aria-expanded', 'true');
    expect(toggle('repos')).toHaveAttribute('aria-expanded', 'false');
    expect(JSON.parse(localStorage.getItem(RAIL_COLLAPSED_KEY)!)).toEqual(['repos']);
  });

  it('a draft already present on mount opens its collapsed section', () => {
    localStorage.setItem(RAIL_COLLAPSED_KEY, JSON.stringify(['repos']));
    render(<TargetRail {...base} drafts={{ repo: true, lens: false }} draftShown="repo" />);
    expect(screen.getByTestId('rail-draft-repo')).toBeInTheDocument();
    expect(toggle('repos')).toHaveAttribute('aria-expanded', 'true');
    expect(JSON.parse(localStorage.getItem(RAIL_COLLAPSED_KEY)!)).toEqual([]);
  });

  it('only on appearing: the reader can collapse the section again while the draft stays', () => {
    const { rerender } = render(<TargetRail {...base} drafts={{ repo: false, lens: true }} />);
    fireEvent.click(toggle('lenses'));
    rerender(<TargetRail {...base} drafts={{ repo: false, lens: true }} draftShown="lens" />);
    expect(toggle('lenses')).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByTestId('rail-draft-lens')).toBeNull();
  });
});

describe('rail sections: the + on a collapsed section', () => {
  it('opens the section and asks for the draft, once', () => {
    const onNewDraft = vi.fn();
    renderRail({ onNewDraft });
    fireEvent.click(toggle('repos'));
    fireEvent.click(toggle('chats'));
    fireEvent.click(screen.getByTestId('rail-new-repo'));
    expect(onNewDraft).toHaveBeenCalledTimes(1);
    expect(onNewDraft).toHaveBeenCalledWith('repo');
    expect(toggle('repos')).toHaveAttribute('aria-expanded', 'true');
    // Only that section: Chats stays collapsed.
    expect(toggle('chats')).toHaveAttribute('aria-expanded', 'false');
    expect(JSON.parse(localStorage.getItem(RAIL_COLLAPSED_KEY)!)).toEqual(['chats']);
    fireEvent.click(screen.getByTestId('rail-new-chat'));
    expect(onNewDraft).toHaveBeenLastCalledWith('chat');
    expect(toggle('chats')).toHaveAttribute('aria-expanded', 'true');
  });

  it('the draft row is visible once the section opens', () => {
    renderRail({ drafts: { repo: false, lens: true }, draftShown: 'lens' });
    fireEvent.click(toggle('lenses'));
    expect(screen.queryByTestId('rail-draft-lens')).toBeNull();
    fireEvent.click(screen.getByTestId('rail-new-lens'));
    expect(screen.getByTestId('rail-draft-lens')).toBeInTheDocument();
  });

  it('on an open section the + changes nothing but the draft', () => {
    const onNewDraft = vi.fn();
    renderRail({ onNewDraft });
    fireEvent.click(screen.getByTestId('rail-new-lens'));
    expect(onNewDraft).toHaveBeenCalledWith('lens');
    expect(toggle('lenses')).toHaveAttribute('aria-expanded', 'true');
    expect(localStorage.getItem(RAIL_COLLAPSED_KEY)).toBeNull();
  });
});

describe('rail sections: Archived', () => {
  it('has no chevron, and shows its count closed AND open', () => {
    renderRail();
    const group = screen.getByTestId('rail-archived');
    // The archive glyph is its only drawing.
    expect(group.querySelectorAll('svg')).toHaveLength(1);
    expect(group).toHaveAttribute('aria-expanded', 'false');
    expect(within(group).getByTestId('rail-archived-count')).toHaveTextContent(/^3$/);
    fireEvent.click(group);
    expect(group).toHaveAttribute('aria-expanded', 'true');
    expect(group.querySelectorAll('svg')).toHaveLength(1);
    expect(within(group).getByTestId('rail-archived-count')).toHaveTextContent(/^3$/);
    expect(screen.queryAllByTestId(/^rail-archived-name-/).map(e => e.textContent)).toEqual(['demo-kb', 'old-notes', 'scratch']);
    fireEvent.click(group);
    expect(screen.queryAllByTestId(/^rail-archived-name-/)).toHaveLength(0);
  });
});

describe('rail sections: "N more" inside an open Chats', () => {
  it('still caps at five and expands, including after a collapse and reopen', () => {
    renderRail();
    expect(chatTitles()).toEqual(['title c7', 'title c6', 'title c5', 'title c4', 'title c3']);
    expect(screen.getByTestId('rail-chats-more')).toHaveTextContent('2 more');
    fireEvent.click(toggle('chats'));
    fireEvent.click(toggle('chats'));
    expect(chatTitles()).toHaveLength(5);
    fireEvent.click(screen.getByTestId('rail-chats-more'));
    expect(chatTitles()).toHaveLength(7);
    expect(screen.queryByTestId('rail-chats-more')).toBeNull();
  });
});
