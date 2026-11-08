import { useRef } from 'react';
import type { CSSProperties, KeyboardEvent as ReactKeyboardEvent, ReactNode } from 'react';
import { PlusIcon, ChevronLeftIcon } from './icons';
import { ACCENT, SECTION_RAIL_COMPACT_W, SECTION_RAIL_W, TONE_COLOR, dim, mono, type Tone } from './accessUtil';

// The parts the Access page is built from, shaped after Browse: a section
// rail where the target rail stands (same width, ground and selected row), a
// list where the Library stands (same row geometry and selection), and a
// detail pane where the fact view stands (a meta line, a display title, then
// the body). Secrets and Authorizations draw their columns with these, so the
// two sections (and a third, Connected accounts) cannot drift apart.

// ─── Tokens ──────────────────────────────────────────────────────────────────

// ─── Badges ──────────────────────────────────────────────────────────────────


// The rail's chip geometry in the app's state tones: green is fine, amber is
// "attend to this soon", red is "this has lapsed", blue is informational.
const TONES: Record<Tone, CSSProperties> = {
  ok: { color: '#8fd1a5', background: '#15211a', border: '1px solid #26432f' },
  warn: { color: '#e2c07a', background: '#262013', border: '1px solid #4a3f22' },
  bad: { color: '#e8a0a0', background: '#1f1414', border: '1px solid #4a2626' },
  info: { color: '#8ab6d6', background: '#131d26', border: '1px solid #244056' },
  neutral: { color: '#999', background: '#1a1a1a', border: '1px solid #2a2a2a' },
};

export function Badge({ tone, children, title, testid, dot }: {
  tone: Tone; children: ReactNode; title?: string; testid?: string;
  /** A leading status dot (the detail header's larger badge). */
  dot?: boolean;
}) {
  return (
    <span data-testid={testid} data-tone={tone} title={title} style={{ ...badge, ...TONES[tone] }}>
      {dot && <span style={{ width: 6, height: 6, borderRadius: '50%', background: 'currentColor', flexShrink: 0 }} />}
      {children}
    </span>
  );
}
const badge: CSSProperties = {
  display: 'inline-flex', alignItems: 'center', gap: 5,
  fontSize: 10, lineHeight: 1.7, padding: '0 6px', borderRadius: 3,
  fontFamily: 'var(--k-font-mono)', whiteSpace: 'nowrap', flexShrink: 0,
};

/** CountPill: the green count the Access tab carries for waiting requests. */
export function CountPill({ n, tone = 'ok', label }: { n: number; tone?: 'ok' | 'neutral'; label?: string }) {
  return (
    <span aria-label={label} style={{
      fontSize: 10, padding: '0 5px', borderRadius: 8, lineHeight: 1.6, fontVariantNumeric: 'tabular-nums', flexShrink: 0,
      ...(tone === 'ok' ? { background: '#1d2a22', color: '#4ade80' } : { background: '#1c1c1c', color: '#777' }),
    }}>{n}</span>
  );
}

// ─── Section rail ────────────────────────────────────────────────────────────

export interface SectionEntry<K extends string> {
  key: K;
  label: string;
  icon: (p: { color: string; size?: number }) => ReactNode;
  /** A count beside the label (the items in it), dim. */
  count?: number | null;
  /** A green count: things waiting on the reader. Wins over `count`. */
  waiting?: number | null;
  /** A status dot: something in the section needs attention. */
  attention?: Tone | null;
  attentionTitle?: string;
}

/** SectionRail is Access's first column: one row per section, in the target
 *  rail's geometry and selected treatment. `compact` draws glyphs only. */
