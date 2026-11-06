import { useState } from 'react';
import type { CSSProperties } from 'react';
import { createChat } from './chatApi';
import type { ChatInfo, ChatProvider } from './chatApi';
import { DRAFT_NAME } from './facets';
import { CHAT_NO_MODELS } from './chatCopy';

// The body of the chat DRAFT (the rail's "New chat" row): the configured
// providers, and for each only the models the server lists as available. A
// model that cannot call tools is shown but unusable, with the server's reason.
// Picking a usable model starts the chat; there is no second step.
export function ChatStartForm({ providers, onCreated, onCancel }: {
  providers: ChatProvider[];
  /** The chat exists. The caller adds it to its list and pins it in one update. */
  onCreated: (chat: ChatInfo) => void;
  onCancel?: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');

  const start = async (provider: string, model: string) => {
    if (busy) return;
    setBusy(true);
    setErr('');
    try {
      const chat = await createChat(provider, model);
      setBusy(false);
      onCreated(chat);
    } catch (e) {
      setBusy(false);
      setErr(e instanceof Error ? e.message : String(e));
    }
  };

  const shown = providers
    .map(p => ({ ...p, models: p.models.filter(m => m.available) }))
    .filter(p => p.models.length > 0);

  return (
    <div data-testid="chat-start-form" style={page}>
      <h2 style={heading}>{DRAFT_NAME.chat}</h2>
      {shown.length === 0 && <div data-testid="chat-no-models" style={{ color: '#888', fontSize: 13 }}>{CHAT_NO_MODELS}</div>}
      {shown.map(p => (
        <section key={p.name} data-testid={`chat-provider-${p.name}`} style={{ marginBottom: 18 }}>
          <div style={providerName}>{p.name}</div>
          {p.models.map(m => (
            <div key={m.id} style={{ marginBottom: 6 }}>
              <button
                type="button"
                data-testid={`chat-model-${p.name}-${m.id}`}
                disabled={busy || !m.tools}
                onClick={() => { void start(p.name, m.id); }}
                style={modelBtn(!m.tools || busy)}
                title={m.id}
              >
                {m.name || m.id}
              </button>
              {!m.tools && m.reason && <span data-testid={`chat-model-reason-${p.name}-${m.id}`} style={reason}>{m.reason}</span>}
            </div>
          ))}
        </section>
      ))}
      {err && <div role="alert" data-testid="chat-start-error" style={{ color: '#e0a0a0', fontSize: 12, marginTop: 8 }}>{err}</div>}
      {onCancel && (
        <button type="button" data-testid="chat-cancel" disabled={busy} onClick={onCancel} style={cancelBtn}>Cancel</button>
      )}
    </div>
  );
}

const page: CSSProperties = { flex: 1, minHeight: 0, overflowY: 'auto', padding: '24px 28px', maxWidth: 560 };
const heading: CSSProperties = { margin: '0 0 16px', fontSize: 16, fontWeight: 600, color: '#ddd' };
const providerName: CSSProperties = { fontSize: 11, fontWeight: 600, letterSpacing: '0.08em', textTransform: 'uppercase', color: '#9a9a9a', marginBottom: 6 };
const modelBtn = (disabled: boolean): CSSProperties => ({
  background: disabled ? 'transparent' : '#1a1a1a', color: disabled ? '#666' : '#ddd',
  border: '1px solid #2a2a2a', borderRadius: 4, padding: '7px 12px', fontSize: 13,
  fontFamily: 'var(--k-font-mono)', cursor: disabled ? 'default' : 'pointer',
});
const reason: CSSProperties = { color: '#888', fontSize: 12, marginLeft: 10 };
const cancelBtn: CSSProperties = {
  marginTop: 12, background: 'transparent', color: '#aaa', border: '1px solid #333',
  borderRadius: 4, padding: '6px 12px', fontSize: 12, cursor: 'pointer',
};
