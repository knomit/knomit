import { useLayoutEffect, useRef, useState } from 'react';
import type { Dispatch, ReactNode } from 'react';
import type { Action } from './state';
import type { Fact } from './api';
import { typeStyles, defaultTypeStyle, chipColors, originGlyphs, repoHue, displayLensPath } from './utils';
import { TypeIcon, GitBranchIcon } from './icons';

// FactMetaLine is everything about a fact except its title, its body and its
// controls — on ONE line.
//
// It replaces four stacked rows written in three visual languages: a mono path
// line, a row of chips, and two 70px boxes holding one number each. That header
// stood 233px tall before the first line of prose, and the dashboard says the
// same kind of thing in a fraction of it.
//
// The order is identity → measurement → address: what kind of fact this is,
// how much to trust it, and where it lives. Hairline dots separate the groups;
// the type and origin keep the glyph and colour they wear everywhere else, so
// the line is scannable by shape before it is read.
//
// The MOUNT (lens context only) is drawn the way the summary panel's Repo rows
// draw it — a dot in the mount's hue, then the plain mono name. Not the
// bordered, filled pill this header used to use: that was a third treatment for
// a thing the app already had two of, and it was heavy enough to push the path
// off the end of this line.
export function FactMetaLine({ fact, dispatch, lensMeta, inline = false, pinned = false }: {
  fact: Fact;
  dispatch: Dispatch<Action>;
  /** Source mount of a lens fact. Absent in repo context, where there is only
   *  one repo to be in and naming it would be noise. */
  lensMeta?: { repo: string; branch: string };
  /** Rendered inside FactBand rather than standing on its own: drop the margins
   *  the band supplies, and grow into the band's row so the path is what wraps
   *  when the row runs short. */
  inline?: boolean;
  /** The fact's own title has scrolled out of view, so this line stands in for
   *  it. Everything keeps its place; only the LAST item changes, from the path
   *  to the title. Nothing reflows — the band swaps one item rather than
   *  rebuilding itself, which is what made a shorter pinned line look wrong. */
  pinned?: boolean;
}) {
  // Is the path on the values' row, or has it wrapped to a line of its own?
  // The band shows the title on the line BELOW the values, so a wrapped path is
  // sitting exactly where the title is about to go — and a three-row band is
  // what this is here to prevent. An inline path is not in the way and stays.
  //
  // Measured, because wrapping is a layout outcome: no amount of reading the
  // props tells you whether this row ran out of width. Re-measured on resize,
  // and again whenever the path comes back.
  const lineRef = useRef<HTMLDivElement | null>(null);
  const pathRef = useRef<HTMLSpanElement | null>(null);
  const [pathWrapped, setPathWrapped] = useState(false);
  useLayoutEffect(() => {
    const measure = () => {
      const line = lineRef.current, path = pathRef.current;
      // No path in the DOM means it is already hidden; there is nothing to
      // re-measure until it returns, which unpinning does.
      if (!line || !path) return;
      const first = line.firstElementChild as HTMLElement | null;
      if (!first) return;
      setPathWrapped(path.offsetTop > first.offsetTop + 2);
    };
    measure();
    if (typeof ResizeObserver === 'undefined' || !lineRef.current) return;
    const ro = new ResizeObserver(measure);
    ro.observe(lineRef.current);
    return () => ro.disconnect();
  }, [fact.path, pinned, lensMeta?.repo]);

  const hidePath = pinned && pathWrapped;

  const ts = typeStyles[fact.type || ''] || defaultTypeStyle;
  const oc = chipColors.origin;

  // Built as a list so the separators can be interleaved, never emitted for a
  // part the fact does not have. A leading or trailing dot is the tell that a
  // header is printing a gap where a value should be.
  const parts: ReactNode[] = [];

  if (fact.kind === 'pragmatic') {
    parts.push(
      <span key="kind" data-testid="fact-kind-badge" style={{
        color: '#fc7', background: '#2e2614', fontSize: 9, padding: '1px 5px',
        borderRadius: 3, fontFamily: 'var(--k-font-mono)', letterSpacing: 0.5,
        textTransform: 'uppercase',
      }}>pragmatic</span>,
    );
  }

  if (fact.type) {
    parts.push(
      // NO border on any type, hypothesis included. The old chip gave
      // hypothesis a dashed box to mark it as a claim about the future rather
      // than a record — but on a line where nothing else is boxed, that reads
      // as a leftover chip rather than as meaning, and the distinction is
      // already carried three other ways: the word "hypothesis" itself, its own
      // colour, and HypothesisIcon.
      //
      // The `pragmatic` badge above is now the ONLY boxed thing on this line,
      // and deliberately: it is a FLAG, not a value. Every fact has a type, so a
      // type is data; only a pragmatic fact carries that badge, and an
      // epistemic one shows nothing at all, so its box marks an exception
      // rather than decorating a field.
      <span key="type" data-testid="fact-type-badge" style={{
        color: ts.color, fontSize: 11, fontFamily: 'var(--k-font-mono)',
        display: 'inline-flex', alignItems: 'center', gap: 4,
      }}>
        <TypeIcon type={fact.type} color={ts.color} size={10} /> {ts.label}
      </span>,
    );
  }

  if (fact.origin) {
    parts.push(
      <span key="origin" data-testid="fact-origin-badge" data-value={fact.origin}
        // Filtering is navigation, not an edit — see TagCloud. A read-only
        // fact is still a fact you can ask questions about.
        title={`Filter by origin: ${fact.origin}`}
        onClick={() => dispatch({ type: 'ADD_FILTER', chip: { category: 'origin', value: fact.origin! } })}
        style={{
          color: oc.text, fontSize: 11, fontFamily: 'var(--k-font-mono)',
          cursor: 'pointer',
          display: 'inline-flex', alignItems: 'center', gap: 4,
        }}>
        {originGlyphs[fact.origin] || '◇'} {fact.origin}
      </span>,
    );
  }

  if (fact.confidence !== undefined && fact.confidence !== null) {
    parts.push(
      <span key="conf" data-testid="fact-confidence" style={{ color: '#7f8b9c' }}>
        conf <b style={{ fontFamily: 'var(--k-font-mono)', fontWeight: 400, color: '#8ab0e8' }}>
          {fact.confidence.toFixed(2)}
        </b>
      </span>,
    );
  }

  // "1 source", not a bare 1 under a SOURCES label: on one line the number has
  // no label above it to lean on, so the word has to travel with it.
  if (fact.sources) {
    parts.push(
      <span key="sources" style={{ color: '#7f8b9c' }}>
        <b style={{ fontFamily: 'var(--k-font-mono)', fontWeight: 400, color: '#b9c1cd' }}>{fact.sources}</b>
        {fact.sources === 1 ? ' source' : ' sources'}
      </span>,
    );
  }

  if (lensMeta) {
    const hue = repoHue(lensMeta.repo);
    parts.push(
      <span key="mount" data-testid="source-badge" data-repo={lensMeta.repo}
        style={{ display: 'inline-flex', alignItems: 'center', gap: 6, flex: 'none' }}>
        <span aria-hidden style={{ width: 5, height: 5, borderRadius: '50%', background: hue, flex: 'none' }} />
        <span style={{ fontFamily: 'var(--k-font-mono)', fontSize: 11, color: '#b9c1cd' }}>{lensMeta.repo}</span>
      </span>,
    );
    // The branch rides with the mount rather than in the top bar, because the
    // top bar in a lens names the LENS — its mount count and its write target —
    // and never a read mount's branch. This is the only place it is visible.
    parts.push(
      <span key="branch" data-testid="fact-branch"
        style={{ display: 'inline-flex', alignItems: 'center', gap: 3, color: '#8af', flex: 'none' }}>
        <GitBranchIcon color="#8af" size={11} />
        <span style={{ fontFamily: 'var(--k-font-mono)', fontSize: 10.5 }}>{lensMeta.branch}</span>
      </span>,
    );
  }

  // The path keeps its place unless the title is about to take it — see
  // hidePath. It yields only when it had already wrapped to its own line.
  if (!hidePath) {
    parts.push(
      <span key="path" ref={pathRef} style={{
        fontFamily: 'var(--k-font-mono)', fontSize: 11, color: '#555',
        overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', minWidth: 0,
      }}>{lensMeta ? displayLensPath(fact.path) : fact.path}</span>,
    );
  }

  return (
    <div data-testid="fact-meta" ref={lineRef} style={{
      display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap',
      marginTop: inline ? 0 : 7, marginBottom: inline ? 0 : 20,
      ...(inline ? { flex: '1 1 auto' } : null),
      fontSize: 11.5, minWidth: 0,
    }}>
      {parts.map((p, i) => (
        <span key={i} style={{ display: 'inline-flex', alignItems: 'center', gap: 8, minWidth: 0 }}>
          {i > 0 && <span aria-hidden style={{ color: '#2f3540' }}>·</span>}
          {p}
        </span>
      ))}
      {pinned && (
        <>
          {/* A zero-height, full-width flex item: the standard way to force a
              line break inside a wrapping row. The title takes the line below
              the values — never inline among them, where it competed for the
              row and shoved them around as it changed. The band is therefore
              two rows: values, then title. */}
          <span aria-hidden style={{ flexBasis: '100%', height: 0 }} />
          <span data-testid="fact-band-title" style={{
            fontFamily: 'var(--k-font-display)', fontWeight: 600, fontSize: 12.5, color: '#e8eef6',
            overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
            minWidth: 0, maxWidth: '100%',
          }}>{fact.title || fact.path}</span>
        </>
      )}
    </div>
  );
}
