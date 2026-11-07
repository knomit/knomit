// A tool call is worded by the tool's own metadata, sent by the server: its
// title (annotations.title) and the value of the argument the tool marks as its
// main one. The page keeps no wording per tool, so changing a tool never needs
// a page change. Failure is the red dot and red text, never a verb.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup, act } from '@testing-library/react';
import { useEffect, useReducer } from 'react';
import { ChatToolGroup } from './ChatToolGroup';
import { ChatSidePanel } from './ChatSidePanel';
import { initialPanelState, panelReducer } from './chatPanelState';
import type { PanelEntry } from './chatPanelState';
import { ChatFacet } from './ChatFacet';
import { installFakeEventSource, uninstallFakeEventSource, chatStream } from './testEventSource';
import { stubFleetFetch } from './fleetTestFetch';
import { activitySummary } from './chatCopy';
import { chatReducer, initialChatState, snapshotState } from './chatState';
import type { ChatEvent, ToolCall } from './chatState';
import type { ChatSnapshot } from './chatApi';

const BASE = '/api/v1/chats/chat-a';

beforeEach(() => { installFakeEventSource(); localStorage.clear(); });
afterEach(() => { cleanup(); uninstallFakeEventSource(); vi.unstubAllGlobals(); });

/** What a row (or the panel header) says, part by part: title · main argument. */
const words = (el: HTMLElement): string =>
  [...el.querySelectorAll('[data-call-part="label"], [data-call-part="sep"], [data-call-part="arg"]')]
    .map(e => e.textContent).join(' ');

let open: (e: PanelEntry) => void;
function Panel({ callFor }: { callFor: (id: string) => ToolCall | undefined }) {
  const [panel, dispatch] = useReducer(panelReducer, initialPanelState);
  useEffect(() => { open = entry => dispatch({ type: 'open', entry }); });
  return <ChatSidePanel chatId="chat-a" panel={panel} dispatch={dispatch} running={false} width={480} onSplitterDown={vi.fn()} boundFor={() => 'r1'} callFor={callFor} />;
}

/** The transcript row and the side panel's header for the same call. */
async function rowAndHead(c: ToolCall) {
  stubFleetFetch({ [`GET ${BASE}/calls/${c.id}`]: { body: { name: c.name, arguments: c.args ?? {}, result: 'r', is_error: c.isError, elided: false } } });
  render(<>
    <ChatToolGroup lead={[]} calls={[c]} approvals={{}} selectedId={null} onSelect={vi.fn()} onAnswer={vi.fn()} />
    <Panel callFor={id => (id === c.id ? c : undefined)} />
  </>);
  fireEvent.click(screen.getByTestId('chat-tool-group-toggle'));
  act(() => open({ kind: 'result', callId: c.id }));
  await screen.findByTestId('chat-result-tab-result');
  return { row: screen.getByTestId(`chat-tool-row-${c.id}`), head: screen.getByTestId('chat-result-title') };
}

