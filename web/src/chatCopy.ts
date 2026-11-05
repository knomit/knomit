// The chat page's visible copy, in one place so it can be reviewed.
//
// VERBATIM: wording the plan and the design give. Do not change it on your own
// ruling.
// DRAFT COPY: wording the plan and design do not give. Each is a named constant
// below, flagged "DRAFT COPY: needs user approval" in the PR 6 worker report.
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

// ── DRAFT COPY: needs user approval ──

/** N14. The approval card's target when the server could not resolve the
 *  call's repo or lens handle (the card would otherwise show an empty target). */
export const APPROVAL_TARGET_UNRESOLVED_DRAFT = 'target not resolved';

/** The start form, when no provider offers a usable model. */
export const CHAT_NO_MODELS_DRAFT = 'No models are available to chat with.';

/** The message box and its two buttons. */
export const CHAT_INPUT_PLACEHOLDER_DRAFT = 'Message';
export const CHAT_SEND_DRAFT = 'Send';
export const CHAT_STOP_DRAFT = 'Stop';

/** The chat's Settings body when nothing has been authorized for the session. */
export const CHAT_NO_GRANTS_DRAFT = 'Nothing has been allowed for this session.';

/** The result panel, while a tool result is being fetched. */
export const CHAT_RESULT_LOADING_DRAFT = 'Loading…';
