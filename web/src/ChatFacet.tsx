import { useCallback, useEffect, useReducer, useRef, useState } from 'react';
import { publishChatPill, forgetChatPill } from './chatPillStore';
import { ChatPanel } from './ChatPanel';
import { ChatResultPanel } from './ChatResultPanel';
import { ChatSettings } from './ChatSettings';
import {
  answerChatApproval, cancelChatTurn, endChatOnPageHide, getChat, getChatContext, openChatEvents, sendChatTurn,
} from './chatApi';
import type { ApprovalDecision, ChatApproval, ChatInfo, ChatNotice } from './chatApi';
import { NEW_CHAT_ERROR } from './chatCopy';
import { chatReducer, initialChatState, refusalReason, sessionCost } from './chatState';
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
  // The last send the server refused (or never answered): its text, and why.
  const [notSent, setNotSent] = useState<{ text: string; reason: string } | null>(null);
  // A send the person had stopped (its text was put back in the composer) that the server accepted after all.
  const [landed, setLanded] = useState<{ text: string } | null>(null);
  // The server turns the page knew when each send went out, to tell whether a snapshot lists that send.
  const turnsBefore = useRef(new Map<number, number>());
  const goneRef = useRef(onGone);
  const sendSeq = useRef(0);
  // The id each send goes out under, so a Stop can name it to the server; and
  // the sends the person stopped before the server answered.
  const wireIds = useRef(new Map<number, string>());
  const withdrawn = useRef(new Set<number>());
  const [tag] = useState(() => Math.random().toString(36).slice(2, 10));
  const stateRef = useRef(state);
  useEffect(() => { stateRef.current = state; });
  const acceptedRef = useRef(new Set<number>());
  const refreshRef = useRef<() => void>(() => {});
  useEffect(() => { goneRef.current = onGone; }, [onGone]);

  useEffect(() => {
    const es = openChatEvents(chatId);
    let closed = false;
    // Snapshot reads are COALESCED: one is in flight at a time. A trigger that
    // arrives meanwhile (a turn end, an event for a turn the page does not
    // know, a send with no answer) only marks that another is wanted; when the
    // one in flight answers it is not applied (it was cut before the trigger and
    // may be stale: a turn end it missed would leave the chat running) and ONE
    // more read follows, however many triggers arrived.
    let inFlight = false;
    let again = false;
    const read = () => {
      inFlight = true;
      again = false;
      // The sends the server had accepted when this read started: it lists them.
      const confirmed = [...acceptedRef.current];
      getChat(chatId)
        .then(snapshot => {
          inFlight = false;
          if (closed) return;
          if (again) { read(); return; }
          dispatch({ kind: 'ready', snapshot, confirmed });
        })
        .catch(e => {
          inFlight = false;
          if (statusOf(e) === 404) { gone(); return; }
          if (closed) return;
          if (again) { read(); return; }
          setActionError(errText(e));
        });
    };
    const refresh = () => { if (inFlight) again = true; else read(); };
    refreshRef.current = refresh;
    const gone = () => { if (!closed) goneRef.current(chatId); };
    const turnOf = (d: Record<string, unknown>): number => Number(d.turn);
    const on = (name: string, fn: (data: Record<string, unknown>) => void) => {
      es.addEventListener(name, (e: MessageEvent) => {
        let data: Record<string, unknown> = {};
        try { data = e.data ? JSON.parse(e.data) as Record<string, unknown> : {}; } catch { return; }
        fn(data);
      });
    };
    on('ready', refresh);
    on('text_delta', d => dispatch({ kind: 'text_delta', text: String(d.text ?? ''), turn: turnOf(d) }));
    on('thinking_delta', d => dispatch({ kind: 'thinking_delta', text: String(d.text ?? ''), turn: turnOf(d) }));
    on('tool_call', d => dispatch({
      kind: 'tool_call', id: String(d.id), name: String(d.name), state: d.state === 'done' ? 'done' : 'running', is_error: d.is_error === true,
      ...(d.arguments !== undefined ? { arguments: d.arguments } : {}),
      ...(typeof d.duration_ms === 'number' ? { duration_ms: d.duration_ms } : {}),
      ...(d.summary && typeof (d.summary as { count?: unknown }).count === 'number' ? { summary: { count: (d.summary as { count: number }).count } } : {}),
      turn: turnOf(d),
    }));
    on('approval', d => dispatch({ kind: 'approval', approval: d as unknown as ChatApproval }));
    on('usage', d => dispatch({
      kind: 'usage', usage: d.usage as UsagePayload,
      ...(typeof d.cost_usd === 'number' ? { cost_usd: d.cost_usd } : {}),
      ...(typeof d.seq === 'number' ? { seq: d.seq } : {}),
      turn: turnOf(d),
    }));
    on('warning', d => dispatch({
      kind: 'warning', message: String(d.message ?? ''), turn: turnOf(d),
      ...(d.kind === 'price_tier' ? { notice: d as unknown as ChatNotice } : {}),
    }));
    // A turn's end is shown at once, and the transcript is read again: the
    // server's view is complete by now, and it settles anything a read in
    // flight at this moment got wrong.
    on('turn_done', d => { dispatch({ kind: 'turn_done', turn: turnOf(d) }); refresh(); });
    on('turn_error', d => {
      dispatch({
        kind: 'turn_error', message: String(d.message ?? ''), cancelled: d.cancelled === true, turn: turnOf(d),
        ...(d.error_info ? { error_info: d.error_info as ChatNotice } : {}),
      });
      refresh();
    });
    on('chat_end', () => { dispatch({ kind: 'chat_end' }); gone(); });
    // A refused subscription is the server saying the chat is not there
    // (404). A plain connection loss is retried by the stream itself, and the
    // next `ready` refetches.
    es.onerror = (e: unknown) => { if ((e as { status?: number } | null)?.status === 404) gone(); };
    return () => { closed = true; es.close(); };
  }, [chatId]);

  // An event named a turn the page has no turn for and no local turn could
  // take (a turn started elsewhere): read the snapshot.
  useEffect(() => { if (state.staleSeq > 0) refreshRef.current(); }, [state.staleSeq]);

  // The page is going away: the chat is over (a chat lives as long as its tab).
  useEffect(() => {
    const onHide = () => endChatOnPageHide(chatId);
    window.addEventListener('pagehide', onHide);
    return () => window.removeEventListener('pagehide', onHide);
  }, [chatId]);

  /** Whether the server accepted send `sendId`: read the snapshot and see
   *  whether it lists one more person message than the page knew when the send
   *  went out. The snapshot is applied either way. */
  const accepted = useCallback((sendId: number): Promise<boolean> => getChat(chatId).then(snapshot => {
    const known = turnsBefore.current.get(sendId) ?? 0;
    const listed = snapshot.items.filter(i => i.kind === 'user').length;
    if (listed > known) acceptedRef.current.add(sendId);
    dispatch({ kind: 'ready', snapshot, confirmed: [...acceptedRef.current] });
    return listed > known;
  }), [chatId]);
  const send = useCallback((text: string) => {
    setActionError('');
    setNotSent(null);
    setLanded(null);
    const sendId = ++sendSeq.current;
    const wire = `${tag}-${sendId}`;
    wireIds.current.set(sendId, wire);
    turnsBefore.current.set(sendId, stateRef.current.turns.filter(t => !t.local).length);
    dispatch({ kind: 'user', text, sendId });
    sendChatTurn(chatId, text, wire).then(body => {
      acceptedRef.current.add(sendId);
      // The person stopped it first, but the server had it: it ends that turn.
      // The text was put back in the composer, and the transcript will list the
      // turn, so it must not stay in both places.
      if (withdrawn.current.has(sendId)) {
        accepted(sendId).then(yes => { if (yes) setLanded({ text }); }).catch(() => refreshRef.current());
        return;
      }
      dispatch({ kind: 'send_accepted', sendId, turn: Number((body as { turn?: number } | null)?.turn), at: Date.now() });
    }).catch(e => {
      // Refused (a 409 while the stopped turn still unwinds, any other
      // non-2xx), a network error or no answer in time: the turn never
      // started, so it leaves the transcript, and its text goes back in the
      // composer with the reason. A 404 means the chat is gone.
      dispatch({ kind: 'send_failed', sendId });
      if (withdrawn.current.has(sendId)) return; // the person's own Stop: nothing to explain
      if (statusOf(e) === 404) { goneRef.current(chatId); return; }
      const reason = `${refusalReason(errText(e))}${statusOf(e) ? ` (${statusOf(e)})` : ''}`;
      if (statusOf(e) !== undefined) { setNotSent({ text, reason }); return; }
      // No status means no answer (a timeout, a dropped connection): the server
      // may have appended the turn anyway. Ask it: if it has the turn, the
      // text is in the transcript and must not also wait in the composer (Retry
      // would send it twice).
      accepted(sendId).then(yes => { if (!yes) setNotSent({ text, reason }); })
        .catch(() => { setNotSent({ text, reason }); refreshRef.current(); });
    });
  }, [chatId, tag, accepted]);
  const stop = useCallback(() => {
    // A send still in flight is withdrawn: the server is told which one, so it
    // is refused even if it arrives after this Stop.
    const last = stateRef.current.turns[stateRef.current.turns.length - 1];
    if (last?.local && !last.local.accepted) {
      const { sendId } = last.local;
      withdrawn.current.add(sendId);
      cancelChatTurn(chatId, wireIds.current.get(sendId)).catch(e => setActionError(errText(e)));
      dispatch({ kind: 'send_failed', sendId });
      return;
    }
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
  return { state, actionError, notSent, landed, send, stop, answer };
}

/**
 * ChatFacet is the body of a chat target: the chat panel as a centered column,
 * and, once a tool row is selected, the result panel beside it. With no row
 * selected there is no result panel. The Settings tab shows what the chat has
 * authorized.
 *
 * App keeps one mounted per live chat, so a chat's stream and transcript
 * survive the reader looking at a repository.
 */
export function ChatFacet({ chat, facet, onGone, onNewChat }: {
  chat: ChatInfo;
  /** Start an empty chat with this chat's model (a notice's "New chat"). */
  onNewChat?: (chat: ChatInfo) => void | Promise<void>;
  /** The tab on screen. */
  facet: 'chat' | 'settings';
  /** The chat is over; it leaves the list. */
  onGone: (id: string) => void;
}) {
  const { state, actionError, notSent, landed, send, stop, answer } = useChatSession(chat.id, onGone);
  // A failed "New chat" (from a notice) is shown, not swallowed.
  const [newChatError, setNewChatError] = useState('');
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const onSelect = useCallback((c: ToolCall) => setSelectedId(cur => (cur === c.id ? null : c.id)), []);

  // The model's context window, for the composer's meter and the top bar's
  // pill. A chat whose window cannot be read keeps 0 (no "/ window", no fill).
  const [ctxWindow, setWindow] = useState(0);
  useEffect(() => {
    let cancelled = false;
    getChatContext(chat.id).then(c => { if (!cancelled) setWindow(c.window); }).catch(() => { /* no meter denominator */ });
    return () => { cancelled = true; };
  }, [chat.id]);
  // The top bar sits outside this component: hand it what the pill shows.
  const cost = sessionCost(state.turns);
  useEffect(() => {
    publishChatPill(chat.id, { cost, used: state.ctxTokens, window: ctxWindow });
  }, [chat.id, cost, state.ctxTokens, ctxWindow]);
  useEffect(() => () => forgetChatPill(chat.id), [chat.id]);

  // The chat stays mounted under Settings (hidden), so what was typed and
  // which row was open are still there on the way back.
  return (
    <>
      {facet === 'settings' && <ChatSettings chatId={chat.id} />}
      <div data-testid="chat-facet" style={{ flex: 1, minHeight: 0, display: facet === 'chat' ? 'flex' : 'none', flexDirection: 'column', overflow: 'hidden' }}>
        <div data-testid="chat-columns" className="k-chat-body">
          <div className="k-chat-main">
            <ChatPanel state={state} selectedId={selectedId} actionError={newChatError || actionError} notSent={notSent} landed={landed} modelName={chat.name || chat.model}
              context={{ used: state.ctxTokens, window: ctxWindow }}
              onSend={send} onStop={stop} onSelect={onSelect} onAnswer={answer}
              onNewChat={onNewChat ? () => {
                setNewChatError('');
                Promise.resolve(onNewChat(chat)).catch(() => setNewChatError(NEW_CHAT_ERROR));
              } : undefined} />
          </div>
          {/* Until the side panel (a later change) replaces it, a result opens
              here, and only while a row is selected. */}
          {selectedId && (
            <div data-testid="settings-scroll" className="k-chat-result">
              <ChatResultPanel chatId={chat.id} callId={selectedId} />
            </div>
          )}
        </div>
      </div>
    </>
  );
}
