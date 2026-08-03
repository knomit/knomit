import type { Dispatch, ReactNode } from 'react';
import ReactMarkdown from 'react-markdown';
import type { Fact, FactRef } from './api';
import type { Action } from './state';
import { markdownPlugins, markdownComponents } from './markdown';
import { typeStyles, defaultTypeStyle, chipColors } from './utils';
import { TypeIcon } from './icons';

// The hop resolves against the repo-relative path (see qualifyHopTarget in
// useTimeTravel.ts), which the server sends as `path` — deciding that a
// canonical kb://<id>/… ref and its bare equivalent name the same fact is
// ClassifyRef's job, and re-deriving it from `raw` here would be a second
// implementation of the rule in the one language the guard test cannot see.
// `raw` stays what the author wrote, for display. The fallback covers only an
// older server that sends no `path`.
function refTarget(r: FactRef): string {
  return r.path ?? r.raw;
}

interface Props {
  fact: Fact;
  dispatch: Dispatch<Action>;
  readOnly: boolean;
  onRefClick?: (refPath: string) => void;
}

// Provenance glyphs for the origin ghost chip. `authored` is the default and
// elided on the wire, so it normally never renders — kept for completeness.
const originGlyphs: Record<string, string> = {
  authored: '✎',
  distilled: '⚗',
  discovered: '◇',
};

