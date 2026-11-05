import { useEffect, useState } from 'react';
import type { CSSProperties } from 'react';
import { listChatGrants } from './chatApi';
import type { ChatGrant } from './chatApi';
import { CHAT_NO_GRANTS_DRAFT } from './chatCopy';

/**
 * ChatSettings is the chat's Settings tab: what has been authorized for the
 * session ("Allow for session"), one row per grant, as the server lists them
 * (GET /chats/{id}/grants). A grant covers a tool and action on one repo or
 * lens and lasts until the chat ends. It is read when the tab opens.
 */
export function ChatSettings({ chatId }: { chatId: string }) {
  const [grants, setGrants] = useState<ChatGrant[] | null>(null);
  const [err, setErr] = useState('');
  useEffect(() => {
    let cancelled = false;
    listChatGrants(chatId)
      .then(g => { if (!cancelled) { setGrants(g); setErr(''); } })
      .catch(e => { if (!cancelled) setErr(e instanceof Error ? e.message : String(e)); });
    return () => { cancelled = true; };
  }, [chatId]);
  return (
    <div data-testid="chat-settings" style={page}>
      {err && <div role="alert" style={{ color: '#e0a0a0', fontSize: 13 }}>{err}</div>}
      {grants && grants.length === 0 && <div data-testid="chat-grants-empty" style={{ color: '#888', fontSize: 13 }}>{CHAT_NO_GRANTS_DRAFT}</div>}
      {grants && grants.map((g, i) => (
        <div key={i} data-testid="chat-grant-row" style={rowStyle}>
          <span data-testid="chat-grant-target" title={g.kind} style={{ color: '#ddd' }}>{g.target}</span>
          <span data-testid="chat-grant-tool" style={mono}>{g.tool}</span>
          <span data-testid="chat-grant-action" style={mono}>{g.action}</span>
        </div>
      ))}
    </div>
  );
}

const page: CSSProperties = { flex: 1, minHeight: 0, overflowY: 'auto', padding: '24px 28px', maxWidth: 640 };
const rowStyle: CSSProperties = { display: 'flex', gap: 14, alignItems: 'baseline', padding: '8px 0', borderBottom: '1px solid #222', fontSize: 13 };
const mono: CSSProperties = { fontFamily: 'var(--k-font-mono)', fontSize: 12, color: '#aaa' };
