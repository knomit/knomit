import { useEffect, useLayoutEffect, useState } from 'react';
import type { CSSProperties, KeyboardEvent as ReactKeyboardEvent, RefObject } from 'react';

// The Access page's non-component parts: layout tokens, hooks, date wording
// and the list keyboard. AccessParts.tsx draws with them.

/** The target rail's width: the section rail stands exactly where it does. */
export const SECTION_RAIL_W = 236;
/** The section rail at a medium width: glyphs only. */
export const SECTION_RAIL_COMPACT_W = 52;
/** The list column's cap: the Library's share would starve the detail. */
export const LIST_MAX_W = 360;
export const LIST_MIN_W = 280;
/** Below this page width the rail collapses to its glyphs. */
export const ACCESS_MEDIUM_PX = 960;
/** Below this page width one column shows at a time. */
export const ACCESS_NARROW_PX = 640;

export const ACCENT = '#7c9';
export const mono: CSSProperties = { fontFamily: 'var(--k-font-mono)' };
export const dim: CSSProperties = { color: '#888', fontSize: 12 };

// ─── Hooks ───────────────────────────────────────────────────────────────────

/** useWidth: the element's own width, measured (0 until measured, as in jsdom). */
export function useWidth(ref: RefObject<HTMLElement | null>): number {
  const [w, setW] = useState(0);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    const measure = () => setW(el.clientWidth);
    measure();
    if (typeof ResizeObserver === 'undefined') return;
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, [ref]);
  return w;
}

/** useNow: Date.now(), re-read every `ms` (countdowns). */
export function useNow(ms: number): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), ms);
    return () => clearInterval(t);
  }, [ms]);
  return now;
}

// ─── Dates ───────────────────────────────────────────────────────────────────

export function fmtDate(iso: string): string {
  return new Date(iso).toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: 'numeric' });
}

/** relDays: "today", "in 10 days", "3 days ago". */
export function relDays(iso: string, now = Date.now()): string {
  const d = Math.round((new Date(iso).getTime() - now) / 86_400_000);
  if (d === 0) return 'today';
  if (d === 1) return 'tomorrow';
  if (d === -1) return 'yesterday';
  return d > 0 ? `in ${d} days` : `${-d} days ago`;
}

/** ago: "just now", "40 min ago", "6 hours ago", "12 days ago". */
export function ago(iso: string, now = Date.now()): string {
  const m = Math.round((now - new Date(iso).getTime()) / 60_000);
  if (m < 1) return 'just now';
  if (m < 60) return `${m} min ago`;
  const h = Math.round(m / 60);
  if (h < 24) return `${h} ${h === 1 ? 'hour' : 'hours'} ago`;
  return relDays(iso, now);
}

/** left: what remains of a short deadline ("8 min left", "40 s left", "expired"). */
export function left(iso: string, now = Date.now()): string {
  const s = Math.round((new Date(iso).getTime() - now) / 1000);
  if (s <= 0) return 'expired';
  if (s < 60) return `${s} s left`;
  return `${Math.ceil(s / 60)} min left`;
}

// ─── Tones ───────────────────────────────────────────────────────────────────

export type Tone = 'ok' | 'warn' | 'bad' | 'info' | 'neutral';
export const TONE_COLOR: Record<Tone, string> = { ok: '#8fd1a5', warn: '#e2c07a', bad: '#e8a0a0', info: '#8ab6d6', neutral: '#888' };

/** listKeys: ↑/↓ (j/k) walk the rows of a list, ← goes back to the section
 *  rail, → and Enter on to the detail. The list owns these keys only while
 *  focus is inside it. */
export function listKeys(ids: string[], selected: string | null, select: (id: string) => void, opts: { onLeft?: () => void; onRight?: () => void }) {
  return (e: ReactKeyboardEvent<HTMLElement>) => {
    const tag = (e.target as HTMLElement).tagName;
    if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    const step = e.key === 'ArrowDown' || e.key === 'j' ? 1 : e.key === 'ArrowUp' || e.key === 'k' ? -1 : 0;
    if (step && ids.length > 0) {
      e.preventDefault();
      const i = selected === null ? -1 : ids.indexOf(selected);
      const next = ids[Math.max(0, Math.min(ids.length - 1, i + step))];
      select(next);
      const listEl = e.currentTarget;
      requestAnimationFrame(() => {
        listEl.querySelector<HTMLElement>(`[data-item="${CSS.escape(next)}"]`)?.focus();
      });
    } else if (e.key === 'ArrowLeft' && opts.onLeft) { e.preventDefault(); opts.onLeft(); }
    else if ((e.key === 'ArrowRight' || e.key === 'Enter') && opts.onRight && selected) { e.preventDefault(); opts.onRight(); }
  };
}

/** The detail column's measure: the fact view's. */
export const detailBody: CSSProperties = { maxWidth: 760, padding: '22px 32px 56px', boxSizing: 'border-box' };
export const detailBodyNarrow: CSSProperties = { padding: '16px 16px 48px', boxSizing: 'border-box' };
