import { useEffect, useState } from 'react';
import type { CSSProperties } from 'react';
import { getChatContext, listChatGrants, revokeChatGrant } from './chatApi';
import type { ChatContext, ChatGrant } from './chatApi';
import { CHAT_NO_GRANTS, CHAT_REVOKE_GRANT, chatContextLine, chatSpillWarning } from './chatCopy';

/**
 * ChatSettings is the chat's Settings tab: what has been authorized for the
 * session ("Allow for session"), one row per grant, as the server lists them
 * (GET /chats/{id}/grants). A grant covers a tool and action on one repo or
 * lens and lasts until the chat ends. It is read when the tab opens. Each row
 * can be revoked: the grant goes and the next such call asks again.
 */
export function ChatSettings({ chatId }: { chatId: string }) {
  const [grants, setGrants] = useState<ChatGrant[] | null>(null);
  const [err, setErr] = useState('');
  const [ctx, setCtx] = useState<ChatContext | null>(null);
  // The context line is informational: a failure to read it shows nothing.
  useEffect(() => {
    let cancelled = false;
    getChatContext(chatId).then(c => { if (!cancelled) setCtx(c); }).catch(() => {});
    return () => { cancelled = true; };
  }, [chatId]);
  useEffect(() => {
    let cancelled = false;
    listChatGrants(chatId)
      .then(g => { if (!cancelled) { setGrants(g); setErr(''); } })
      .catch(e => { if (!cancelled) setErr(e instanceof Error ? e.message : String(e)); });
    return () => { cancelled = true; };
  }, [chatId]);
  const revoke = (g: ChatGrant) => {
    setErr('');
    revokeChatGrant(chatId, g)
      .then(() => setGrants(cur => cur && cur.filter(x => x !== g)))
      .catch(e => {
        // Not held any more (404): the row is stale, so it goes too.
        if ((e as { status?: number } | null)?.status === 404) setGrants(cur => cur && cur.filter(x => x !== g));
        else setErr(e instanceof Error ? e.message : String(e));
      });
  };
  return (
    <div data-testid="chat-settings" style={page}>
      {ctx && ctx.window > 0 && <div data-testid="chat-context" style={{ color: '#ddd', fontSize: 13, marginBottom: 12 }}>{chatContextLine(ctx.window, ctx.how ?? '')}</div>}
      {ctx && ctx.spill && <div data-testid="chat-context-spill" style={{ color: '#e0c080', fontSize: 13, marginBottom: 12 }}>{chatSpillWarning(ctx.window)}</div>}
      {err && <div role="alert" style={{ color: '#e0a0a0', fontSize: 13 }}>{err}</div>}
      {grants && grants.length === 0 && <div data-testid="chat-grants-empty" style={{ color: '#888', fontSize: 13 }}>{CHAT_NO_GRANTS}</div>}
      {grants && grants.map((g, i) => (
        <div key={i} data-testid="chat-grant-row" style={rowStyle}>
          <span data-testid="chat-grant-target" title={g.kind} style={{ color: '#ddd' }}>{g.target}</span>
          <span data-testid="chat-grant-tool" style={mono}>{g.tool}</span>
          <span data-testid="chat-grant-action" style={mono}>{g.action}</span>
          <button type="button" data-testid="chat-grant-revoke" onClick={() => revoke(g)} style={revokeBtn}>{CHAT_REVOKE_GRANT}</button>
        </div>
      ))}
    </div>
  );
}

const page: CSSProperties = { flex: 1, minHeight: 0, overflowY: 'auto', padding: '24px 28px', maxWidth: 640 };
const rowStyle: CSSProperties = { display: 'flex', gap: 14, alignItems: 'baseline', padding: '8px 0', borderBottom: '1px solid #222', fontSize: 13 };
const mono: CSSProperties = { fontFamily: 'var(--k-font-mono)', fontSize: 12, color: '#aaa' };
const revokeBtn: CSSProperties = {
  marginLeft: 'auto', background: 'transparent', color: '#e0a0a0', border: '1px solid #5c2a2a',
  borderRadius: 4, padding: '2px 10px', fontSize: 12, cursor: 'pointer', fontFamily: 'inherit',
};
