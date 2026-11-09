import { useRef, useState } from 'react';
import type { CSSProperties } from 'react';
import type { ChatInfo } from './chatApi';
import { ProviderIcon } from './icons';
import { noMouseFocus } from './utils';
import { btn, confirmBox } from './manageStyles';
import { formatCost } from './chatState';
import { ChatTitleEdit } from './ChatTitleEdit';
import { MARK_STYLE, STATE_WORDS } from './chatRail';
import type { RailState } from './chatRail';
import {
  COST_UNPRICED, RAIL_END_CHAT, RAIL_END_CONFIRM, RAIL_END_KEEP,
  RAIL_TIP_COST, RAIL_TIP_MODEL, RAIL_TIP_STARTED, railTurns,
} from './chatCopy';

/** The start time as the tooltip words it: the local clock, hours and minutes. */
function startedAt(iso: string | undefined): string {
  if (!iso) return '';
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false });
}

/**
 * One chat in the rail: a single line, the provider's mark (whose colour is the
 * chat's state) and the title with an ellipsis. Hovering shows a tooltip and an
 * x that ends the chat; a chat with a turn running asks first, inline.
 */
export function ChatRailRow({ chat, index, providerKind, state, selected, locked, rowStyle, onPin, onEnd, onRename }: {
  chat: ChatInfo;
  /** The row's place in the list. Rows re-sort live; a moved row's tooltip is
   *  closed (the pointer is no longer over it and no mouseleave fires). */
  index: number;
  providerKind?: string;
  state: RailState;
  selected: boolean;
  locked: boolean;
  /** The rail's row geometry and selected treatment. */
  rowStyle: CSSProperties;
  onPin: () => void;
  onEnd: (id: string) => void;
  /** Rename the chat: a double-click on its name edits it in place. */
  onRename?: (id: string, title: string) => void;
}) {
  const wrap = useRef<HTMLDivElement>(null);
  const [hover, setHover] = useState(false);
  const [tipAt, setTipAt] = useState<{ left: number; top: number } | null>(null);
  const [confirming, setConfirming] = useState(false);
  const [editing, setEditing] = useState(false);
  const [at, setAt] = useState(index);
  if (at !== index) { setAt(index); setHover(false); setTipAt(null); }
  const title = chat.title || chat.name || chat.model;
  const words = STATE_WORDS[state];
  const busy = chat.state === 'running' || chat.state === 'awaiting_approval';

  const enter = () => {
    setHover(true);
    const r = wrap.current?.getBoundingClientRect();
    if (r) setTipAt({ left: r.right + 8, top: r.top });
  };
  const leave = () => { setHover(false); setTipAt(null); };
  const end = () => { if (busy) setConfirming(true); else onEnd(chat.id); };

  return (
    <div ref={wrap} data-testid={`rail-chat-wrap-${chat.id}`} style={{ position: 'relative' }}
      onMouseEnter={enter} onMouseLeave={leave} onMouseMove={() => { if (!hover) enter(); }}>
      {editing ? (
        // The row while its name is edited: the same geometry, an input in place
        // of the name (an input inside the row's button would be invalid).
        <div data-testid={`rail-chat-${chat.id}`} data-chat-state={state} aria-current={selected || undefined} style={{ ...rowStyle, paddingRight: 30 }}>
          <span data-testid={`rail-chat-mark-${chat.id}`} data-state={state} style={{ ...markBox, ...MARK_STYLE[state] }}>
            <ProviderIcon kind={providerKind} name={chat.provider} color="currentColor" size={13} />
          </span>
          <ChatTitleEdit initial={title} testId={`rail-chat-rename-${chat.id}`} onDone={t => {
            setEditing(false);
            if (t !== null) onRename?.(chat.id, t);
          }} />
        </div>
      ) : (
        <button
          type="button"
          data-testid={`rail-chat-${chat.id}`}
          data-chat-state={state}
          aria-current={selected || undefined}
          disabled={locked}
          onMouseDown={noMouseFocus}
          onClick={locked ? undefined : onPin}
          style={{ ...rowStyle, paddingRight: 30 }}
        >
          <span data-testid={`rail-chat-mark-${chat.id}`} data-state={state} style={{ ...markBox, ...MARK_STYLE[state] }}>
            <ProviderIcon kind={providerKind} name={chat.provider} color="currentColor" size={13} />
          </span>
          <span data-testid={`rail-chat-title-${chat.id}`} style={titleStyle}
            onDoubleClick={onRename && !locked ? () => { setHover(false); setTipAt(null); setEditing(true); } : undefined}>{title}</span>
        </button>
      )}
      <button
        type="button"
        data-testid={`rail-chat-end-${chat.id}`}
        aria-label={RAIL_END_CHAT}
        title={RAIL_END_CHAT}
        disabled={locked}
        onMouseDown={noMouseFocus}
        onClick={end}
        style={{ ...endBtn, opacity: hover ? 1 : 0, pointerEvents: hover ? 'auto' : 'none' }}
        onFocus={() => setHover(true)}
        onBlur={() => setHover(false)}
      >
        <svg width="10" height="10" viewBox="0 0 10 10" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" aria-hidden="true">
          <path d="M2 2l6 6M8 2l-6 6" />
        </svg>
      </button>
      {confirming && (
        <div data-testid={`rail-chat-confirm-${chat.id}`} style={{ ...confirmBox, marginTop: 4, padding: 10, fontSize: 12, color: '#ccc' }}>
          <div style={{ marginBottom: 8 }}>{RAIL_END_CONFIRM}</div>
          <div style={{ display: 'flex', gap: 8 }}>
            <button type="button" data-testid={`rail-chat-confirm-end-${chat.id}`} style={btn(false, 'danger')} onClick={() => { setConfirming(false); onEnd(chat.id); }}>{RAIL_END_CHAT}</button>
            <button type="button" data-testid={`rail-chat-confirm-keep-${chat.id}`} style={btn(false)} onClick={() => setConfirming(false)}>{RAIL_END_KEEP}</button>
          </div>
        </div>
      )}
      {hover && tipAt && !editing && (
        <div role="tooltip" data-testid={`rail-chat-tip-${chat.id}`} style={{ ...tip, left: tipAt.left, top: tipAt.top }}>
          <b style={tipTitle}>{chat.title || chat.name || chat.model}</b>
          {/* The state in words, in the normal text colour: amber is for
              failures by the UI copy convention, and the mark carries it. */}
          {words && <div data-testid={`rail-chat-tip-state-${chat.id}`} style={{ margin: '-2px 0 7px', color: '#ccc' }}>{words}</div>}
          <div style={kv}>
            <span>{RAIL_TIP_MODEL}</span><span data-testid={`rail-chat-tip-model-${chat.id}`}>{chat.name || chat.model}</span>
            <span>{RAIL_TIP_STARTED}</span><span data-testid={`rail-chat-tip-started-${chat.id}`}>{[startedAt(chat.started_at), railTurns(chat.turns ?? 0)].filter(Boolean).join(' · ')}</span>
            <span>{RAIL_TIP_COST}</span><span data-testid={`rail-chat-tip-cost-${chat.id}`}>{chat.cost_usd === undefined ? COST_UNPRICED : formatCost(chat.cost_usd)}</span>
          </div>
        </div>
      )}
    </div>
  );
}

const markBox: CSSProperties = {
  width: 18, height: 18, flexShrink: 0, display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
  borderRadius: 5, margin: '-3px 0', position: 'relative',
};
const titleStyle: CSSProperties = {
  flexGrow: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
};
const endBtn: CSSProperties = {
  position: 'absolute', right: 6, top: '50%', transform: 'translateY(-50%)',
  width: 20, height: 20, display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
  background: 'transparent', border: 'none', borderRadius: 4, color: '#9a9a9a', cursor: 'pointer', padding: 0,
};
const tip: CSSProperties = {
  position: 'fixed', width: 230, zIndex: 50, background: '#1b1b1d', border: '1px solid #333', borderRadius: 8,
  boxShadow: '0 14px 40px -10px rgba(0,0,0,.7)', padding: '10px 11px', fontSize: 11.5, color: '#aaa', pointerEvents: 'none',
};
const tipTitle: CSSProperties = { color: '#eee', fontWeight: 600, fontSize: 12, display: 'block', marginBottom: 6, lineHeight: 1.35, overflowWrap: 'anywhere' };
const kv: CSSProperties = { display: 'grid', gridTemplateColumns: 'auto 1fr', gap: '3px 10px' };
