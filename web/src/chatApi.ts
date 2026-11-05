// The chat client: the routes under /api/v1/chat-providers and /api/v1/chats
// (internal/edge/web/api/chat). Every JSON call goes through api.ts's
// fetchJSON and every event stream through openEventStream, which is what
// makes the desktop's Wails transport work for chat too.
import { fetchJSON, apiUrl } from './api';
import { openEventStream } from './eventStream';
import type { EventSourceLike } from './eventStream';

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
  running: boolean; items: ChatViewItem[]; pending_approvals: ChatApproval[]; grants: ChatGrant[];
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

export function sendChatTurn(id: string, text: string): Promise<unknown> {
  return fetchJSON(`${chatUrl(id)}/turns`, { method: 'POST', headers: JSON_HEADERS, body: JSON.stringify({ text }) });
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