export function SectionRail<K extends string>({ entries, active, onPick, compact, onRight }: {
  entries: SectionEntry<K>[];
  active: K;
  onPick: (k: K) => void;
  compact?: boolean;
  /** ArrowRight: on to the list. */
  onRight?: () => void;
}) {
  const refs = useRef(new Map<K, HTMLButtonElement>());
  const onKey = (e: ReactKeyboardEvent) => {
    const i = entries.findIndex(x => x.key === active);
    const step = e.key === 'ArrowDown' || e.key === 'j' ? 1 : e.key === 'ArrowUp' || e.key === 'k' ? -1 : 0;
    if (step) {
      e.preventDefault();
      const next = entries[(i + step + entries.length) % entries.length];
      onPick(next.key);
      refs.current.get(next.key)?.focus();
    } else if (e.key === 'ArrowRight' && onRight) {
      e.preventDefault();
      onRight();
    }
  };
  return (
    <nav aria-label="Access sections" data-testid="access-sections"
      style={{ ...railStyle, width: compact ? SECTION_RAIL_COMPACT_W : SECTION_RAIL_W, padding: compact ? '10px 6px' : '10px 10px' }}
      onKeyDown={onKey}>
      {entries.map(s => {
        const on = s.key === active;
        return (
          <button key={s.key} ref={el => { if (el) refs.current.set(s.key, el); else refs.current.delete(s.key); }}
            type="button" data-testid={`access-section-${s.key}`} aria-current={on || undefined}
            tabIndex={on ? 0 : -1}
            title={compact ? s.label : undefined} aria-label={compact ? s.label : undefined}
            onClick={() => onPick(s.key)}
            style={{ ...railRow(on), ...(compact ? { justifyContent: 'center', padding: '9px 0', position: 'relative' } : {}) }}
            onMouseEnter={e => { if (!on) e.currentTarget.style.background = '#161616'; }}
            onMouseLeave={e => { if (!on) e.currentTarget.style.background = 'transparent'; }}>
            <span style={{ display: 'inline-flex', flexShrink: 0 }}>{s.icon({ color: on ? ACCENT : '#8a8a8a', size: compact ? 16 : 14 })}</span>
            {!compact && <span style={railName}>{s.label}</span>}
            {!compact && s.attention && (
              <span title={s.attentionTitle} aria-label={s.attentionTitle}
                style={{ width: 6, height: 6, borderRadius: '50%', background: TONE_COLOR[s.attention], flexShrink: 0 }} />
            )}
            {!compact && (s.waiting ? <CountPill n={s.waiting} label={`${s.waiting} waiting`} />
              : s.count != null ? <span style={railCount}>{s.count}</span> : null)}
            {compact && (s.waiting || s.attention) && (
              <span aria-hidden style={{ position: 'absolute', top: 6, right: 9, width: 6, height: 6, borderRadius: '50%',
                background: s.waiting ? '#4ade80' : TONE_COLOR[s.attention!] }} />
            )}
          </button>
        );
      })}
    </nav>
  );
}

