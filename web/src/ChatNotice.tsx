import type { CSSProperties, ReactNode } from 'react';
import type { ChatTurn } from './chatState';
import { noticeCount } from './chatState';
import {
  NOTICE_FULL, NOTICE_NEW_CHAT, noticeFullRest, noticeOf, noticeTierNext,
} from './chatCopy';

// Notices are ONE LINE in the voice of the cost footer, never a filled box:
// [tone icon] [key phrase] · [rest, footer grey] · [action link], 11.5px. The
// full server sentence is the title (hover). Tones: blocked (red) and info
// (blue). There is no amber tone: the KB convention reserves amber for
// failures, and the price-tier line is a trade-off, so it takes the info tone.

export type NoticeTone = 'blocked' | 'info';
const TONE: Record<NoticeTone, string> = { blocked: '#b0736c', info: '#7393b3' };
const FOOT = '#8a8a8a';
const TEXT = '#d4d4d4';

const svgProps = { width: 13, height: 13, viewBox: '0 0 24 24', fill: 'none', stroke: 'currentColor', strokeWidth: 2.2, strokeLinecap: 'round', strokeLinejoin: 'round', 'aria-hidden': true } as const;

function ToneIcon({ tone }: { tone: NoticeTone }) {
  if (tone === 'info') {
    return (
      <svg {...svgProps}><circle cx="12" cy="12" r="9" /><line x1="12" y1="11" x2="12" y2="16" /><line x1="12" y1="8" x2="12.01" y2="8" /></svg>
    );
  }
  return (
    <svg {...svgProps}><circle cx="12" cy="12" r="9" /><line x1="5.6" y1="5.6" x2="18.4" y2="18.4" /></svg>
  );
}

/** One notice line. `lead` is the key phrase (drawn in `leadColor`, the tone
 *  colour by default), `rest` the footer-grey remainder, `action` the link. */
export function NoticeLine({ tone, lead, leadColor, rest, restMono, title, action, testId = 'chat-notice' }: {
  tone: NoticeTone;
  lead: ReactNode;
  leadColor?: string;
  rest?: ReactNode;
  /** The remainder is figures ("1.02M > 1M, nothing sent"): the mockup sets it in the mono face, 10.5px. */
  restMono?: boolean;
  /** The full server sentence, shown on hover. */
  title?: string;
  action?: { label: string; onClick: () => void };
  testId?: string;
}) {
  return (
    <div data-testid={testId} data-tone={tone} title={title} style={line}>
      <span aria-hidden="true" style={{ color: TONE[tone], display: 'flex' }}><ToneIcon tone={tone} /></span>
      <span data-testid={`${testId}-lead`} style={{ color: leadColor ?? TONE[tone] }}>{lead}</span>
      {rest && <><span aria-hidden="true" style={{ color: '#3a3a3a' }}>·</span><span data-testid={`${testId}-rest`} style={restMono ? { color: '#9a9a9a', fontFamily: 'var(--k-font-mono)', fontSize: 10.5 } : { color: FOOT }}>{rest}</span></>}
      {action && (
        <>
          <span aria-hidden="true" style={{ color: '#3a3a3a' }}>·</span>
          <button type="button" data-testid={`${testId}-action`} onClick={action.onClick} style={link}>{action.label}</button>
        </>
      )}
    </div>
  );
}

/** The notices of one turn: its price-tier warning and, if it ended in one, the
 *  context-full block or the turn's error. `modelName` is the model's display
 *  name; `onNewChat` starts an empty chat with the same model, offered on the
 *  context-full line only. The price-tier line is the whole message: no link
 *  and no hover sentence (a fresh chat starts uninitialized, so "New chat"
 *  there helped nobody). */
export function TurnNotices({ turn, modelName, onNewChat }: {
  turn: ChatTurn;
  modelName: string;
  onNewChat?: () => void;
}) {
  const action = onNewChat ? { label: NOTICE_NEW_CHAT, onClick: onNewChat } : undefined;
  const w = turn.warningInfo;
  const e = turn.errorInfo;
  return (
    <>
      {w && w.kind === 'price_tier' && (
        <NoticeLine tone="info" testId="chat-notice-price" leadColor={TEXT}
          lead={noticeOf(noticeCount(w.prompt_tokens ?? 0), noticeCount(w.tier_tokens || w.threshold || 0))}
          rest={noticeTierNext(modelName, w.mult ?? 0)} />
      )}
      {!w && turn.warning && (
        <NoticeLine tone="info" testId="chat-notice-price" leadColor={TEXT} lead={turn.warning} />
      )}
      {turn.error && e && e.kind === 'context_full' && (
        <NoticeLine tone="blocked" testId="chat-notice-full" title={turn.error} action={action}
          lead={NOTICE_FULL} restMono rest={noticeFullRest(e.needed ? noticeCount(e.needed) : '', e.window ? noticeCount(e.window) : '')} />
      )}
      {turn.error && !(e && e.kind === 'context_full') && (
        <NoticeLine tone="blocked" testId="chat-turn-error" title={turn.error} lead={turn.error} />
      )}
    </>
  );
}

const line: CSSProperties = { display: 'flex', alignItems: 'center', gap: 7, margin: '12px 0 2px', fontSize: 11.5, color: FOOT, flexWrap: 'wrap' };
const link: CSSProperties = {
  background: 'transparent', border: 'none', padding: 0, font: 'inherit', fontSize: 11.5, color: '#c8c8c8', cursor: 'pointer',
  textDecoration: 'underline', textDecorationColor: '#4a4a4a', textUnderlineOffset: 3,
};
