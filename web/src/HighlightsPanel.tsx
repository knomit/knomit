import type { Highlight, RankAxis } from './api';
import { TypeIcon } from './icons';
import { typeStyles, defaultTypeStyle } from './utils';

// typeLabel is the row tooltip: the fact's type plus the impact count the row
// no longer prints. Impact still ranks the list, so keeping it reachable on
// hover means the ordering can still be checked against the fact's connections.
function typeLabel(h: Highlight): string {
  const label = (typeStyles[h.type] || defaultTypeStyle).label;
  return `${label} — derived from ${h.impact} fact${h.impact === 1 ? '' : 's'}`;
}

const AXES: { key: RankAxis; label: string }[] = [
  { key: 'impact', label: 'Impact' },
  { key: 'confidence', label: 'Confidence' },
  { key: 'recent', label: 'Recent' },
];

// HighlightsPanel renders the top-N and its axis control, and nothing else.
//
// The type census used to render here as a row of pills, because the pills and
// the rows both come off `stats.types`/`stats.highlights`. It now renders in
// FacetPanel beside Domains and Entities, where it always belonged: it is a
// facet filter over the whole folder, not part of the ranked list. That move
// also settles a coupling this component had to work around — the pills list
// EVERY type including observation and reference, which never appear as ROWS
// (they are the substrate the distilled layer is built from, and on core they
// would bury it 1150-to-180), so a folder holding only observations needed its
// pills to survive an empty highlights list. Nothing here is load-bearing for
// that any more: FacetPanel renders the census unconditionally.
//
// The list is presentational: it renders `highlights` in the order received
// and never re-sorts. Ranking happens server-side over the full eligible set,
// because the top-N under one axis is NOT the top-N under another — re-sorting
// an already-truncated list would silently answer a different question.
export function HighlightsPanel({ highlights, axis, onAxisChange, onOpen }: {
  highlights: Highlight[];
  axis: RankAxis;
  onAxisChange: (a: RankAxis) => void;
  onOpen: (path: string) => void;
}) {
  const rows = highlights;

  // Nothing to rank.
  if (rows.length === 0) return null;

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
