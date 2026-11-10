import { useEffect, useMemo, useRef, useState } from 'react';
import type { CSSProperties } from 'react';
import ReactMarkdown from 'react-markdown';
import { markdownPlugins, markdownComponents } from './markdown';
import { getChatCall } from './chatApi';
import type { ChatCall } from './chatApi';
import { materializedMarkdown } from './chatState';
import type { ResolvedFact } from './chatFactLinks';
import { JsonView } from './JsonView';
import type { JsonViewHandle } from './JsonView';
import { jsonText, resultText } from './jsonViewText';
import { useKbLinks } from './jsonViewLinks';
import { CHAT_RESULT_LOADING, PANEL_TAB_RESULT, PANEL_TAB_ARGUMENTS } from './chatCopy';

/**
 * ChatResultPanel is where a selected tool call's result is shown, inside the
 * side panel. The result is fetched when the call is opened and not before
 * (GET /chats/{id}/calls/{callId}), so a large result costs nothing until
 * someone asks for it. It has two tabs, Result and Arguments. A materialize
 * result is a fact, so the Result tab renders it as one; any other result, and
 * the arguments, are shown in the JSON view (JsonView): unpacked, pretty-printed
 * and folded when they are JSON, as the text they are when not. The call's
 * heading (its timeline row) is the side panel's top row, not drawn here.
 */
export function ChatResultPanel({ chatId, callId, onCall, onOpenFact, onFinder }: {
  chatId: string;
  callId: string;
  /** Told the call once it has loaded (the side panel's header reads it). */
  onCall?: (callId: string, call: ChatCall) => void;
  /** A kb:// path in the JSON view was clicked: open its fact. */
  onOpenFact?: (f: ResolvedFact) => void;
  /** The JSON view on screen, to open its find bar (null: none is). */
  onFinder?: (h: JsonViewHandle | null) => void;
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
  const materialized = call?.name === 'knomit_materialize' && !call.is_error;
  const view = useMemo(() => {
    if (!call) return null;
    if (active === 'arguments') return jsonText(call.arguments);
    return materialized ? null : resultText(call.result);
  }, [call, active, materialized]);
  const links = useKbLinks(view?.text ?? null);
  return (
    <div data-testid="chat-result-panel" style={panel}>
      {!shown && <div data-testid="chat-result-loading" style={{ color: '#777', fontSize: 13 }}>{CHAT_RESULT_LOADING}</div>}
      {shown?.error && <div role="alert" data-testid="chat-result-error" style={{ color: '#e0a0a0', fontSize: 13 }}>{shown.error}</div>}
      {call && (
        <>
          <div role="tablist" style={tabs}>
            {(['result', 'arguments'] as const).map(t => (
              <button key={t} role="tab" type="button" data-testid={`chat-result-tab-${t}`} aria-selected={active === t}
                onClick={() => setTab({ callId, tab: t })} style={tabBtn(active === t)}>
                {t === 'result' ? PANEL_TAB_RESULT : PANEL_TAB_ARGUMENTS}
              </button>
            ))}
          </div>
          {active === 'result' && materialized && (
            <div data-testid="chat-result-fact" className="k-prose" style={factBox}>
              <ReactMarkdown remarkPlugins={markdownPlugins} components={markdownComponents}>{materializedMarkdown(call.result)}</ReactMarkdown>
            </div>
          )}
          {view && (
            <div style={{ flex: 1, minHeight: 0 }}>
              <JsonView key={`${callId}:${active}`} testId={active === 'result' ? 'chat-result-text' : 'chat-result-args'}
                text={view.text} json={view.json} error={active === 'result' && call.is_error}
                links={links} onOpenFact={onOpenFact} onReady={onFinder} />
            </div>
          )}
        </>
      )}
    </div>
  );
}

// The least chrome, the most room (user, 2026-11-09): 12px sides, no rules
// between the header, the tabs and the view. The panel
// is a column: the tabs, then the content, which scrolls itself (so the JSON
// view's find bar stays at its foot).
const panel: CSSProperties = { height: '100%', display: 'flex', flexDirection: 'column', padding: '0 12px', boxSizing: 'border-box' };
// No rule under the tab row: the active tab's underline marks it, and the view's frame starts below.
const tabs: CSSProperties = { display: 'flex', gap: 2, marginBottom: 6, flexShrink: 0 };
const tabBtn = (on: boolean): CSSProperties => ({
  background: 'none', border: 'none', borderRadius: 0, padding: '7px 9px', cursor: 'pointer', fontSize: 11.5,
  color: on ? '#eee' : '#777', boxShadow: on ? 'inset 0 -2px 0 #6ea8fe' : 'none',
});
const factBox: CSSProperties = { color: '#c4c4c4', lineHeight: 1.7, fontSize: 13.5, flex: 1, minHeight: 0, overflowY: 'auto' };
