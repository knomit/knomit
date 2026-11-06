import { useState } from 'react';
import type { CSSProperties } from 'react';
import { CopyButton } from './fleetParts';
import { cachedPercent, compactCount, formatCost } from './chatState';
import type { TurnUsage } from './chatState';
import {
  COST_IN, COST_OUT, cachedLabel, COST_ROW_INPUT, COST_ROW_CACHE_READ, COST_ROW_CACHE_WRITE, COST_ROW_OUTPUT,
  COST_THIS_TURN, COST_UNPRICED, costSession,
} from './chatCopy';

/**
 * ChatTurnFooter is the line under a finished answer: copy-answer, the turn's
 * estimated cost, "<in> in · <out> out", and the cache meter with its share
 * (cache_read / (input + cache_read + cache_write)). Hovering (or focusing) it
 * opens a card with the four token counts, this turn's cost and the session's.
 *
 * The cost of each of the four rows is NOT shown: the server sends one summed
 * cost per turn and no prices, so a per-row split would be a guess.
 */
export function ChatTurnFooter({ usage, costUsd, answer, sessionCostUsd, sessionTurns }: {
  usage: TurnUsage;
  costUsd: number | null;
  /** The answer's text, which the copy icon copies. */
  answer: string;
  sessionCostUsd: number | null;
  /** How many turns the session has had. */
  sessionTurns: number;
}) {
  const [hover, setHover] = useState(false);
  const pct = cachedPercent(usage);
  return (
    <div data-testid="chat-footer" style={footer}>
      {answer && <CopyButton text={answer} label="Copy" testid="chat-copy-answer" />}
      <span
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
          <div data-testid="chat-cost-card" role="tooltip" style={cardStyle}>
            <Row label={COST_ROW_INPUT} tokens={usage.input} testid="input" />
            <Row label={COST_ROW_CACHE_READ} tokens={usage.cacheRead} testid="cache-read" />
            <Row label={COST_ROW_CACHE_WRITE} tokens={usage.cacheWrite} testid="cache-write" />
            <Row label={COST_ROW_OUTPUT} tokens={usage.output} testid="output" />
            <div style={{ borderTop: '1px solid #2a2a2a', margin: '8px 0 6px' }} />
            <Total label={COST_THIS_TURN} usd={costUsd} testid="turn" strong />
            <Total label={costSession(sessionTurns)} usd={sessionCostUsd} testid="session" />
          </div>
        )}
      </span>
    </div>
  );
}

function Row({ label, tokens, testid }: { label: string; tokens: number; testid: string }) {
  return (
    <div data-testid={`chat-cost-row-${testid}`} style={{ display: 'flex', justifyContent: 'space-between', gap: 24, padding: '2px 0' }}>
      <span style={{ color: '#7a7a7a' }}>{label}</span>
      <span data-testid={`chat-cost-tokens-${testid}`} style={{ color: '#aaa' }}>{tokens.toLocaleString('en-US')}</span>
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
  position: 'absolute', left: 0, bottom: 'calc(100% + 8px)', zIndex: 5, minWidth: 230, padding: '10px 12px',
  background: '#181818', border: '1px solid #2c2c2c', borderRadius: 8, boxShadow: '0 8px 24px rgba(0,0,0,.5)',
  fontSize: 11, whiteSpace: 'nowrap', color: '#aaa',
};