export function FactBody({ fact, dispatch, readOnly, onRefClick }: Props) {
  return (
    <>
      {(fact.type || fact.origin) && (() => {
        const ts = typeStyles[fact.type || ''] || defaultTypeStyle;
        const isPragmatic = fact.kind === 'pragmatic';
        return (
          <div style={{ marginBottom: 14, display: 'inline-flex', alignItems: 'center', gap: 6 }}>
            {isPragmatic && (
              <span data-testid="fact-kind-badge" style={{
                color: '#fc7', background: '#2e2614', fontSize: 9, padding: '2px 6px',
                borderRadius: 3, fontFamily: 'var(--k-font-mono)', letterSpacing: 0.5,
                textTransform: 'uppercase',
              }}>pragmatic</span>
            )}
            {fact.type && (
              <span data-testid="fact-type-badge" style={{
                color: ts.color, background: ts.bg, fontSize: 10, padding: '2px 8px',
                borderRadius: 3, fontFamily: 'var(--k-font-mono)', letterSpacing: 0.5,
                border: fact.type === 'hypothesis' ? `1px dashed ${ts.color}` : 'none',
                display: 'inline-flex', alignItems: 'center', gap: 4,
              }}>
                <TypeIcon type={fact.type} color={ts.color} size={10} /> {ts.label}
              </span>
            )}
            {fact.origin && (() => {
              const oc = chipColors.origin;
              return (
                <span data-testid="fact-origin-badge" data-value={fact.origin}
                  title={readOnly ? `origin: ${fact.origin}` : `Filter by origin: ${fact.origin}`}
                  onClick={() => { if (!readOnly) dispatch({ type: 'ADD_FILTER', chip: { category: 'origin', value: fact.origin! } }); }}
                  style={{
                    color: oc.text, background: 'transparent', fontSize: 10, padding: '2px 8px',
                    borderRadius: 3, fontFamily: 'var(--k-font-mono)', letterSpacing: 0.5,
                    border: `1px solid ${oc.close}`, cursor: readOnly ? 'default' : 'pointer',
                    display: 'inline-flex', alignItems: 'center', gap: 4,
                  }}>
                  {originGlyphs[fact.origin] || '◇'} {fact.origin}
                </span>
              );
            })()}
          </div>
        );
      })()}

      <div data-testid="fact-meta" style={{ display: 'flex', gap: 10, marginBottom: 28 }}>
        <StatBox label="Confidence" value={fact.confidence?.toFixed(2)} color="#8af" />
        <StatBox label="Sources" value={fact.sources} color="#7c9" />
      </div>

      <div data-testid="fact-body" className="k-prose" style={{ color: '#ccc', lineHeight: 1.7, fontSize: 14, marginBottom: 8 }}>
        <ReactMarkdown remarkPlugins={markdownPlugins} components={markdownComponents}>{fact.body || ''}</ReactMarkdown>
      </div>

      <TagCloud label="Domains" entries={fact.domain || []} color="119,204,153"
        readOnly={readOnly}
        onTagClick={d => dispatch({ type: 'ADD_FILTER', chip: { category: 'domain', value: d } })} />
      <TagCloud label="Entities" entries={fact.entities || []} color="136,170,255"
        readOnly={readOnly}
        onTagClick={e => dispatch({ type: 'ADD_FILTER', chip: { category: 'entity', value: e } })} />

      {(() => {
        const allRefs = fact.refs || [];
        if (allRefs.length === 0) return null;
        const mono = { fontSize: 12, fontFamily: 'var(--k-font-mono)' } as const;
        const hoverIn = (e: React.MouseEvent) => { (e.currentTarget as HTMLElement).style.color = '#adf'; };
        const hoverOut = (e: React.MouseEvent) => { (e.currentTarget as HTMLElement).style.color = '#8af'; };
        return (
          <div>
            <div style={{ fontSize: 10, textTransform: 'uppercase', letterSpacing: 1.5, color: '#555', marginBottom: 10 }}>References</div>
            <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
              {/*
                Rendered from the server's `kind`, never from a regex over `raw`.
                The rule that used to live here — schemeless means clickable —
                could not know whether a target existed, could not tell a
                foreign repo from a typo, and made any schemeless string a live
                link. `kind` also encodes existence AT THE VIEWED COMMIT, which
                only the server can determine.
              */}
              {allRefs.map(r => {
                switch (r.kind) {
                  case 'url':
                    // Only http(s) is openable; file:// and other schemes are inert.
                    return /^https?:\/\//i.test(r.raw) ? (
                      <a key={r.raw} href={r.raw} target="_blank" rel="noopener noreferrer"
                        style={{ color: '#8af', fontSize: 12, textDecoration: 'none', transition: 'color 0.15s' }}
                        onMouseEnter={hoverIn} onMouseLeave={hoverOut}
                      >{'↗'} {r.raw}</a>
                    ) : (
                      <span key={r.raw} style={{ color: '#666', ...mono }}>{'→'} {r.raw}</span>
                    );

                  case 'fact':
                    // The server already confirmed this resolves at the version
                    // being viewed; onRefClick hands it to the commit-anchored hop.
                    return onRefClick ? (
                      <span key={r.raw} onClick={() => onRefClick(refTarget(r))}
                        style={{ color: '#8af', cursor: 'pointer', transition: 'color 0.15s', ...mono }}
                        onMouseEnter={hoverIn} onMouseLeave={hoverOut}
                      >{'→'} {refTarget(r)}</span>
                    ) : (
                      <span key={r.raw} style={{ color: '#666', ...mono }}>{'→'} {refTarget(r)}</span>
                    );

                  case 'broken':
                    return (
                      <span key={r.raw} style={{ color: '#a66', ...mono }}
                        title="No fact at this path in the version being viewed">
                        {'⚠'} {r.raw} <span style={{ color: '#555' }}>(unresolved)</span>
                      </span>
                    );

                  case 'foreign':
                    // Not broken — just not ours to open.
                    return (
                      <span key={r.raw} style={{ color: '#666', ...mono }}
                        title="A fact in another knomit repo">
                        {'→'} {r.raw} <span style={{ color: '#555' }}>(another repo)</span>
                      </span>
                    );

                  case 'source_code':
                    return (
                      <span key={r.raw} style={{ color: '#666', ...mono }}
                        title="Source citation — retrieve the exact bytes with: git cat-file blob <blob>">
                        {'→'} {r.raw} <span style={{ color: '#555' }}>(source)</span>
                      </span>
                    );

                  default:
                    return <span key={r.raw} style={{ color: '#666', ...mono }}>{'→'} {r.raw}</span>;
                }
              })}
            </div>
          </div>
        );
      })()}
    </>
  );
}

