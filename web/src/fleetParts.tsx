import { useState, type CSSProperties, type ReactNode } from 'react';
import { CopyIcon } from './icons';
import { wizardErr, wizardErrNote } from './manageStyles';
import { CHIP_TONES, type ChipTone } from './fleetText';
import type { FleetRefusal } from './api';

// The fleet pages' small parts, one rendering each: a chip, a copy button, a
// key/value grid, and a refusal (the server's words, its code beneath, then
// what was kept).

/** Chip: the rail's chip geometry in a fleet tone. */
export function Chip({ text, tone, testid }: { text: string; tone: ChipTone; testid?: string }) {
  return <span data-testid={testid} data-tone={tone} style={{ ...chipBase, ...CHIP_TONES[tone] }}>{text}</span>;
}

/** CopyButton is the app's copy affordance (CopyIcon, green for 1.5 s after
 *  a copy): navigator.clipboard.writeText of exactly `text`. */
export function CopyButton({ text, label, testid }: { text: string; label: string; testid?: string }) {
  const [copied, setCopied] = useState(false);
  const copy = () => {
    navigator.clipboard.writeText(text)
      .then(() => { setCopied(true); setTimeout(() => setCopied(false), 1500); })
      .catch(() => { /* clipboard unavailable: nothing to recover */ });
  };
  return (
    <button type="button" data-testid={testid} title={copied ? 'Copied' : 'Copy'} aria-label={label} style={copyBtn} onClick={copy}>
      <CopyIcon color={copied ? '#7c9' : '#888'} size={13} />
    </button>
  );
}

/** CopyValue: a mono value with its copy button beside it. */
export function CopyValue({ text, label, testid }: { text: string; label: string; testid?: string }) {
  return (
    <span style={copyRow}>
      <span data-testid={testid} style={mono}>{text}</span>
      <CopyButton text={text} label={label} />
    </span>
  );
}

/** Kv: the settings blocks' key/value grid. A row with a null value is left out. */
export function Kv({ rows, testid }: { rows: [string, ReactNode | null][]; testid?: string }) {
  return (
    <div data-testid={testid} style={kvGrid}>
      {rows.filter(([, v]) => v !== null).map(([k, v]) => (
        <span key={k} style={{ display: 'contents' }}>
          <span style={kvKey}>{k}</span>
          <span style={kvVal}>{v}</span>
        </span>
      ))}
    </div>
  );
}

/** Refusal: a refused action under its button. The server's message
 *  verbatim, its code beneath (the repo wizard's error box), then the note
 *  that says what was (not) kept. */
export function Refusal({ err, note, testid = 'fleet-refusal' }: { err: Error | null; note?: string; testid?: string }) {
  if (!err) return null;
  const code = (err as FleetRefusal).code;
  return (
    <div data-testid={testid} role="alert" style={wizardErr}>
      <div>{err.message}</div>
      {code && <div data-testid={`${testid}-code`} style={codeLine}>{code}</div>}
      {note && <div style={wizardErrNote}>{note}</div>}
    </div>
  );
}

const mono: CSSProperties = { fontFamily: 'var(--k-font-mono)', fontSize: 12, overflowWrap: 'anywhere' };


const chipBase: CSSProperties = {
  display: 'inline-flex', alignItems: 'center', fontSize: 10, lineHeight: 1.7, padding: '0 6px',
  borderRadius: 3, fontFamily: 'var(--k-font-mono)', whiteSpace: 'nowrap', flexShrink: 0,
};
const copyBtn: CSSProperties = { background: 'none', border: 'none', padding: 2, cursor: 'pointer', display: 'inline-flex', alignItems: 'center', flexShrink: 0 };
const copyRow: CSSProperties = { display: 'inline-flex', alignItems: 'center', gap: 6, maxWidth: '100%', minWidth: 0 };
const kvGrid: CSSProperties = {
  display: 'grid', gridTemplateColumns: 'minmax(120px, max-content) minmax(0, 1fr)', columnGap: 18, rowGap: 6,
  fontSize: 12, alignItems: 'baseline',
};
const kvKey: CSSProperties = { color: '#888', fontSize: 12 };
const kvVal: CSSProperties = { color: '#ddd', minWidth: 0, overflowWrap: 'anywhere' };
const codeLine: CSSProperties = { fontFamily: 'var(--k-font-mono)', fontSize: 11, color: '#a08750', marginTop: 3 };