const railStyle: CSSProperties = {
  flexShrink: 0, minHeight: 0, overflowY: 'auto', boxSizing: 'border-box',
  display: 'flex', flexDirection: 'column', gap: 2,
  background: '#0e0e0e', borderRight: '1px solid #222',
};
// The target rail's row and its repo selection (green tint, inset bar).
const railRow = (on: boolean): CSSProperties => ({
  width: '100%', display: 'flex', alignItems: 'center', gap: 10,
  border: 'none', borderRadius: 4, padding: '8px 10px', fontSize: 13, textAlign: 'left',
  fontFamily: 'inherit', cursor: 'pointer',
  ...(on ? { background: '#1a231d', color: '#dfe9e2', boxShadow: `inset 2px 0 0 -0.5px ${ACCENT}` }
    : { background: 'transparent', color: '#bbb' }),
});
const railName: CSSProperties = { flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' };
const railCount: CSSProperties = { fontSize: 11, color: '#5a5a5a', fontVariantNumeric: 'tabular-nums', flexShrink: 0 };

/** SectionStrip: the section rail at a phone width, as underlined tabs. */
export function SectionStrip<K extends string>({ entries, active, onPick }: {
  entries: SectionEntry<K>[]; active: K; onPick: (k: K) => void;
}) {
  return (
    <div role="tablist" aria-label="Access sections" data-testid="access-sections" style={{
      display: 'flex', gap: 2, padding: '0 10px', borderBottom: '1px solid #222', background: '#0e0e0e', flexShrink: 0, overflowX: 'auto',
    }}>
      {entries.map(s => {
        const on = s.key === active;
        return (
          <button key={s.key} type="button" role="tab" aria-selected={on} data-testid={`access-section-${s.key}`}
            onClick={() => onPick(s.key)} style={{
              display: 'inline-flex', alignItems: 'center', gap: 7, background: 'none', border: 'none', borderRadius: 0,
              borderBottom: `2px solid ${on ? ACCENT : 'transparent'}`, color: on ? '#eee' : '#888',
              fontSize: 12.5, padding: '9px 10px 8px', cursor: 'pointer', whiteSpace: 'nowrap', fontFamily: 'inherit',
            }}>
            {s.icon({ color: on ? ACCENT : '#777', size: 13 })}
            {s.label}
            {s.waiting ? <CountPill n={s.waiting} /> : null}
          </button>
        );
      })}
    </div>
  );
}

// ─── List column ─────────────────────────────────────────────────────────────

/** ListHeader: the list's toolbar, as tall as the Library's header — a find
 *  box and, for a section that can create, the rail's `+`. */
export function ListHeader({ query, onQuery, placeholder, add }: {
  query: string;
  onQuery: (q: string) => void;
  placeholder: string;
  add?: { label: string; active: boolean; onClick: () => void; testid: string };
}) {
  return (
    <div style={listHeader}>
      <label style={findBox}>
        <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden="true" style={{ flexShrink: 0 }}>
          <circle cx="11" cy="11" r="7" /><line x1="21" y1="21" x2="16.6" y2="16.6" />
        </svg>
        <input value={query} onChange={e => onQuery(e.target.value)} placeholder={placeholder} aria-label={placeholder}
          onKeyDown={e => { if (e.key === 'Escape') onQuery(''); }}
          style={findInput} />
      </label>
      {add && (
        <button type="button" data-testid={add.testid} title={add.label} aria-label={add.label} onClick={add.onClick}
          style={{ ...plusBtn, ...(add.active ? { background: '#14191f', color: '#8ab6d6' } : {}) }}
          onMouseEnter={e => { if (!add.active) e.currentTarget.style.color = '#ccc'; }}
          onMouseLeave={e => { if (!add.active) e.currentTarget.style.color = '#888'; }}>
          <PlusIcon color="currentColor" size={15} />
        </button>
      )}
    </div>
  );
}
const listHeader: CSSProperties = {
  display: 'flex', alignItems: 'center', gap: 8, height: 48, boxSizing: 'border-box', flexShrink: 0,
  padding: '0 10px 0 12px', borderBottom: '1px solid #1e1e1e',
};
const findBox: CSSProperties = {
  flex: 1, minWidth: 0, height: 28, display: 'flex', alignItems: 'center', gap: 7,
  background: '#161616', border: '1px solid #242424', borderRadius: 5, padding: '0 9px', color: '#555', cursor: 'text',
};
const findInput: CSSProperties = {
  flex: 1, minWidth: 0, background: 'transparent', border: 'none', outline: 'none',
  color: '#ddd', fontSize: 12, fontFamily: 'inherit', padding: 0,
};
const plusBtn: CSSProperties = {
  width: 28, height: 28, flexShrink: 0, display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
  border: '1px solid #2a2a2a', borderRadius: 5, padding: 0, background: '#161616', color: '#888', cursor: 'pointer',
};

/** ListGroup: a heading inside the list (Waiting, Approved), in the target
 *  rail's section-header type. */
export function ListGroup({ label, count, children, testid }: { label: string; count?: number; children: ReactNode; testid?: string }) {
  return (
    <div data-testid={testid} role="group" aria-label={label}>
      <div style={groupHead}>
        <span style={{ flex: 1 }}>{label}</span>
        {count !== undefined && <span style={{ color: '#555', fontVariantNumeric: 'tabular-nums', letterSpacing: 0 }}>{count}</span>}
      </div>
      {children}
    </div>
  );
}
const groupHead: CSSProperties = {
  display: 'flex', alignItems: 'center', gap: 6, padding: '12px 12px 6px',
  fontSize: 10.5, fontWeight: 600, letterSpacing: '0.08em', textTransform: 'uppercase', color: '#777',
};

/** ItemRow: a list row in the Library's geometry and selection — a glyph, a
 *  title over a mono meta line, and a badge at the right. */
export function ItemRow({ id, selected, onSelect, glyph, title, meta, badge, italic, testid, titleMono }: {
  id: string;
  selected: boolean;
  onSelect: () => void;
  glyph: ReactNode;
  title: ReactNode;
  meta?: ReactNode;
  badge?: ReactNode;
  italic?: boolean;
  titleMono?: boolean;
  testid?: string;
}) {
  return (
    <button type="button" data-item={id} data-testid={testid} aria-current={selected || undefined}
      tabIndex={selected ? 0 : -1} onClick={onSelect}
      style={selected ? itemSel : item}
      onMouseEnter={selected ? undefined : e => { e.currentTarget.style.background = '#191922'; }}
      onMouseLeave={selected ? undefined : e => { e.currentTarget.style.background = 'transparent'; }}>
      <span style={{ display: 'inline-flex', flexShrink: 0, alignSelf: meta ? 'flex-start' : 'center', marginTop: meta ? 2 : 0 }}>{glyph}</span>
      <span style={{ display: 'flex', flexDirection: 'column', gap: 2, flex: 1, minWidth: 0 }}>
        <span style={{ ...itemTitle, ...(titleMono ? { ...mono, fontSize: 12.5 } : {}), ...(italic ? { fontStyle: 'italic', color: '#8ab6d6' } : {}) }}>{title}</span>
        {meta && <span style={itemMeta}>{meta}</span>}
      </span>
      {badge && <span style={{ flexShrink: 0, alignSelf: 'center' }}>{badge}</span>}
    </button>
  );
}
const item: CSSProperties = {
  width: '100%', display: 'flex', alignItems: 'center', gap: 10, padding: '8px 12px',
  border: 'none', borderRadius: 0, background: 'transparent', textAlign: 'left', cursor: 'pointer', fontFamily: 'inherit', color: 'inherit',
};
const itemSel: CSSProperties = { ...item, background: '#2a2a3a' };
const itemTitle: CSSProperties = { fontSize: 13, color: '#ddd', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' };
const itemMeta: CSSProperties = {
  ...mono, fontSize: 10.5, color: '#666', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
};

/** ListEmpty: what a list says with nothing in it — a glyph, a line, and the
 *  way to add one. */
export function ListEmpty({ icon, title, children, action, testid }: {
  icon: ReactNode; title: string; children?: ReactNode; action?: ReactNode; testid?: string;
}) {
  return (
    <div data-testid={testid} style={{ padding: '40px 24px', display: 'flex', flexDirection: 'column', alignItems: 'center', textAlign: 'center', gap: 8 }}>
      <span style={{ display: 'inline-flex', width: 44, height: 44, borderRadius: 22, alignItems: 'center', justifyContent: 'center', background: '#171717', border: '1px solid #242424', marginBottom: 4 }}>{icon}</span>
      <div style={{ fontSize: 13, color: '#ccc' }}>{title}</div>
      {children && <div style={{ ...dim, lineHeight: 1.55, maxWidth: 260 }}>{children}</div>}
      {action && <div style={{ marginTop: 8 }}>{action}</div>}
    </div>
  );
}

// ─── Detail column ───────────────────────────────────────────────────────────

/** DetailBack: at a phone width, the way from a detail back to its list. */
export function DetailBack({ label, onClick }: { label: string; onClick: () => void }) {
  return (
    <button type="button" data-testid="access-detail-back" onClick={onClick} style={{
      display: 'inline-flex', alignItems: 'center', gap: 4, background: 'none', border: 'none', borderRadius: 4,
      color: '#8a8a8a', fontSize: 12, padding: '2px 4px 2px 0', marginBottom: 14, cursor: 'pointer', fontFamily: 'inherit',
    }}>
      <ChevronLeftIcon color="currentColor" size={13} /> {label}
    </button>
  );
}

/** DetailHeader: the fact view's band, for an Access item — a meta line
 *  (kind glyph and word, status badge, a dim aside), the title, and a line
 *  under it. `actions` sit at the right of the title. */
export function DetailHeader({ kindIcon, kind, kindColor, status, aside, title, titleMono, sub, actions }: {
  kindIcon: ReactNode;
  kind: string;
  kindColor: string;
  status?: ReactNode;
  aside?: ReactNode;
  title: ReactNode;
  titleMono?: boolean;
  sub?: ReactNode;
  actions?: ReactNode;
}) {
  return (
    <header style={{ marginBottom: 6 }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap', fontSize: 11.5 }}>
        <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6, color: kindColor, ...mono, fontSize: 11 }}>
          {kindIcon}{kind}
        </span>
        {status}
        <span style={{ flex: 1 }} />
        {aside && <span style={{ color: '#666', fontSize: 11 }}>{aside}</span>}
      </div>
      <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginTop: 10 }}>
        <h2 style={{
          margin: 0, flex: 1, minWidth: 0, color: '#eee', lineHeight: 1.25, overflowWrap: 'anywhere',
          ...(titleMono ? { ...mono, fontSize: 19, fontWeight: 500 } : { fontFamily: 'var(--k-font-display)', fontSize: 21, fontWeight: 600, letterSpacing: '-0.01em' }),
        }}>{title}</h2>
        {actions}
      </div>
      {sub && <div style={{ ...dim, fontSize: 12.5, marginTop: 6, lineHeight: 1.5 }}>{sub}</div>}
    </header>
  );
}

