import { useLayoutEffect, useRef, useState } from 'react';
import type { CSSProperties } from 'react';
import { CopyButton } from './fleetParts';
import { CARD_GAP as GAP, cardSide } from './chatCardSide';
import { cachedPercent, compactCount, formatCost } from './chatState';
import type { CostRows, TurnUsage } from './chatState';
import {
  COST_IN, COST_OUT, cachedLabel, COST_ROW_INPUT, COST_ROW_CACHE_READ, COST_ROW_CACHE_WRITE, COST_ROW_OUTPUT,
  COST_THIS_TURN, COST_UNPRICED, costCardHeader, costSession,
} from './chatCopy';

/**
 * ChatTurnFooter is the line under a finished answer: copy-answer, the turn's
 * estimated cost, "<in> in · <out> out", and the cache meter with its share
 * (cache_read / (input + cache_read + cache_write)). Hovering (or focusing) it
 * opens a card with the four token counts, this turn's cost and the session's.
 *
 * The card opens with "Turn N · <model>" and each of the four rows shows its own
 * cost. The server computes the rows (each request at its own price tier) and they
 * add up to the turn's cost; the page does no pricing of its own.
 */
export function ChatTurnFooter({ usage, costUsd, costRows, turnNumber, modelName, answer, sessionCostUsd, sessionTurns }: {
  usage: TurnUsage;
  costUsd: number | null;
  /** The cost by token kind; null without a price card. */
  costRows: CostRows | null;
  /** 1-based number of this turn in the chat. */
  turnNumber: number;
  /** The model's display name (its configured label, else its id). */
  modelName: string;
  /** The answer's text, which the copy icon copies. */
  answer: string;
  sessionCostUsd: number | null;
  /** How many turns the session has had. */
  sessionTurns: number;
}) {
  const [hover, setHover] = useState(false);
  const [side, setSide] = useState<'above' | 'below'>('above');
  const usageEl = useRef<HTMLSpanElement>(null);
  const card = useRef<HTMLDivElement>(null);
  // The card opens above the footer (the mockup's direction, and clear of the
  // composer on the latest turn) when the scrolling transcript has room for it
  // there, and below otherwise: a card taller than the room above would have its
  // first rows cut off by the transcript's top edge.
  useLayoutEffect(() => {
    if (!hover || !usageEl.current || !card.current) return;
    setSide(cardSide(usageEl.current.getBoundingClientRect().top - scrollTop(usageEl.current), card.current.getBoundingClientRect().height));
  }, [hover]);
  const pct = cachedPercent(usage);
  return (
    <div data-testid="chat-footer" style={footer}>
      {answer && <CopyButton text={answer} label="Copy" testid="chat-copy-answer" />}
      <span
        ref={usageEl}
        data-testid="chat-usage" tabIndex={0} style={{ display: 'inline-flex', alignItems: 'center', gap: 10, position: 'relative', cursor: 'default' }}
        onMouseEnter={() => setHover(true)} onMouseLeave={() => setHover(false)}
        onFocus={() => setHover(true)} onBlur={() => setHover(false)}
      >
        <span data-testid="chat-cost" style={{ color: '#8a8a8a' }}>{costUsd === null ? COST_UNPRICED : formatCost(costUsd)}</span>
        <span data-testid="chat-tokens">{compactCount(usage.input)} {COST_IN} · {compactCount(usage.output)} {COST_OUT}</span>
        <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
          <span aria-hidden="true" style={{ display: 'inline-block', width: 28, height: 3, borderRadius: 2, background: '#262626', overflow: 'hidden' }}>
            <span data-testid="chat-cache-bar" style={{ display: 'block', height: '100%', width: `${pct}%`, background: '#6fbf92' }} />
          </span>
          <span data-testid="chat-cached">{cachedLabel(pct)}</span>
        </span>
        {hover && (
          <div ref={card} data-testid="chat-cost-card" data-side={side} role="tooltip" style={side === 'above' ? cardStyle : { ...cardStyle, bottom: 'auto', top: 'calc(100% + 8px)' }}>
            <div data-testid="chat-cost-header" style={{ fontFamily: 'var(--k-font-body)', fontSize: 11, color: '#888', marginBottom: 8 }}>
              {costCardHeader(turnNumber, modelName)}
            </div>
            <div style={{ display: 'grid', gridTemplateColumns: '1fr auto auto', gap: '5px 14px' }}>
              <Row label={COST_ROW_INPUT} tokens={usage.input} usd={costRows?.input} testid="input" />
              <Row label={COST_ROW_CACHE_READ} tokens={usage.cacheRead} usd={costRows?.cacheRead} testid="cache-read" />
              <Row label={COST_ROW_CACHE_WRITE} tokens={usage.cacheWrite} usd={costRows?.cacheWrite} testid="cache-write" />
              <Row label={COST_ROW_OUTPUT} tokens={usage.output} usd={costRows?.output} testid="output" />
            </div>
            <div style={{ borderTop: '1px solid #2a2a2a', margin: '8px 0 6px' }} />
            <Total label={COST_THIS_TURN} usd={costUsd} testid="turn" strong />
            <Total label={costSession(sessionTurns)} usd={sessionCostUsd} testid="session" />
          </div>
        )}
      </span>
    </div>
  );
}

/** The top edge of the nearest scrolling ancestor, or of the viewport. */
function scrollTop(el: HTMLElement): number {
  for (let n = el.parentElement; n; n = n.parentElement) {
    const o = getComputedStyle(n).overflowY;
    if (o === 'auto' || o === 'scroll') return n.getBoundingClientRect().top;
  }
  return 0;
}

const rowNum: CSSProperties = { color: '#c8c8c8', fontFamily: 'var(--k-font-mono)', fontSize: 10.5, textAlign: 'right' };

/** One row of the card: label, token count, and (with a price card) its cost. A row
 *  without a price still takes its grid cell, so the columns stay aligned. */
function Row({ label, tokens, usd, testid }: { label: string; tokens: number; usd: number | undefined; testid: string }) {
  return (
    <div data-testid={`chat-cost-row-${testid}`} style={{ display: 'contents' }}>
      <span style={{ color: '#6a6a6a', fontFamily: 'var(--k-font-body)', fontSize: 11 }}>{label}</span>
      <span data-testid={`chat-cost-tokens-${testid}`} style={rowNum}>{tokens.toLocaleString('en-US')}</span>
      {usd === undefined ? <span /> : <span data-testid={`chat-cost-usd-${testid}`} style={rowNum}>{formatCost(usd)}</span>}
    </div>
  );
}

function Total({ label, usd, testid, strong }: { label: string; usd: number | null; testid: string; strong?: boolean }) {
  return (
    <div style={{ display: 'flex', justifyContent: 'space-between', gap: 24, padding: '2px 0', color: strong ? '#e0e0e0' : '#8a8a8a' }}>
      <span>{label}</span>
      <span data-testid={`chat-cost-${testid}`}>{usd === null ? COST_UNPRICED : formatCost(usd)}</span>
    </div>
  );
}

const footer: CSSProperties = {
  display: 'flex', alignItems: 'center', gap: 10, marginTop: 12, fontSize: 10.5, fontFamily: 'var(--k-font-mono)', color: '#555',
};
const cardStyle: CSSProperties = {
  position: 'absolute', left: 0, bottom: `calc(100% + ${GAP}px)`, zIndex: 5, minWidth: 230, padding: '10px 12px',
  background: '#181818', border: '1px solid #2c2c2c', borderRadius: 8, boxShadow: '0 8px 24px rgba(0,0,0,.5)',
  fontSize: 11, whiteSpace: 'nowrap', color: '#aaa',
};