describe('a tool call row and its panel header take their words from the tool', () => {
  it('a titled tool with a main argument: "<title> · <main argument>"', async () => {
    const { row, head } = await rowAndHead({ id: 'b', name: 'knomit_bind', title: 'Bind', keyArg: 'demo-kb', state: 'done', isError: false, args: { repo: 'demo-kb' } });
    expect(words(row)).toBe('Bind · demo-kb');
    expect(words(head)).toBe('Bind · demo-kb');
  });

  it('a titled tool with no main argument is just its title', async () => {
    const { row, head } = await rowAndHead({ id: 'r', name: 'knomit_repos', title: 'List repos', state: 'done', isError: false, args: {} });
    expect(words(row)).toBe('List repos');
    expect(words(head)).toBe('List repos');
  });

  it('a tool with no title is shown by its raw name', async () => {
    const { row, head } = await rowAndHead({ id: 'x', name: 'acme_frobnicate', state: 'done', isError: false, args: { q: 1 } });
    expect(words(row)).toBe('acme_frobnicate');
    expect(words(head)).toBe('acme_frobnicate');
  });

  it('a failed call is red, with the same words: no verb says it failed', async () => {
    const ok = await rowAndHead({ id: 'q', name: 'knomit_query', title: 'Query', keyArg: 'approval gate', state: 'done', isError: false });
    const okColor = getComputedStyle(ok.row.querySelector('[data-call-part="line"]')!).color;
    cleanup();
    const { row, head } = await rowAndHead({ id: 'q', name: 'knomit_query', title: 'Query', keyArg: 'approval gate', state: 'done', isError: true });
    for (const el of [row, head]) {
      expect(el).toHaveAttribute('data-error', 'true');
      expect(words(el)).toBe('Query · approval gate');
      expect(getComputedStyle(el.querySelector('[data-call-part="line"]')!).color).not.toBe(okColor);
    }
  });

  it('the loader: its title and what it loaded, as the server words it', async () => {
    const { row, head } = await rowAndHead({ id: 'l', name: 'load_tools', title: 'Load tools', keyArg: 'knomit_query, knomit_explain', state: 'done', isError: false, loaded: ['knomit_query', 'knomit_explain'] });
    expect(words(row)).toBe('Load tools · knomit_query, knomit_explain');
    expect(words(head)).toBe('Load tools · knomit_query, knomit_explain');
  });

  it('a running call reads the same as a finished one (no "Searching")', () => {
    render(<ChatToolGroup lead={[]} approvals={{}} selectedId={null} onSelect={vi.fn()} onAnswer={vi.fn()} running
      calls={[{ id: 'q', name: 'knomit_query', title: 'Query', keyArg: 'approval gate', state: 'running', isError: false, args: { text: 'approval gate' } }]} />);
    expect(screen.getByTestId('chat-activity-summary').textContent).toBe('Query');
    fireEvent.click(screen.getByTestId('chat-tool-group-toggle'));
    expect(words(screen.getByTestId('chat-tool-row-q'))).toBe('Query · approval gate');
  });
});

describe('the title and main argument arrive on the events and the snapshot', () => {
  it('the running and done tool_call events carry them onto the call', () => {
    const run = (evs: ChatEvent[]) => evs.reduce(chatReducer, initialChatState);
    const first = (s: ReturnType<typeof run>) => (s.turns[0].parts.find(p => p.kind === 'group') as { calls: ToolCall[] }).calls[0];
    const start: ChatEvent[] = [{ kind: 'user', text: 'q', sendId: 1 }, { kind: 'send_accepted', sendId: 1, turn: 0, at: 1 },
      { kind: 'tool_call', id: 'q', name: 'knomit_query', state: 'running', title: 'Query', key_arg: 'approval gate', turn: 0 }];
    expect(first(run(start))).toMatchObject({ title: 'Query', keyArg: 'approval gate', state: 'running' });
    // A done event that has them again keeps them (the loader's main argument is set only then).
    const done = run([...start, { kind: 'tool_call', id: 'q', name: 'knomit_query', state: 'done', title: 'Query', key_arg: 'other', turn: 0 }]);
    expect(first(done)).toMatchObject({ title: 'Query', keyArg: 'other', state: 'done' });
  });

  it('a snapshot row carries them onto the call', () => {
    const snap: ChatSnapshot = {
      id: 'chat-a', provider: 'p', model: 'm', running: false, grants: [], pending_approvals: [],
      items: [{ kind: 'user', text: 'q' }, { kind: 'tool_call', id: 'b', name: 'knomit_bind', state: 'done', title: 'Bind', key_arg: 'demo-kb' }],
    };
    const s = snapshotState(snap);
    const g = s.turns[0].parts.find(p => p.kind === 'group') as { calls: ToolCall[] };
    expect(g.calls[0]).toMatchObject({ title: 'Bind', keyArg: 'demo-kb' });
  });
});