/** Block: one titled part of a detail — a small-caps heading over a rule. */
export function Block({ title, aside, children, tone, testid }: {
  title: string; aside?: ReactNode; children: ReactNode; tone?: 'danger'; testid?: string;
}) {
  return (
    <section data-testid={testid} style={{ marginTop: 26 }}>
      <div style={{
        display: 'flex', alignItems: 'baseline', gap: 8, paddingBottom: 7, marginBottom: 4,
        borderBottom: `1px solid ${tone === 'danger' ? '#3a2222' : '#222'}`,
      }}>
        <span style={{ fontSize: 10.5, fontWeight: 600, letterSpacing: '0.1em', textTransform: 'uppercase', color: tone === 'danger' ? '#c98080' : '#7a7a7a' }}>{title}</span>
        <span style={{ flex: 1 }} />
        {aside}
      </div>
      {children}
    </section>
  );
}

/** Prop: one labelled row of a Block. The label column is fixed, so the
 *  values line up down the page; below 520px of pane the label goes above. */
export function Prop({ label, children, action, top, testid }: {
  label: string; children: ReactNode; action?: ReactNode; top?: boolean; testid?: string;
}) {
  return (
    <div data-testid={testid} className="k-access-prop" style={{ ...propRow, alignItems: top ? 'flex-start' : 'center' }}>
      <div className="k-access-prop-label" style={{ ...propLabel, paddingTop: top ? 4 : 0 }}>{label}</div>
      <div style={{ flex: 1, minWidth: 0, color: '#ddd', fontSize: 13, lineHeight: 1.5 }}>{children}</div>
      {action && <div style={{ flexShrink: 0, alignSelf: top ? 'flex-start' : 'center' }}>{action}</div>}
    </div>
  );
}
const propRow: CSSProperties = { display: 'flex', gap: '6px 12px', padding: '9px 0', borderBottom: '1px solid #1a1a1a', flexWrap: 'wrap' };
const propLabel: CSSProperties = { width: 128, flexShrink: 0, color: '#888', fontSize: 12.5 };

