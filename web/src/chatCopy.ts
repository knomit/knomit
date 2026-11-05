// The chat page's visible copy, in one place so it can be reviewed.
//
// VERBATIM: wording the plan and the design give. Do not change it on your own
// ruling.
// APPROVED: wording the plan and design did not give, approved by the user on
// 2026-11-05. Do not change it on your own ruling either.
// New wording that is not yet approved is flagged in the report that adds it
// and needs the user's approval before it ships. (None at present.)
// The server's own sentences (the price-tier warning, the reasoning-truncated
// message, a refusal) are NOT here: the page shows them as the server sends
// them.

// ── VERBATIM (design "Chat" > Rendering, and Approvals) ──

/** The header of a group of consecutive tool calls. */
export const TOOL_GROUP_HEADER = 'Used Knomit';
export const APPROVAL_ALLOW = 'Allow';
export const APPROVAL_DECLINE = 'Decline';
export const APPROVAL_ALLOW_SESSION = 'Allow for session';
/** The two tabs of a chat target (the user's answer to Q1) live in
 *  TopBar's FACET_LABEL: 'Chat' and 'Settings'. */

// ── APPROVED (2026-11-05) ──

/** N14. The approval card's target when the server could not resolve the
 *  call's repo or lens handle (the card would otherwise show an empty target). */
export const APPROVAL_TARGET_UNRESOLVED = 'target not resolved';

/** The start form, when no provider offers a usable model. */
export const CHAT_NO_MODELS = 'No models are available to chat with.';

/** The message box and its two buttons. */
export const CHAT_INPUT_PLACEHOLDER = 'Message';
export const CHAT_SEND = 'Send';
export const CHAT_STOP = 'Stop';

/** The chat's Settings body when nothing has been authorized for the session. */
export const CHAT_NO_GRANTS = 'Nothing has been allowed for this session.';

/** The chat Settings line for the model's context window. `how` is what the
 *  server reports: "config", "memory", or "trained" (derived, and the model's
 *  own trained length was the smaller). */
export const CHAT_CONTEXT_HOW: Record<string, string> = {
  config: 'set in config',
  memory: "limited to fit this machine's memory",
  trained: "the model's trained length",
};
export function chatContextLine(window: number, how: string): string {
  const h = CHAT_CONTEXT_HOW[how];
  return h ? `Context window ${window} tokens, ${h}` : `Context window ${window} tokens`;
}

/** Shown when Ollama reports part of the model running on the CPU. */
export function chatSpillWarning(window: number): string {
  return `This model is partly running on CPU at a context of ${window} tokens. Set num_ctx for this provider in knomit.toml to a smaller value.`;
}

/** The result panel, while a tool result is being fetched. */
export const CHAT_RESULT_LOADING = 'Loading…';

/** The button on a Settings grant row that takes the grant back. */
export const CHAT_REVOKE_GRANT = 'Revoke';

/** The detail of the error shown when a send gets no answer in time
 *  (`<url> → <this>`, the same shape as every other request error). */
export const CHAT_SEND_TIMEOUT = 'no answer';

/** C8. What an adaptive chat shows under a turn's usage: how many of the tools
 *  the model can call are loaded in the request, and about how many tokens
 *  their definitions take. Approved by the user 2026-11-05. */
export function toolsInContextLine(loadedNow: number, total: number, tokens: number): string {
  const t = tokens >= 1000 ? `${(tokens / 1000).toFixed(1)}K` : String(tokens);
  return `Tools in context: ${loadedNow} of ${total} (about ${t} tokens)`;
}

/** C8. The row of a call to the loader tool in the "Used Knomit" group.
 *  Approved by the user 2026-11-05. */
export function loadedToolsRow(names: string[]): string {
  return `Loaded tools: ${names.join(', ')}`;
}
