// The chat page's visible copy, in one place so it can be reviewed.
//
// VERBATIM: wording the plan and the design give. Do not change it on your own
// ruling.
// APPROVED: wording the plan and design did not give, approved by the user on
// 2026-11-05. Do not change it on your own ruling either.
// DRAFT: wording not yet approved; its constant carries a _DRAFT suffix and the
// report that adds it flags it.
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

/** The result panel, while a tool result is being fetched. */
export const CHAT_RESULT_LOADING = 'Loading…';

// ── DRAFT COPY: needs user approval ──

/** The button on a Settings grant row that takes the grant back. */
export const CHAT_REVOKE_GRANT_DRAFT = 'Revoke';