/** IconAction: a small labelled action at a Prop's right (Edit, Replace). */
export function IconAction({ icon, label, onClick, disabled, testid, title }: {
  icon?: ReactNode; label: string; onClick: () => void; disabled?: boolean; testid?: string; title?: string;
}) {
  return (
    <button type="button" data-testid={testid} disabled={disabled} onClick={onClick} title={title}
      style={{
        display: 'inline-flex', alignItems: 'center', gap: 5, background: 'none', border: '1px solid transparent', borderRadius: 4,
        color: disabled ? '#444' : '#8a9bb0', fontSize: 12, padding: '3px 7px', cursor: disabled ? 'default' : 'pointer', fontFamily: 'inherit',
      }}
      onMouseEnter={e => { if (!disabled) { e.currentTarget.style.borderColor = '#2a2a2a'; e.currentTarget.style.color = '#cfe0f2'; } }}
      onMouseLeave={e => { e.currentTarget.style.borderColor = 'transparent'; e.currentTarget.style.color = disabled ? '#444' : '#8a9bb0'; }}>
      {icon}{label}
    </button>
  );
}

/** Chip: a mono value (a host, a scope) as a small boxed token. */
export function Chip({ children, onRemove, removeLabel, removeDisabled, removeTitle, tone, testid }: {
  children: ReactNode; onRemove?: () => void; removeLabel?: string; removeDisabled?: boolean; removeTitle?: string;
  tone?: 'add' | 'drop' | 'muted'; testid?: string;
}) {
  const t = tone === 'add' ? { borderColor: '#26432f', color: '#8fd1a5', background: '#121a15' }
    : tone === 'drop' ? { borderColor: '#4a2626', color: '#c98080', background: '#1a1212', textDecoration: 'line-through' }
      : tone === 'muted' ? { color: '#666', borderStyle: 'dashed' as const } : {};
  return (
    <span data-testid={testid} style={{ ...chip, ...t }}>
      {children}
      {onRemove && (
        <button type="button" aria-label={removeLabel} title={removeTitle ?? removeLabel} disabled={removeDisabled} onClick={onRemove}
          style={{ background: 'none', border: 'none', borderRadius: 3, color: removeDisabled ? '#444' : '#888', cursor: removeDisabled ? 'default' : 'pointer', padding: '0 2px', marginLeft: 2, fontSize: 13, lineHeight: 1 }}>×</button>
      )}
    </span>
  );
}
const chip: CSSProperties = {
  display: 'inline-flex', alignItems: 'center', ...mono, fontSize: 11.5, lineHeight: 1.6,
  padding: '1px 7px', borderRadius: 4, background: '#181818', border: '1px solid #2a2a2a', color: '#ccc',
};

