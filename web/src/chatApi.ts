// The chat client: the routes under /api/v1/chat-providers and /api/v1/chats
// (internal/edge/web/api/chat). Every JSON call goes through api.ts's
// fetchJSON and every event stream through openEventStream, which is what
// makes the desktop's Wails transport work for chat too.
import { fetchJSON, apiUrl } from './api';
import { openEventStream } from './eventStream';
import type { EventSourceLike } from './eventStream';
import { CHAT_SEND_TIMEOUT_DRAFT } from './chatCopy';

/** One live chat, as GET /chats lists it. */
export interface ChatInfo { id: string; provider: string; model: string }

/** A model the picker offers: `available` is the provider's own answer, `tools`
 *  whether it can call tools, `reason` says why a model is unusable. */
export interface ChatModelOption { id: string; available: boolean; tools: boolean; reason?: string }
export interface ChatProvider { name: string; kind: string; models: ChatModelOption[] }

/** A row of GET /chats/{id}'s transcript. Tool results are not in it: they are
 *  fetched lazily from the call (getChatCall). */
export interface ChatViewItem {
  kind: 'user' | 'assistant' | 'tool_call';
  text?: string; id?: string; name?: string; arguments?: unknown;
  state?: 'running' | 'done'; is_error?: boolean;
  /** On a user item: how the turn it opens went (see chat.go viewItem). */
  usage?: { input: number; cache_read: number; cache_write: number; output: number };
  cost_usd?: number; warning?: string; error?: string; cancelled?: boolean;
}

/** A tool call waiting for the person. `target` is empty when the server could
 *  not resolve the call's repo or lens. */
export interface ChatApproval {
  call_id: string; tool: string; action: string;
  target?: string; target_kind?: string;
  destructive: boolean; grantable: boolean; arguments?: unknown;
}

export interface ChatGrant { target: string; kind: string; tool: string; action: string }

export interface ChatSnapshot extends ChatInfo {
  running: boolean; usage_seq?: number; items: ChatViewItem[]; pending_approvals: ChatApproval[]; grants: ChatGrant[];
}

/** GET /chats/{id}/calls/{callId}. */
export interface ChatCall { name: string; arguments: unknown; result: string; is_error: boolean; elided: boolean }

export type ApprovalDecision = 'allow' | 'decline' | 'allow_session';

const chatsUrl = (): string => apiUrl('/api/v1/chats');
const chatUrl = (id: string): string => `${chatsUrl()}/${encodeURIComponent(id)}`;
const JSON_HEADERS = { 'Content-Type': 'application/json' };

export async function listChatProviders(): Promise<ChatProvider[]> {
  const data = await fetchJSON<{ _embedded?: { providers?: ChatProvider[] } }>(apiUrl('/api/v1/chat-providers'));
  return data._embedded?.providers ?? [];
}

export async function listChats(): Promise<ChatInfo[]> {
  const data = await fetchJSON<{ _embedded?: { chats?: ChatInfo[] } }>(chatsUrl());
  return (data._embedded?.chats ?? []).map(({ id, provider, model }) => ({ id, provider, model }));
}

export async function createChat(provider: string, model: string): Promise<ChatInfo> {
  const c = await fetchJSON<ChatInfo>(chatsUrl(), { method: 'POST', headers: JSON_HEADERS, body: JSON.stringify({ provider, model }) });
  return { id: c.id, provider: c.provider, model: c.model };
}

export function getChat(id: string): Promise<ChatSnapshot> {
  return fetchJSON<ChatSnapshot>(chatUrl(id));
}

/** How long a send may go unanswered before it counts as failed. The server
 *  answers POST /turns as soon as it has appended the turn to its view (the 202
 *  does not wait for the model), so a healthy answer takes a round trip. 15 s
 *  is far past that, long enough for a slow link or the desktop proxy's first
 *  request (it answers 503 + Retry-After while booting, which is an answer),
 *  and short enough that a request that is hung does not hold the chat for
 *  minutes. If it did reach the server, the turn shows up in the next snapshot. */
export const SEND_TIMEOUT_MS = 15_000;

export async function sendChatTurn(id: string, text: string, timeoutMs = SEND_TIMEOUT_MS): Promise<unknown> {
  const url = `${chatUrl(id)}/turns`;
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), timeoutMs);
  try {
    return await fetchJSON(url, { method: 'POST', headers: JSON_HEADERS, body: JSON.stringify({ text }), signal: ctl.signal });
  } catch (e) {
    if (ctl.signal.aborted) throw new Error(`${url} → ${CHAT_SEND_TIMEOUT_DRAFT}`);
    throw e;
  } finally {
    clearTimeout(timer);
  }
}

// The 204 routes have no body for fetchJSON to parse.
async function noContent(url: string, init: RequestInit): Promise<void> {
  const r = await fetch(url, init);
  if (!r.ok) {
    let detail = r.statusText;
    try { const b = await r.json() as { detail?: string; title?: string }; detail = b.detail || b.title || detail; } catch { /* keep statusText */ }
    // The status rides on the Error, as fetchJSON's does (api.ts HttpError).
    throw Object.assign(new Error(`${url} → ${r.status} ${detail}`), { status: r.status });
  }
}

export function cancelChatTurn(id: string): Promise<void> {
  return noContent(`${chatUrl(id)}/cancel`, { method: 'POST' });
}

export function answerChatApproval(id: string, callId: string, decision: ApprovalDecision): Promise<void> {
  return noContent(`${chatUrl(id)}/approvals/${encodeURIComponent(callId)}`, {
    method: 'POST', headers: JSON_HEADERS, body: JSON.stringify({ decision }),
  });
}

export function deleteChat(id: string): Promise<void> {
  return noContent(chatUrl(id), { method: 'DELETE' });
}

/** The page is going away: end the chat. `keepalive` lets the request outlive
 *  the page. Fire and forget; the server's stream grace is the backstop. */
export function endChatOnPageHide(id: string): void {
  try { void fetch(chatUrl(id), { method: 'DELETE', keepalive: true }).catch(() => {}); } catch { /* going away */ }
}

export async function listChatGrants(id: string): Promise<ChatGrant[]> {
  const data = await fetchJSON<{ _embedded?: { grants?: ChatGrant[] } }>(`${chatUrl(id)}/grants`);
  return data._embedded?.grants ?? [];
}

/** Take back one session grant, named as listChatGrants returned it. */
export function revokeChatGrant(id: string, grant: ChatGrant): Promise<void> {
  return noContent(`${chatUrl(id)}/grants`, { method: 'DELETE', headers: JSON_HEADERS, body: JSON.stringify(grant) });
}

export function getChatCall(id: string, callId: string): Promise<ChatCall> {
  return fetchJSON<ChatCall>(`${chatUrl(id)}/calls/${encodeURIComponent(callId)}`);
}

/** The server's SSE event names (internal/edge/web/api/chat/chat.go). */
export const CHAT_EVENT_NAMES = [
  'ready', 'text_delta', 'reasoning', 'tool_call', 'approval', 'usage', 'warning', 'turn_done', 'turn_error', 'chat_end',
] as const;
export type ChatEventName = typeof CHAT_EVENT_NAMES[number];

export function openChatEvents(id: string): EventSourceLike {
  return openEventStream(`${chatUrl(id)}/events`);
}
