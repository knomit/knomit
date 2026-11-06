import { useEffect, useRef, useState } from 'react';
import type { CSSProperties } from 'react';
import { KnomitMark } from './ChatToolGroup';
import {
  CHAT_CONTINUE, CHAT_EDIT_RESEND, CHAT_SENDING, CHAT_STOPPED, CHAT_THINKING, CHAT_NO_RESPONSE, waitCounter,
} from './chatCopy';

/** "Sending…" under the dimmed user strip, with three dots, aligned with the strip's text. */
export function SendingLine() {
  return (
    <div data-testid="chat-sending" style={sending}>
      <span aria-hidden="true" style={{ display: 'inline-flex', gap: 3 }}>
        {[0.9, 0.55, 0.25].map((o, i) => <i key={i} style={{ width: 4, height: 4, borderRadius: '50%', background: '#e8c27a', opacity: o, display: 'block' }} />)}
      </span>
      {CHAT_SENDING}
    </div>
  );
}

/** The thinking placeholder: where the activity line will go until the first
 *  tool call or text arrives. The counter runs from the server's acceptance
 *  (`since`; the moment the page first saw the turn when it joined mid-way). It
 *  turns grey and reads "No response yet" once the elapsed time passes
 *  `expectedMs`, the slowest first reply this model has taken in this server
 *  run; with none observed (0) it never does. `thinking` is the reasoning
 *  summary so far, streamed under it. */
export function ThinkingPlaceholder({ since, expectedMs, thinking }: { since?: number; expectedMs: number; thinking: string }) {
  const [joined] = useState(() => Date.now());
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, []);
  const start = since ?? joined;
  const elapsed = Math.max(0, now - start);
  const stalled = expectedMs > 0 && elapsed > expectedMs;
  return (
    <div data-testid="chat-thinking" data-stalled={stalled || undefined} style={{ margin: '8px 0' }}>
      <div style={{ ...think, color: stalled ? '#888' : '#b8a27a' }}>
        <span style={{ ...tile, ...(stalled ? { background: '#1c1c1c', boxShadow: 'none', filter: 'grayscale(1)', opacity: 0.7 } : {}) }}>
          <KnomitMark size={11} />
        </span>
        <span data-testid="chat-thinking-label">{stalled ? CHAT_NO_RESPONSE : CHAT_THINKING}</span>
        <span data-testid="chat-thinking-counter" style={{ fontFamily: 'var(--k-font-mono)', fontSize: 10.5, color: stalled ? '#666' : '#7a6c50' }}>
          {waitCounter(Math.floor(elapsed / 1000))}
        </span>
      </div>
      {thinking && <ThinkingSummary text={thinking} />}
    </div>
  );
}

/** The reasoning summary streamed so far: capped at four lines, scrolling
 *  inside, and following the newest line as text arrives. */
function ThinkingSummary({ text }: { text: string }) {
  const box = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const el = box.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [text]);
  return <div ref={box} data-testid="chat-thinking-summary" style={summary}>{text}</div>;
}

/** The neutral divider after a stop, and, on the latest turn, the two ways on. */
export function StoppedMark({ latest, onEdit, onContinue }: { latest: boolean; onEdit: () => void; onContinue: () => void }) {
  return (
    <div data-testid="chat-stopped">
      <div style={divider}>
        <span aria-hidden="true" style={{ flex: 1, height: 1, background: '#242424' }} />
        <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
          <svg aria-hidden="true" width="10" height="10" viewBox="0 0 10 10"><rect width="10" height="10" rx="2" fill="#777" /></svg>
          {CHAT_STOPPED}
        </span>
        <span aria-hidden="true" style={{ flex: 1, height: 1, background: '#242424' }} />
      </div>
      {latest && (
        <div style={{ display: 'flex', gap: 8, marginTop: 10 }}>
          <button type="button" data-testid="chat-edit-resend" onClick={onEdit} style={ghostBtn}>{CHAT_EDIT_RESEND}</button>
          <button type="button" data-testid="chat-continue" onClick={onContinue} style={ghostBtn}>{CHAT_CONTINUE}</button>
        </div>
      )}
    </div>
  );
}

const sending: CSSProperties = { display: 'flex', alignItems: 'center', gap: 10, padding: '0 0 0 14px', margin: '-4px 0 8px', fontSize: 11, color: '#777' };
const think: CSSProperties = { display: 'flex', alignItems: 'center', gap: 8, fontSize: 12, padding: '5px 9px 5px 7px', width: 'max-content' };
const tile: CSSProperties = {
  width: 16, height: 16, borderRadius: 4, background: '#241f14', display: 'flex', alignItems: 'center', justifyContent: 'center',
  boxShadow: '0 0 0 3px rgba(232,194,122,.10), 0 0 10px rgba(232,194,122,.25)',
};
const summary: CSSProperties = {
  margin: '0 0 10px 31px', fontSize: 12.5, lineHeight: 1.55, color: '#7c7c7c', fontStyle: 'italic',
  borderLeft: '1px solid #262626', paddingLeft: 10, maxWidth: 560, whiteSpace: 'pre-wrap',
  // Four lines (12.5px at 1.55), then it scrolls inside.
  maxHeight: 12.5 * 1.55 * 4, overflowY: 'auto',
};
const divider: CSSProperties = { display: 'flex', alignItems: 'center', gap: 8, marginTop: 10, fontSize: 11.5, color: '#777' };
const ghostBtn: CSSProperties = {
  background: 'transparent', color: '#c8c8c8', border: '1px solid #2e2e2e', borderRadius: 5, padding: '4px 10px',
  fontSize: 12, cursor: 'pointer', fontFamily: 'inherit',
};
