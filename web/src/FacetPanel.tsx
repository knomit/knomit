import { useState, useRef, useEffect } from 'react';
import type { Dispatch, CSSProperties } from 'react';
import type { Action } from './state';
import { typeStyles, defaultTypeStyle } from './utils';

// FacetPanel is the summary view's facet strip: Domains, Entities and Types as
// three ranked columns, each row a one-click filter.
//
// It replaces three stacked label-plus-cloud TagClouds that cost ~298px of a
// ~730px panel and pushed Highlights — the thing the panel exists to show —
// below the fold. Ranked columns cost ~140px and, unlike a wrapped cloud, make
// the counts readable: a bar under each name carries its share of the leader.
//
// The overflow affordance is the reason this is a component and not a style
// change. The three facets do NOT overflow by the same order of magnitude: a
// corpus with 9 types has 51 domains and 714 entities. Expanding a column in
// place is fine for domains and absurd for entities, so both go through the
// same browser — one facet, full width, with a search field doing the
// narrowing. Anything that expands in place would mean `+N more` behaves
// differently depending on which column you click it in.
//
// The whole histogram already ships in the stats payload (no LIMIT server-side
// — internal/store/index.go groups every domain and entity), so the browser
// needs no new endpoint: it is filtering a map the panel already holds.

export type FacetKind = 'domain' | 'entity' | 'type';

// Rows visible per column before the overflow row. Six keeps the block at the
// height of the stat strip plus a little, which is the budget that made the
// change worth making.
const TOP_N = 6;

const FACETS: { kind: FacetKind; label: string }[] = [
  { kind: 'domain', label: 'Domains' },
  { kind: 'entity', label: 'Entities' },
  { kind: 'type',   label: 'Types' },
];

// Domain green and entity blue are the hues these two facets have always had,
// here and on the fact body's own tag clouds. Types deliberately has NO hue of
// its own: every type already owns a colour in `typeStyles`, and a Types row
// wears it, so a type reads the same in this column, on a Library row and on a
// highlight glyph.
const KIND_COLOR: Record<Exclude<FacetKind, 'type'>, string> = {
  domain: '119,204,153',
  entity: '136,170,255',
};

function rowColor(kind: FacetKind, name: string): string {
  if (kind !== 'type') return `rgba(${KIND_COLOR[kind]},0.45)`;
  return (typeStyles[name] || defaultTypeStyle).color;
}

function headingColor(kind: FacetKind): string {
  return kind === 'type' ? '#8a93a3' : `rgba(${KIND_COLOR[kind]},0.85)`;
}

// Count descending, then name ascending. The name tie-break matters: JSON
// object order decides ties otherwise, and two renders of the same corpus would
// be free to disagree about which of two equal-count domains leads.
function ranked(hist: Record<string, number>): [string, number][] {
  return Object.entries(hist).sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
}

const labelStyle: CSSProperties = {
  fontSize: 10, textTransform: 'uppercase', letterSpacing: 1.5, color: '#555f6d',
};

// FacetRow is one ranked value: name, a hairline share bar, and its count.
// Carries the tag-item/data-value pair the TagCloud rows used, so every caller
// that reaches for a facet by value — tests included — still finds one.
function FacetRow({ name, count, max, kind, onPick }: {
  name: string; count: number; max: number; kind: FacetKind; onPick: (v: string) => void;
}) {
  const color = rowColor(kind, name);
  return (
    <div data-testid="tag-item" data-value={name} data-facet={kind}
      onClick={() => onPick(name)}
      title={`Filter by ${name} — ${count} fact${count === 1 ? '' : 's'}`}
      style={{
        display: 'grid', gridTemplateColumns: '1fr auto', alignItems: 'center',
        gap: 8, cursor: 'pointer', padding: '1.5px 0',
      }}
      onMouseEnter={e => { (e.currentTarget.firstChild as HTMLElement).style.color = '#fff'; }}
      onMouseLeave={e => { (e.currentTarget.firstChild as HTMLElement).style.color = '#b9c1cd'; }}
    >
      <span style={{
        position: 'relative', fontSize: 11.5, color: '#b9c1cd',
        overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
        // Clearance under the descenders: flush against the baseline the bar
        // reads as an underline on every row rather than as a measure.
        display: 'block', paddingBottom: 4,
        transition: 'color 0.12s',
      }}>
        {name}
        <span aria-hidden style={{
          position: 'absolute', left: 0, bottom: 0, height: 1.5, borderRadius: 1,
          width: `${Math.max(6, max > 0 ? (count / max) * 100 : 100)}%`,
          background: color, opacity: 0.6,
        }} />
      </span>
      <span style={{
        fontFamily: 'var(--k-font-mono)', fontSize: 10, color: '#6d7788',
        fontVariantNumeric: 'tabular-nums',
      }}>{count}</span>
    </div>
  );
}

