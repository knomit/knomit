import { useCallback, useEffect, useReducer, useRef, useState } from 'react';
import { FacetColumns } from './FacetColumns';
import { ChatPanel } from './ChatPanel';
import { ChatResultPanel } from './ChatResultPanel';
import { ChatSettings } from './ChatSettings';
import {
  answerChatApproval, cancelChatTurn, endChatOnPageHide, getChat, openChatEvents, sendChatTurn,
} from './chatApi';
import type { ApprovalDecision, ChatApproval, ChatInfo } from './chatApi';
import { chatReducer, initialChatState } from './chatState';
import type { ToolCall, UsagePayload } from './chatState';

const errText = (e: unknown): string => (e instanceof Error ? e.message : String(e));

/** The status an HttpError carries (api.ts), if any. */
const statusOf = (e: unknown): number | undefined => (e as { status?: number } | null)?.status;

/**
 * useChatSession owns one chat's connection: the events stream (opened through
 * openEventStream, so the desktop's Wails transport carries it too), the
 * transcript reducer, and the actions. Every connect begins with `ready`, on
 * which the page refetches GET /chats/{id} and resets to it; there is no
 * replay. A chat the server no longer has ends the session (`onGone`).
 */
function useChatSession(chatId: string, onGone: (id: string) => void) {
  const [state, dispatch] = useReducer(chatReducer, initialChatState);
  const [actionError, setActionError] = useState('');
  const goneRef = useRef(onGone);
  const sendSeq = useRef(0);
  const acceptedRef = useRef(new Set<number>());
  useEffect(() => { goneRef.current = onGone; }, [onGone]);

  useEffect(() => {
    const es = openChatEvents(chatId);
    let closed = false;
    // Snapshot reads are numbered and only the LATEST one started is applied.
    // A turn can end while an earlier read is in flight; that read's answer
    // (cut before the end, so running:true) must not land after, and wedge the
    // chat on a turn that is over. The read started at the end supersedes it.
    let reads = 0;
    const refresh = () => {
      const mine = ++reads;
      // The sends the server had accepted when this read started: it lists them.
      const confirmed = [...acceptedRef.current];
      getChat(chatId)
        .then(snapshot => { if (!closed && mine === reads) dispatch({ kind: 'ready', snapshot, confirmed }); })
        .catch(e => { if (statusOf(e) === 404) gone(); else if (!closed && mine === reads) setActionError(errText(e)); });
    };
    const gone = () => { if (!closed) goneRef.current(chatId); };
    const on = (name: string, fn: (data: Record<string, unknown>) => void) => {
      es.addEventListener(name, (e: MessageEvent) => {
        let data: Record<string, unknown> = {};
        try { data = e.data ? JSON.parse(e.data) as Record<string, unknown> : {}; } catch { return; }
        fn(data);
      });
    };
    on('ready', refresh);
    on('text_delta', d => dispatch({ kind: 'text_delta', text: String(d.text ?? '') }));
    on('tool_call', d => dispatch({
      kind: 'tool_call', id: String(d.id), name: String(d.name), state: d.state === 'done' ? 'done' : 'running', is_error: d.is_error === true,
    }));
    on('approval', d => dispatch({ kind: 'approval', approval: d as unknown as ChatApproval }));
    on('usage', d => dispatch({
      kind: 'usage', usage: d.usage as UsagePayload,
      ...(typeof d.cost_usd === 'number' ? { cost_usd: d.cost_usd } : {}),
      ...(typeof d.seq === 'number' ? { seq: d.seq } : {}),
    }));
    on('warning', d => dispatch({ kind: 'warning', message: String(d.message ?? '') }));
    // A turn's end is shown at once, and the transcript is read again: the
    // server's view is complete by now, and it settles anything a read in
    // flight at this moment got wrong.
    on('turn_done', () => { dispatch({ kind: 'turn_done' }); refresh(); });
    on('turn_error', d => {
      dispatch({ kind: 'turn_error', message: String(d.message ?? ''), cancelled: d.cancelled === true });
      refresh();
    });
    on('chat_end', () => { dispatch({ kind: 'chat_end' }); gone(); });
    // A refused subscription is the server saying the chat is not there
    // (404). A plain connection loss is retried by the stream itself, and the
    // next `ready` refetches.
    es.onerror = (e: unknown) => { if ((e as { status?: number } | null)?.status === 404) gone(); };
    return () => { closed = true; es.close(); };
  }, [chatId]);

  // The page is going away: the chat is over (a chat lives as long as its tab).
  useEffect(() => {
    const onHide = () => endChatOnPageHide(chatId);
    window.addEventListener('pagehide', onHide);
    return () => window.removeEventListener('pagehide', onHide);
  }, [chatId]);

  const send = useCallback((text: string) => {
    setActionError('');
    const sendId = ++sendSeq.current;
    dispatch({ kind: 'user', text, sendId });
    sendChatTurn(chatId, text).then(() => {
      acceptedRef.current.add(sendId);
      dispatch({ kind: 'send_accepted', sendId });
    }).catch(e => {
      // Refused (a 409 while the stopped turn still unwinds, any other
      // non-2xx), a network error or no answer in time: the turn never
      // started, so it leaves the transcript, and the error is shown. A 404
      // means the chat is gone.
      dispatch({ kind: 'send_failed', sendId });
      if (statusOf(e) === 404) goneRef.current(chatId);
      else setActionError(errText(e));
    });
  }, [chatId]);
  const stop = useCallback(() => {
    // Stop ends the turn here whatever the stream says: a turn end that never
    // arrives must not leave the chat unable to send.
    cancelChatTurn(chatId).catch(e => setActionError(errText(e)));
    dispatch({ kind: 'stopped' });
  }, [chatId]);
  const answer = useCallback((callId: string, decision: ApprovalDecision) => {
    setActionError('');
    answerChatApproval(chatId, callId, decision)
      .then(() => dispatch({ kind: 'approval_answered', callId }))
      .catch(e => {
        // Not pending any more (the turn was cancelled): the card is stale.
        if (statusOf(e) === 404) dispatch({ kind: 'approval_answered', callId });
        else setActionError(errText(e));
      });
  }, [chatId]);
  return { state, actionError, send, stop, answer };
}

