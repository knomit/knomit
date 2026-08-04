import type { Dispatch } from 'react';
import type { Highlight, RankAxis } from './api';
import type { Action } from './state';
import { TagCloud } from './FactBody';

// Types that never appear in highlights, mirroring the server. Kept in sync by
// the server's own exclusion — this is belt-and-braces for the type pills,
// which are built from the full type census rather than from the rows.
const EXCLUDED_TYPES = new Set(['observation', 'reference']);

const CAPTIONS: Record<RankAxis, string> = {
  impact: 'The facts the most others were built on. The number is how many facts each was derived from.',
  confidence: 'The strongest facts here, ranked by confidence.',
  recent: 'The most recently committed facts here.',
};

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
                borderRight: '1px solid #262c35',
                background: axis === a.key ? '#1a2a3a' : 'transparent',
                color: axis === a.key ? '#cfe4f5' : '#7a8593',
              }}>{a.label}</button>
          ))}
        </div>
      </div>

      <div data-testid="highlights-caption" style={{ fontSize: 10.5, color: '#5a6675', lineHeight: 1.5, marginBottom: 9 }}>
        {CAPTIONS[axis]} Click a type to filter, a title to open it.
      </div>

      <TagCloud label="Types" entries={typeEntries} color="136,170,255"
        onTagClick={t => dispatch({ type: 'ADD_FILTER', chip: { category: 'type', value: t } })} />

      <div>
        {rows.map(h => (
          <div key={h.path} data-testid="highlight-row"
            style={{ display: 'flex', gap: 9, alignItems: 'baseline', padding: '5px 0', borderBottom: '1px solid #16191e' }}>
            {axis === 'impact' && (
              <span style={{ flex: 'none', width: 26, textAlign: 'right', fontWeight: 600, fontSize: 12, color: '#5fbf92' }}>
                {h.impact}
              </span>
            )}
            <span onClick={() => onOpen(h.path)}
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