// FacetBrowser is the overflow view: ONE facet, full width, every value, with
// search doing the narrowing.
//
// A pick CLOSES it, and that is deliberate: the chip flips a path-sorted view
// to Recent, the list refetches and its first row opens, which is what a facet
// pick means everywhere else in the app — the columns below do the same, and so
// does the repo summary. So this is the way to REACH one value among hundreds,
// not a mode the reader stays in. Adding a second chip means coming back.
function FacetBrowser({ kind, label, entries, onPick, onClose }: {
  kind: FacetKind; label: string; entries: [string, number][];
  onPick: (v: string) => void; onClose: () => void;
}) {
  const [q, setQ] = useState('');
  const input = useRef<HTMLInputElement>(null);
  useEffect(() => { input.current?.focus(); }, []);

  const needle = q.trim().toLowerCase();
  const hits = needle ? entries.filter(([n]) => n.toLowerCase().includes(needle)) : entries;
  const max = entries.length > 0 ? entries[0][1] : 0;

  return (
    <div data-testid="facet-browser" data-facet={kind}
      onKeyDown={e => { if (e.key === 'Escape') { e.stopPropagation(); onClose(); } }}
      style={{ marginTop: 4 }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 9, marginBottom: 9 }}>
        <button data-testid="facet-back" onClick={onClose}
          style={{
            background: 'none', border: 0, padding: '2px 6px 2px 0', cursor: 'pointer',
            font: 'inherit', fontSize: 11, color: '#7a8593',
          }}>← All facets</button>
        <div style={{ ...labelStyle, color: headingColor(kind) }}>{label} · {entries.length}</div>
        <input data-testid="facet-search" ref={input} type="text" value={q}
          onChange={e => setQ(e.target.value)}
          placeholder={`Search ${label.toLowerCase()}…`}
          aria-label={`Search ${label.toLowerCase()}`}
          style={{
            marginLeft: 'auto', width: 190, padding: '3px 9px', font: 'inherit', fontSize: 11,
            background: '#14171f', border: '1px solid #262c35', borderRadius: 4,
            color: '#cfd6e2', outline: 'none',
          }} />
      </div>
      {/* FIXED height, not max-height, and sized to leave the seventh row half
          visible: it matches the columns view within a few pixels so opening
          the browser doesn't shove Highlights up the panel, it doesn't bounce
          as a search narrows the hits, and the clipped row is the only honest
          signal that the list scrolls. */}
      <div style={{
        display: 'grid', gridTemplateColumns: 'repeat(3, minmax(0, 1fr))', gap: '0 22px',
        height: 154, overflowY: 'auto', alignContent: 'start',
      }}>
        {hits.length === 0
          ? <div data-testid="facet-no-match" style={{ fontSize: 11, color: '#5a6675', padding: '6px 0' }}>
              No {label.toLowerCase()} match “{q.trim()}”
            </div>
          : hits.map(([n, c]) => (
              <FacetRow key={n} name={n} count={c} max={max} kind={kind} onPick={onPick} />
            ))}
      </div>
    </div>
  );
}

// FacetPanel is shared by the repo summary and the lens union header, so the
// two views cannot drift — the same rule the TagCloud pair it replaces followed.
export function FacetPanel({ domains, entities, types, dispatch }: {
  domains: Record<string, number>;
  entities: Record<string, number>;
  types: Record<string, number>;
  dispatch: Dispatch<Action>;
}) {
  const [open, setOpen] = useState<FacetKind | null>(null);

  const hists: Record<FacetKind, Record<string, number>> = {
    domain: domains, entity: entities, type: types,
  };
  const present = FACETS
    .map(f => ({ ...f, entries: ranked(hists[f.kind]) }))
    .filter(f => f.entries.length > 0);

  if (present.length === 0) return null;

  const pick = (kind: FacetKind, value: string) =>
    dispatch({ type: 'ADD_FILTER', chip: { category: kind, value } });

  const opened = present.find(f => f.kind === open);
  // A facet that emptied out from under an open browser (a refetch on a
  // narrower path) falls back to the columns rather than rendering a dead pane.
  if (opened) {
    return (
      <div data-testid="facet-panel" style={{ marginTop: 4 }}>
        <FacetBrowser kind={opened.kind} label={opened.label} entries={opened.entries}
          onPick={v => pick(opened.kind, v)} onClose={() => setOpen(null)} />
      </div>
    );
  }

  return (
    <div data-testid="facet-panel" style={{
      marginTop: 4,
      display: 'grid', gridTemplateColumns: `repeat(${present.length}, minmax(0, 1fr))`, gap: 22,
    }}>
      {present.map(f => {
        const max = f.entries[0][1];
        const rest = f.entries.length - TOP_N;
        return (
          <div key={f.kind} data-testid="facet-column" data-facet={f.kind}>
            <div style={{ ...labelStyle, color: headingColor(f.kind), marginBottom: 8 }}>
              {f.label} · {f.entries.length}
            </div>
            {f.entries.slice(0, TOP_N).map(([n, c]) => (
              <FacetRow key={n} name={n} count={c} max={max} kind={f.kind}
                onPick={v => pick(f.kind, v)} />
            ))}
            {/* The overflow row keeps a column's height fixed whether or not it
                overflows, so the three columns stay aligned and Highlights sits
                at the same offset in every folder. */}
            <div data-testid={rest > 0 ? 'facet-more' : 'facet-more-spacer'} data-facet={f.kind}
              onClick={rest > 0 ? () => setOpen(f.kind) : undefined}
              style={{
                fontSize: 10.5, color: '#5f6a7c', padding: '1.5px 0',
                cursor: rest > 0 ? 'pointer' : 'default', minHeight: 17,
              }}
              onMouseEnter={e => { if (rest > 0) e.currentTarget.style.color = '#9fb0c8'; }}
              onMouseLeave={e => { e.currentTarget.style.color = '#5f6a7c'; }}
            >{rest > 0 ? `+${rest} more` : ''}</div>
          </div>
        );
      })}
    </div>
  );
}