export function StatBox({ label, value, color }: { label: string; value: ReactNode; color: string }) {
  return (
    <div style={{ borderLeft: `3px solid ${color}`, padding: '10px 16px', background: '#1a1a2a', borderRadius: '0 6px 6px 0', minWidth: 90 }}>
      <div style={{ fontSize: 10, color: '#666', textTransform: 'uppercase', letterSpacing: 1 }}>{label}</div>
      <div style={{ fontFamily: 'var(--k-font-display)', fontSize: 22, fontWeight: 600, color: '#eee', letterSpacing: '-0.01em', marginTop: 2 }}>{value}</div>
    </div>
  );
}

export function TagCloud({ label, entries, color, onTagClick, readOnly = false, focusedValue }: {
  label: string;
  entries: [string, number][] | string[];
  color: string;
  onTagClick: (value: string) => void;
  readOnly?: boolean;
  focusedValue?: string;
}) {
  if (entries.length === 0) return null;

  const items: [string, number][] = typeof entries[0] === 'string'
    ? (entries as string[]).map(s => [s, 1])
    : entries as [string, number][];
  const max = items[0][1];
  const weighted = items.some(([, n]) => n !== items[0][1]);

  return (
    <div style={{ marginBottom: 22 }}>
      <div style={{ fontSize: 10, textTransform: 'uppercase', letterSpacing: 1.5, color: '#555', marginBottom: 10 }}>{label}</div>
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
        {items.map(([name, n]) => {
          const ratio = max > 0 ? n / max : 1;
          const accent = `rgba(${color},`;
          return (
            <span key={name} data-testid="tag-item" data-value={name}
              onClick={() => { if (!readOnly) onTagClick(name); }}
              style={{
                display: 'inline-flex', alignItems: 'center', gap: 5,
                cursor: readOnly ? 'default' : 'pointer',
                padding: weighted && ratio >= 0.75 ? '5px 11px' : weighted ? '4px 9px' : '5px 11px',
                borderRadius: 6,
                background: weighted && ratio < 0.5 ? 'rgba(26,26,42,0.6)' : '#1a1a2a',
                border: `1px solid ${accent}${weighted ? (ratio >= 0.75 ? 0.3 : ratio >= 0.5 ? 0.2 : 0.1) : 0.2})`,
                transition: 'border-color 0.15s, opacity 0.15s',
                outline: name === focusedValue ? `2px solid rgba(${color},0.55)` : 'none',
                outlineOffset: 1,
              }}
              onMouseEnter={e => { if (!readOnly) (e.currentTarget as HTMLElement).style.borderColor = `${accent}0.5)`; }}
              onMouseLeave={e => { (e.currentTarget as HTMLElement).style.borderColor = `${accent}${weighted ? (ratio >= 0.75 ? 0.3 : ratio >= 0.5 ? 0.2 : 0.1) : 0.2})`; }}
            >
              <span style={{
                fontSize: weighted && ratio >= 0.5 ? 12 : weighted ? 11 : 12,
                fontWeight: weighted && ratio >= 0.75 ? 600 : 'normal',
                color: !weighted || ratio >= 0.5 ? `rgb(${color})` : `${accent}0.6)`,
              }}>{name}</span>
              {weighted && (
                <span style={{
                  fontSize: 9, borderRadius: 10, padding: '1px 5px', fontWeight: 600,
                  color: ratio >= 0.5 ? '#111' : `${accent}0.5)`,
                  background: ratio >= 0.75 ? `rgb(${color})` : ratio >= 0.5 ? `${accent}0.8)` : `${accent}0.15)`,
                }}>{n}</span>
              )}
            </span>
          );
        })}
      </div>
    </div>
  );
}