/**
 * ChatFacet is the body of a chat target, and it has Browse's layout: the chat
 * panel where Browse's results list sits (the left column, the same width and
 * splitter), and, once a tool row is selected, the result panel where Browse's
 * single fact sits. With no row selected there is no result panel. The Settings
 * tab shows what the chat has authorized.
 *
 * App keeps one mounted per live chat, so a chat's stream and transcript
 * survive the reader looking at a repository.
 */
export function ChatFacet({ chat, facet, onGone }: {
  chat: ChatInfo;
  /** The tab on screen. */
  facet: 'chat' | 'settings';
  /** The chat is over; it leaves the list. */
  onGone: (id: string) => void;
}) {
  const { state, actionError, send, stop, answer } = useChatSession(chat.id, onGone);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const onSelect = useCallback((c: ToolCall) => setSelectedId(cur => (cur === c.id ? null : c.id)), []);

  // The chat stays mounted under Settings (hidden), so what was typed and
  // which row was open are still there on the way back.
  return (
    <>
      {facet === 'settings' && <ChatSettings chatId={chat.id} />}
      <div data-testid="chat-facet" style={{ flex: 1, minHeight: 0, display: facet === 'chat' ? 'flex' : 'none', flexDirection: 'column', overflow: 'hidden' }}>
        <FacetColumns
          testId="chat-columns"
          left={<ChatPanel state={state} selectedId={selectedId} actionError={actionError}
            onSend={send} onStop={stop} onSelect={onSelect} onAnswer={answer} />}
        >
          {selectedId && <ChatResultPanel chatId={chat.id} callId={selectedId} />}
        </FacetColumns>
      </div>
    </>
  );
}