/** Stats: the Browse summary's stat row, for an overview. */
export function Stats({ items }: { items: { n: number | string; label: string; tone?: Tone }[] }) {
  return (
    <div style={{ display: 'flex', gap: 28, flexWrap: 'wrap', marginTop: 4 }}>
      {items.map(s => (
        <div key={s.label}>
          <div style={{ fontFamily: 'var(--k-font-display)', fontWeight: 600, fontSize: 19, color: s.tone && s.n !== 0 ? TONE_COLOR[s.tone] : '#e8e8e8', fontVariantNumeric: 'tabular-nums' }}>{s.n}</div>
          <div style={{ fontSize: 10, letterSpacing: '0.1em', textTransform: 'uppercase', color: '#666', marginTop: 2 }}>{s.label}</div>
        </div>
      ))}
    </div>
  );
}

/** Note: a one-line outcome in the detail (saved, approved), or a refusal. */
export function Note({ tone, children, testid, onDismiss }: { tone: 'ok' | 'warn' | 'info'; children: ReactNode; testid?: string; onDismiss?: () => void }) {
  const t = tone === 'ok' ? { background: '#121a15', border: '1px solid #24402c', color: '#9fd5b0' }
    : tone === 'warn' ? { background: '#262013', border: '1px solid #4a3f22', color: '#e2c07a' }
      : { background: '#131a22', border: '1px solid #243a50', color: '#a9c6e0' };
  return (
    <div role={tone === 'warn' ? 'alert' : 'status'} data-testid={testid} style={{
      ...t, borderRadius: 6, padding: '8px 12px', fontSize: 12.5, lineHeight: 1.55, marginTop: 14,
      display: 'flex', gap: 10, alignItems: 'flex-start', whiteSpace: 'pre-wrap', overflowWrap: 'anywhere',
    }}>
      <span style={{ flex: 1 }}>{children}</span>
      {onDismiss && <button type="button" aria-label="Dismiss" onClick={onDismiss} style={{ background: 'none', border: 'none', color: 'inherit', opacity: 0.7, cursor: 'pointer', padding: 0, fontSize: 13, lineHeight: 1.4 }}>✕</button>}
    </div>
  );
}

