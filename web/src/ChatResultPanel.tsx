import { useEffect, useState } from 'react';
import type { CSSProperties } from 'react';
import ReactMarkdown from 'react-markdown';
import { markdownPlugins, markdownComponents } from './markdown';
import { getChatCall } from './chatApi';
import type { ChatCall } from './chatApi';
import { toolRowLabel } from './chatState';
import { CHAT_RESULT_LOADING_DRAFT } from './chatCopy';

/** The markdown a knomit_materialize result stands for: its document `text`
 *  (audience "agent"), or its facts (audience "human") as a title and a body
 *  each. Anything else is shown as it came. */
export function materializedMarkdown(result: string): string {
  try {
    const v = JSON.parse(result) as { text?: unknown; facts?: unknown };
    if (typeof v.text === 'string') return v.text;
    if (Array.isArray(v.facts)) {
      return v.facts.map((f: { title?: string; body?: string }) =>
        `${f.title ? `# ${f.title}\n\n` : ''}${f.body ?? ''}`).join('\n\n---\n\n');
    }
  } catch { /* not JSON: the text itself */ }
  return result;
}

/**
 * ChatResultPanel is where a selected tool call's result is shown, on the
 * right, where Browse shows a fact. The result is fetched when the row is
 * selected and not before (GET /chats/{id}/calls/{callId}), so a large result
 * costs nothing until someone asks for it. A materialize result is a fact, so
 * it is rendered as one; any other result is shown as the text it is.
 */
export function ChatResultPanel({ chatId, callId }: { chatId: string; callId: string }) {
  const [loaded, setLoaded] = useState<{ callId: string; call?: ChatCall; error?: string } | null>(null);
  useEffect(() => {
    let cancelled = false;
    getChatCall(chatId, callId)
      .then(call => { if (!cancelled) setLoaded({ callId, call }); })
      .catch(e => { if (!cancelled) setLoaded({ callId, error: e instanceof Error ? e.message : String(e) }); });
    return () => { cancelled = true; };
  }, [chatId, callId]);

  // An answer for another call than the selected one is the previous row's.
  const shown = loaded?.callId === callId ? loaded : null;
  const call = shown?.call;
  return (
    <div data-testid="chat-result-panel" style={panel}>
      {!shown && <div data-testid="chat-result-loading" style={{ color: '#777', fontSize: 13 }}>{CHAT_RESULT_LOADING_DRAFT}</div>}
      {shown?.error && <div role="alert" data-testid="chat-result-error" style={{ color: '#e0a0a0', fontSize: 13 }}>{shown.error}</div>}
      {call && (
        <>
          <div data-testid="chat-result-title" style={title}>{toolRowLabel(call.name)}</div>
          {call.name === 'knomit_materialize' && !call.is_error ? (
            <div data-testid="chat-result-fact" className="k-prose" style={{ color: '#ccc', lineHeight: 1.7, fontSize: 14 }}>
              <ReactMarkdown remarkPlugins={markdownPlugins} components={markdownComponents}>{materializedMarkdown(call.result)}</ReactMarkdown>
            </div>
          ) : (
            <pre data-testid="chat-result-text" data-error={call.is_error || undefined} style={pre(call.is_error)}>{call.result}</pre>
          )}
          {JSON.stringify(call.arguments, null, 2) !== '{}' && (
            <pre data-testid="chat-result-args" style={{ ...pre(false), marginTop: 14, color: '#888' }}>{JSON.stringify(call.arguments, null, 2)}</pre>
          )}
        </>
      )}
    </div>
  );
}

const panel: CSSProperties = { height: '100%', overflowY: 'auto', padding: '14px 18px', boxSizing: 'border-box' };
const title: CSSProperties = { fontSize: 12, fontWeight: 600, color: '#9a9a9a', marginBottom: 10 };
const pre = (error: boolean): CSSProperties => ({
  margin: 0, fontSize: 12, whiteSpace: 'pre-wrap', wordBreak: 'break-word', fontFamily: 'var(--k-font-mono)',
  color: error ? '#e0a0a0' : '#bbb',
});
