// The page's ONE chat events stream. A browser holds few HTTP/1.1 connections
// per origin (six, and plain http://localhost is HTTP/1.1), so a stream per
// chat starved the page at five chats and hung every fetch at nine. The server
// carries the events of all the caller's chats on GET /chats/events, each
// tagged with its chat id; this module opens that stream once, when the first
// chat subscribes, and routes each event to the subscribers of its chat.
//
// Reconnect has two paths. First the stream's own: EventSource retries a lost
// connection, and the desktop's WailsEventSource retries both of its legs. But
// a stream can end CLOSED for good: EventSource on a non-200 answer or a wrong
// content type, WailsEventSource on the desktop's `error` frame with `closed`
// (the server refused). Then the hub opens a new stream itself, after
// REOPEN_INITIAL_MS doubling to REOPEN_MAX_MS, reset by a `ready`, for as long
// as anyone listens. Every connect starts with a page-level `ready`, which is
// delivered to EVERY subscriber, and each one reloads the snapshot it shows.
// The stream closes, and a pending reopen is dropped, when the last subscriber
// leaves.
import { openEventStream } from './eventStream';
import type { EventSourceLike } from './eventStream';
import { CHAT_EVENT_NAMES, chatEventsUrl } from './chatApi';

export type ChatEventListener = (name: string, data: Record<string, unknown>) => void;

/** Back-off of the hub's own reopen of a CLOSED stream. */
export const REOPEN_INITIAL_MS = 1000;
export const REOPEN_MAX_MS = 30_000;
const CLOSED = 2;

interface Hub {
  es: EventSourceLike;
  /** The delay of the next reopen, and the pending one if any. */
  reopenMs: number;
  reopen: ReturnType<typeof setTimeout> | null;
  byChat: Map<string, Set<ChatEventListener>>;
  /** The stream has said `ready` at least once: a chat that subscribes now has
   *  missed it and is told, so its first read does not wait for a reconnect. */
  ready: boolean;
  /** Page-level watchers: they see `ready` and EVERY chat's `chat_state`, listed
   *  or not, and hold the stream open while they watch. */
  watchers: Set<ChatEventListener>;
  /** The latest chat_state per chat id, kept whether or not anyone subscribes to
   *  that chat: a chat created in this page has its first events (the turn its
   *  create started) before the page knows its id. Bounded by the owner's own
   *  chats (the stream carries no one else's); dropped on `chat_end`, cleared on
   *  every connect (`ready`), and gone with the stream. */
  rows: Map<string, Record<string, unknown>>;
}

let hub: Hub | null = null;

/** The latest chat_state the page stream carried for this chat since it connected. */
export function latestChatState(chatId: string): Record<string, unknown> | undefined {
  return hub?.rows.get(chatId);
}

const idle = (h: Hub) => h.byChat.size === 0 && h.watchers.size === 0;

/** How many streams the page has open for chat events: 0 or 1. */
export function chatEventStreamCount(): number { return hub ? 1 : 0; }

const route = (h: Hub, name: string, e: MessageEvent) => {
  let data: Record<string, unknown> = {};
  try { data = e.data ? JSON.parse(e.data) as Record<string, unknown> : {}; } catch { return; }
  if (name === 'ready') {
    h.ready = true;
    h.reopenMs = REOPEN_INITIAL_MS;
    h.rows.clear();
    // Page-level: nothing says which chats missed what, so all reload.
    for (const set of [...h.byChat.values()]) for (const fn of [...set]) fn(name, data);
    for (const fn of [...h.watchers]) fn(name, data);
    return;
  }
  const chat = typeof data.chat === 'string' ? data.chat : '';
  if (name === 'chat_state' && chat) {
    h.rows.set(chat, data);
    for (const fn of [...h.watchers]) fn(name, data);
  } else if (name === 'chat_end') {
    h.rows.delete(chat);
  }
  const set = h.byChat.get(chat);
  if (set) for (const fn of [...set]) fn(name, data);
};

function attach(h: Hub): void {
  const es = h.es;
  for (const name of CHAT_EVENT_NAMES) es.addEventListener(name, (e: MessageEvent) => { if (h.es === es) route(h, name, e); });
  // A CONNECTING error is the transport retrying; only CLOSED is ours to fix.
  es.addEventListener('error', () => { if (h.es === es && es.readyState === CLOSED) scheduleReopen(h); });
}

function scheduleReopen(h: Hub): void {
  if (hub !== h || h.reopen) return;
  const delay = h.reopenMs;
  h.reopenMs = Math.min(h.reopenMs * 2, REOPEN_MAX_MS);
  h.reopen = setTimeout(() => {
    h.reopen = null;
    if (hub !== h || idle(h)) return;
    h.es.close();
    h.es = openEventStream(chatEventsUrl());
    attach(h);
  }, delay);
}

function open(): Hub {
  const h: Hub = {
    es: openEventStream(chatEventsUrl()), reopenMs: REOPEN_INITIAL_MS, reopen: null,
    byChat: new Map(), ready: false, watchers: new Set(), rows: new Map(),
  };
  attach(h);
  return h;
}

/** Close the hub's stream and drop its pending reopen, if it went idle. */
function closeIfIdle(h: Hub): void {
  if (hub !== h || !idle(h)) return;
  hub = null;
  if (h.reopen) { clearTimeout(h.reopen); h.reopen = null; }
  h.es.close();
}

/**
 * Subscribe to one chat's events (and to the page-level `ready`). Opens the
 * page stream if it is not open. Returns the unsubscribe.
 */
export function subscribeChatEvents(chatId: string, fn: ChatEventListener): () => void {
  if (!hub) hub = open();
  const h = hub;
  let set = h.byChat.get(chatId);
  if (!set) { set = new Set(); h.byChat.set(chatId, set); }
  set.add(fn);
  if (h.ready) {
    queueMicrotask(() => { if (h.byChat.get(chatId)?.has(fn)) fn('ready', {}); });
  }
  return () => {
    const s = h.byChat.get(chatId);
    if (!s) return;
    s.delete(fn);
    if (s.size === 0) h.byChat.delete(chatId);
    // Deferred one microtask so a remount in the same commit (StrictMode's
    // effect replay, a keyed re-render) keeps the connection.
    queueMicrotask(() => closeIfIdle(h));
  };
}

/**
 * Watch the page-level events (`ready`, and every chat's `chat_state`) and keep
 * the stream open while watching, even with no chat subscribed: the rail needs
 * the first events of a chat it does not know yet. Returns the unwatch.
 */
export function watchChatStates(fn: ChatEventListener): () => void {
  if (!hub) hub = open();
  const h = hub;
  h.watchers.add(fn);
  if (h.ready) queueMicrotask(() => { if (h.watchers.has(fn)) fn('ready', {}); });
  return () => {
    h.watchers.delete(fn);
    queueMicrotask(() => closeIfIdle(h));
  };
}
