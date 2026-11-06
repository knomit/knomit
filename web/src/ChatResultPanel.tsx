import { useEffect, useRef, useState } from 'react';
import type { CSSProperties } from 'react';
import ReactMarkdown from 'react-markdown';
import { markdownPlugins, markdownComponents } from './markdown';
import { getChatCall } from './chatApi';
import type { ChatCall } from './chatApi';
import { toolRowLabel, materializedMarkdown } from './chatState';
import { CHAT_RESULT_LOADING, PANEL_TAB_RESULT, PANEL_TAB_ARGUMENTS } from './chatCopy';

/**
 * ChatResultPanel is where a selected tool call's result is shown, inside the
 * side panel. The result is fetched when the call is opened and not before
 * (GET /chats/{id}/calls/{callId}), so a large result costs nothing until
 * someone asks for it. It has two tabs, Result and Arguments. A materialize
 * result is a fact, so the Result tab renders it as one; any other result is
 * shown as the text it is.
 */
export function ChatResultPanel({ chatId, callId, onCall }: {
  chatId: string;
  callId: string;
  /** Told the call once it has loaded (the side panel's breadcrumb reads it). */
  onCall?: (callId: string, call: ChatCall) => void;
}) {
  const [tab, setTab] = useState<{ callId: string; tab: 'result' | 'arguments' }>({ callId, tab: 'result' });
  // A different call starts on Result.
  const active = tab.callId === callId ? tab.tab : 'result';
  const [loaded, setLoaded] = useState<{ callId: string; call?: ChatCall; error?: string } | null>(null);
  const onCallRef = useRef(onCall);
  useEffect(() => { onCallRef.current = onCall; });
  useEffect(() => {
    let cancelled = false;
    getChatCall(chatId, callId)
      .then(call => { if (!cancelled) { setLoaded({ callId, call }); onCallRef.current?.(callId, call); } })
      .catch(e => { if (!cancelled) setLoaded({ callId, error: e instanceof Error ? e.message : String(e) }); });
    return () => { cancelled = true; };
  }, [chatId, callId]);

  // An answer for another call than the selected one is the previous row's.
  const shown = loaded?.callId === callId ? loaded : null;
  const call = shown?.call;
  return (
    <div data-testid="chat-result-panel" style={panel}>
      {!shown && <div data-testid="chat-result-loading" style={{ color: '#777', fontSize: 13 }}>{CHAT_RESULT_LOADING}</div>}
      {shown?.error && <div role="alert" data-testid="chat-result-error" style={{ color: '#e0a0a0', fontSize: 13 }}>{shown.error}</div>}
      {call && (
        <>
          <div data-testid="chat-result-title" style={title}>{toolRowLabel(call.name)}</div>
          <div role="tablist" style={tabs}>
            {(['result', 'arguments'] as const).map(t => (
              <button key={t} role="tab" type="button" data-testid={`chat-result-tab-${t}`} aria-selected={active === t}
                onClick={() => setTab({ callId, tab: t })} style={tabBtn(active === t)}>
                {t === 'result' ? PANEL_TAB_RESULT : PANEL_TAB_ARGUMENTS}
              </button>
            ))}
          </div>
          {active === 'result' && (call.name === 'knomit_materialize' && !call.is_error ? (
            <div data-testid="chat-result-fact" className="k-prose" style={{ color: '#c4c4c4', lineHeight: 1.7, fontSize: 13.5 }}>
              <ReactMarkdown remarkPlugins={markdownPlugins} components={markdownComponents}>{materializedMarkdown(call.result)}</ReactMarkdown>
            </div>
          ) : (
            <pre data-testid="chat-result-text" data-error={call.is_error || undefined} style={pre(call.is_error)}>{call.result}</pre>
          ))}
          {active === 'arguments' && (
            <pre data-testid="chat-result-args" style={{ ...pre(false), color: '#aaa' }}>{JSON.stringify(call.arguments, null, 2)}</pre>
          )}
        </>
      )}
    </div>
  );
}

// Styling is the mockup's side-panel fact: .sfact (padding, h1, p) and .stabs.
const panel: CSSProperties = { height: '100%', overflowY: 'auto', padding: '18px 20px', boxSizing: 'border-box' };
const title: CSSProperties = {
  fontFamily: 'var(--k-font-display)', fontSize: 19, fontWeight: 600, letterSpacing: '-.2px', lineHeight: 1.3,
  color: '#eee', margin: '0 0 12px',
};
const tabs: CSSProperties = { display: 'flex', gap: 2, borderBottom: '1px solid #1e1e1e', marginBottom: 14 };
const tabBtn = (on: boolean): CSSProperties => ({
  background: 'none', border: 'none', borderRadius: 0, padding: '7px 9px', cursor: 'pointer', fontSize: 11.5,
  color: on ? '#eee' : '#777', boxShadow: on ? 'inset 0 -2px 0 #6ea8fe' : 'none',
});
const pre = (error: boolean): CSSProperties => ({
  margin: 0, padding: '10px 12px', fontSize: 12, lineHeight: 1.6, whiteSpace: 'pre-wrap', wordBreak: 'break-word',
  fontFamily: 'var(--k-font-mono)', background: '#0c0c0c', border: '1px solid #1e1e1e', borderRadius: 6,
  color: error ? '#e0a0a0' : '#c4c4c4',
});
