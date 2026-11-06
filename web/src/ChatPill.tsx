import type { CSSProperties } from 'react';
import { compactCount, formatCost } from './chatState';
import { PILL_SESSION, CONTEXT_LABEL } from './chatCopy';
import { useChatPill } from './chatPillStore';

/** ContextMeter is the thin bar of how much of the context window is used. */
export function ContextMeter({ used, window, width = 56 }: { used: number; window: number; width?: number }) {
  const pct = window > 0 ? Math.min(100, (used / window) * 100) : 0;
  return (
    <span data-testid="context-meter" data-pct={Math.round(pct)} aria-hidden="true"
      style={{ display: 'inline-block', width, height: 3, borderRadius: 2, background: '#262626', overflow: 'hidden', verticalAlign: 'middle' }}>
      <span style={{ display: 'block', height: '100%', width: `${pct}%`, background: '#6fbf92' }} />
    </span>
  );
}

/** ChatPill is the top bar's "session $x | context [meter] 68K": the summed
 *  turn costs and the size of the latest prompt against the window. */
export function ChatPill({ chatId }: { chatId: string }) {
  const d = useChatPill(chatId);
  if (!d) return null;
  return (
    <span data-testid="chat-pill" style={pill}>
      <span data-testid="chat-pill-session">
        <span style={{ color: '#6a6a6a' }}>{PILL_SESSION}</span>
        {' '}<strong style={val}>{d.cost === null ? '–' : formatCost(d.cost)}</strong>
      </span>
      <span aria-hidden="true" style={{ width: 1, alignSelf: 'stretch', background: '#262626' }} />
      <span data-testid="chat-pill-context" style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
        <span style={{ color: '#6a6a6a' }}>{CONTEXT_LABEL}</span>
        <ContextMeter used={d.used} window={d.window} />
        <strong style={val}>{compactCount(d.used)}</strong>
      </span>
    </span>
  );
}

const pill: CSSProperties = {
  display: 'inline-flex', alignItems: 'center', gap: 10, alignSelf: 'center', marginRight: 10, padding: '3px 12px',
  border: '1px solid #262626', borderRadius: 999, fontFamily: 'var(--k-font-mono)', fontSize: 11, color: '#8a8a8a',
  whiteSpace: 'nowrap', flexShrink: 0,
};
const val: CSSProperties = { color: '#d0d0d0', fontWeight: 500 };