describe('the activity line: titles, counts and targets', () => {
  const line = (calls: ToolCall[], bound = '') => {
    render(<ChatToolGroup lead={[]} calls={calls} approvals={{}} selectedId={null} bound={bound} onSelect={vi.fn()} onAnswer={vi.fn()} />);
    return screen.queryByTestId('chat-activity-summary')?.textContent ?? '';
  };

  it('one bind and two queries: "Bind demo-kb · Query 2× in knomit"', () => {
    const text = (p: ReturnType<typeof activitySummary>) => p.map(s => s.map(x => x.text).join('')).join(' · ');
    const pieces = activitySummary([
      { title: 'Bind', target: 'demo-kb', bind: true },
      { title: 'Query', target: 'knomit' },
      { title: 'Query', target: 'knomit' },
    ]);
    expect(text(pieces)).toBe('Bind demo-kb · Query 2× in knomit');
    // The target a bind names and the count are drawn strong.
    expect(pieces[0].filter(s => s.strong).map(s => s.text)).toEqual(['demo-kb']);
    expect(pieces[1].filter(s => s.strong).map(s => s.text)).toEqual(['2×']);
    // Drawn from calls: each query runs where the bind left the chat.
    expect(line([
      { id: 'b', name: 'knomit_bind', title: 'Bind', keyArg: 'demo-kb', state: 'done', isError: false, args: { lens: 'demo-kb' } },
      { id: 'q1', name: 'knomit_query', title: 'Query', keyArg: 'a', state: 'done', isError: false, args: { binding: 'h', text: 'a' } },
      { id: 'q2', name: 'knomit_query', title: 'Query', keyArg: 'b', state: 'done', isError: false, args: { binding: 'h', text: 'b' } },
    ])).toBe('Bind demo-kb · Query 2× in demo-kb');
  });

  it('a stream event\'s title reaches the row through ChatFacet', async () => {
    stubFleetFetch({ [`GET ${BASE}`]: { body: { id: 'chat-a', provider: 'p', model: 'm', running: true, grants: [], pending_approvals: [], items: [{ kind: 'user', text: 'q' }] } } });
    render(<ChatFacet chat={{ id: 'chat-a', provider: 'p', model: 'm' }} facet="chat" onGone={vi.fn()} />);
    await screen.findByTestId('chat-turn-0');
    act(() => { chatStream('chat-a')!.emit('tool_call', { id: 'q', name: 'knomit_query', state: 'running', title: 'Query', key_arg: 'approval gate', turn: 0 }); });
    fireEvent.click(await screen.findByTestId('chat-tool-group-toggle'));
    expect(words(screen.getByTestId('chat-tool-row-q'))).toBe('Query · approval gate');
  });

  it('a failed bind adds nothing; the query after it ran where the chat was bound', () => {
    expect(line([
      { id: 'b', name: 'knomit_bind', title: 'Bind', keyArg: 'nosuch', state: 'done', isError: true, args: { repo: 'nosuch' } },
      { id: 'q', name: 'knomit_query', title: 'Query', keyArg: 'x', state: 'done', isError: false, args: { binding: 'h', text: 'x' } },
    ], 'r1')).toBe('Query in r1');
  });

  it('counts per title and target in first-appearance order; untitled tools by their name; the loader names no target', () => {
    expect(line([
      { id: 'l', name: 'load_tools', title: 'Load tools', state: 'done', isError: false, args: { names: ['knomit_explain'] } },
      { id: 'b', name: 'knomit_bind', title: 'Bind', state: 'done', isError: false, args: { repo: 'alpha' } },
      { id: 'e1', name: 'knomit_explain', title: 'Explain', state: 'done', isError: false, args: { binding: 'h', file: 'x' } },
      { id: 'x', name: 'acme_frobnicate', state: 'done', isError: false, args: {} },
      { id: 'e2', name: 'knomit_explain', title: 'Explain', state: 'done', isError: false, args: { binding: 'h', file: 'y' } },
    ])).toBe('Load tools · Bind alpha · Explain 2× in alpha · acme_frobnicate');
  });
});

// The page has no table of tool wording. The only tool names it spells are
// where it BEHAVES differently for a tool: bind tracking (which repo a chat is
// bound to) and the materialize renderer.
describe('web/src words no tool itself', () => {
  const sources = import.meta.glob(['./**/*.ts', './**/*.tsx', '!./**/*.test.ts', '!./**/*.test.tsx'], { query: '?raw', import: 'default', eager: true }) as Record<string, string>;

  it('has no per-tool wording functions or verbs', () => {
    for (const [file, src] of Object.entries(sources)) {
      expect(src, file).not.toMatch(/\btoolRowLabel\b|\btoolVerb\b|\btoolKind\b|\bliveStep\b|\bloadedToolsRow\b|knomit: Knomit|'Bound|'Searched|'Searching|Loaded tools/);
    }
  });

  it('spells a tool name only where it changes behaviour', () => {
    const found: string[] = [];
    for (const [file, src] of Object.entries(sources)) {
      for (const m of src.matchAll(/['"]knomit_[a-z_]+['"]/g)) found.push(`${file.replace('./', '')} ${m[0]}`);
    }
    expect(found.sort()).toEqual([
      "ChatResultPanel.tsx 'knomit_materialize'",
      "ChatSidePanel.tsx 'knomit_materialize'",
      "chatState.ts 'knomit_bind'",
    ]);
  });
});
