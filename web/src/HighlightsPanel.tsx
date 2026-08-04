import type { Dispatch } from 'react';
import type { Highlight, RankAxis } from './api';
import type { Action } from './state';
import { TagCloud } from './FactBody';
import { TypeIcon } from './icons';
import { typeStyles, defaultTypeStyle } from './utils';

// typeLabel is the row tooltip: the fact's type plus the impact count the row
// no longer prints. Impact still ranks the list, so keeping it reachable on
// hover means the ordering can still be checked against the fact's connections.
function typeLabel(h: Highlight): string {
  const label = (typeStyles[h.type] || defaultTypeStyle).label;
  return `${label} — derived from ${h.impact} fact${h.impact === 1 ? '' : 's'}`;
}

// Types that never appear in highlights, mirroring the server. Kept in sync by
// the server's own exclusion — this is belt-and-braces for the type pills,
// which are built from the full type census rather than from the rows.
const EXCLUDED_TYPES = new Set(['observation', 'reference']);

const AXES: { key: RankAxis; label: string }[] = [
  { key: 'impact', label: 'Impact' },
  { key: 'confidence', label: 'Confidence' },
  { key: 'recent', label: 'Recent' },
];

// HighlightsPanel renders the overview's top-N list plus its verbs: a type
// pill adds a filter chip, a title opens the fact, an axis button asks the
// owner to refetch.
//
// Presentational by design — it renders `highlights` in the order received and
// never re-sorts. Ranking happens server-side over the full eligible set,
// because the top-10 under one axis is NOT the top-10 under another: sorting a
// truncated list client-side would show "the 10 most load-bearing facts, in
// date order" under a caption promising the most recent ones.
export function HighlightsPanel({ highlights, types, axis, onAxisChange, onOpen, dispatch }: {
  highlights: Highlight[];
  types: Record<string, number>;
  axis: RankAxis;
  onAxisChange: (a: RankAxis) => void;
  onOpen: (path: string) => void;
  dispatch: Dispatch<Action>;
}) {
  if (highlights.length === 0) return null;

  const rows = highlights;
  const typeEntries = Object.entries(types)
    .filter(([t]) => !EXCLUDED_TYPES.has(t))
    .sort((a, b) => b[1] - a[1]);

  return (
    <div style={{ marginTop: 22 }}>
      <div style={{ display: 'flex', alignItems: 'baseline', marginBottom: 6 }}>
        <div style={{ fontSize: 10, textTransform: 'uppercase', letterSpacing: 1.5, color: '#555' }}>
          Highlights
        </div>
        <div style={{ marginLeft: 'auto', display: 'flex', border: '1px solid #262c35', borderRadius: 4, overflow: 'hidden' }}>
          {AXES.map(a => (
            <button key={a.key} onClick={() => onAxisChange(a.key)}
              style={{
                padding: '2px 9px', fontSize: 10, cursor: 'pointer', border: 0,
                // Selection reads through background and colour; the browser's
                // default focus ring drew a white pill over the group's own
                // border. Matches the sort-axis buttons in LibraryHeader.
                outline: 'none',
                borderRight: '1px solid #262c35',
                background: axis === a.key ? '#1a2a3a' : 'transparent',
                color: axis === a.key ? '#cfe4f5' : '#7a8593',
              }}>{a.label}</button>
          ))}
        </div>
      </div>

      {/* No caption and no "Types" heading: the rows and pills behave exactly
          like the domain and entity tiles above, so explaining them again was
          noise. The axis control already names the ordering. */}
      <TagCloud label="" entries={typeEntries} color="136,170,255"
        onTagClick={t => dispatch({ type: 'ADD_FILTER', chip: { category: 'type', value: t } })} />

      <div>
        {rows.map(h => (
          <div key={h.path} data-testid="highlight-row"
            style={{ display: 'flex', gap: 9, alignItems: 'center', padding: '5px 0', borderBottom: '1px solid #16191e' }}>
            {/* Type glyph in the type's own colour — the same TypeIcon +
                typeStyles pairing Library rows use, so a fact reads the same
                wherever it appears. The impact count is no longer shown; it
                survives as the row tooltip, since the ranking is otherwise
                unexplained. */}
            <span data-testid="highlight-type-icon" title={typeLabel(h)}
              style={{ flex: 'none', display: 'flex', alignItems: 'center' }}>
              <TypeIcon type={h.type} color={(typeStyles[h.type] || defaultTypeStyle).color} size={12} />
            </span>
            <span onClick={() => onOpen(h.path)} title={typeLabel(h)}
              style={{ flex: 1, fontSize: 11.5, lineHeight: 1.35, color: '#c9ced6', cursor: 'pointer' }}>
              {h.title}
            </span>
            <span style={{ flex: 'none', fontSize: 9.5, color: '#4a5464' }}>
              {h.confidence.toFixed(2)}
            </span>
          </div>
        ))}
      </div>
    </div>
  );
}
