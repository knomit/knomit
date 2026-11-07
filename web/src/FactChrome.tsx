import { useEffect, useRef, useState } from 'react';
import type { Dispatch, ReactNode, RefObject } from 'react';
import type { Fact, RefGroup } from './api';
import type { Action } from './state';
import { READ_ONLY_TITLE } from './state';
import { relativeTime } from './utils';
import type { EdgeDir } from './utils';
import { RetractIcon, NewspaperIcon } from './icons';
import { FactBand } from './FactBand';
import { ConnectionsCell, CELL_PAD_X } from './ConnectionsMenu';
import { ConnectionsPanel } from './ConnectionsPanel';
import { VersionWalker } from './VersionWalker';
import { MotifCell } from './MotifCell';
import { MotifOverflowCell, OVERFLOW } from './MotifRow';
import type { OrderedMotifs } from './MotifRow';
import { MotifPanel } from './MotifPanel';

// A fact's chrome, drawn by FactView: the band's edges group, its actions, and the
// page that keeps the band still while the body scrolls and lets the title flow up
// into it.

/** Everything the header's connections menu and its panel need. */
export interface ConnectionsSlot {
  panelId: string;
  open: EdgeDir | null;
  incoming: RefGroup[];
  outgoing: RefGroup[];
  error: string | null;
  onToggle: (dir: EdgeDir) => void;
  onClose: () => void;
  onHop: (path: string, pinnedCommit: string) => void;
  menuRef: RefObject<HTMLSpanElement | null>;
  onMouseEnter: () => void;
  onMouseLeave: () => void;
}

export interface MotifSlot {
  open: string | null;
  onToggle: (motif: string) => void;
  onClose: () => void;
  onPivot: (motif: string) => void;
  panelId: string;
}

// The fact header's control strip: no border, no fill, no dividers — the same
// bare treatment the two rows above it get. Separation comes from the gap and
// from each cell's own padding, and `overflow` is deliberately NOT hidden:
// there is no radius left to clip, and clipping would eat the inset underline
// an open cell draws on its bottom edge.
const controlStrip: React.CSSProperties = {
  // Wraps its cells when the pane is narrower than the strip (FactBand's row 3
  // wraps the strip and the actions apart; this wraps within the strip).
  display: 'inline-flex', alignItems: 'stretch', gap: 2, flexWrap: 'wrap', minWidth: 0,
  // Every cell carries CELL_PAD_X of its own, which an open cell needs so its
  // fill and underline are not flush against the text. With no border to sit
  // inside, that padding pushed the first cell's text one notch right of the
  // path and the meta line above it — three rows of a header that no longer
  // started in the same column. The negative margin gives exactly that padding
  // back, so the text aligns and the highlight still has room.
  marginLeft: -CELL_PAD_X,
};
const stripCell: React.CSSProperties = { display: 'inline-flex', alignItems: 'center' };

/** Row 3's left group: everything that opens the panel below the band. */
export function FactEdgesGroup({ connections, motifs, motifSlot }: {
  connections?: ConnectionsSlot;
  motifs: OrderedMotifs;
  motifSlot?: MotifSlot;
}) {
  return (
          <span
            ref={connections?.menuRef}
            onMouseEnter={connections?.onMouseEnter}
            onMouseLeave={connections?.onMouseLeave}
            style={{ position: 'relative', display: 'flex', minWidth: 0 }}
          >
            <span data-testid="fact-control-strip" style={controlStrip}>
              {connections && (
                <>
                  <span style={stripCell}>
                    <ConnectionsCell
                      dir="in"
                      count={connections.incoming.length}
                      open={connections.open === 'in'}
                      onToggle={connections.onToggle}
                      panelId={connections.panelId}
                      error={connections.error}
                    />
                  </span>
                  <span style={stripCell}>
                    <ConnectionsCell
                      dir="out"
                      count={connections.outgoing.length}
                      open={connections.open === 'out'}
                      onToggle={connections.onToggle}
                      panelId={connections.panelId}
                      error={connections.error}
                    />
                  </span>
                </>
              )}
              {/* The shape cells. Ordered by how many facts carry each motif,
                  never by the order the author typed them into the file — that
                  order means nothing, and the panel below sorts the same way so
                  the row and the panel cannot disagree about one list. */}
              {motifs.shown.map(m => (
                <span key={m.motif} style={stripCell}>
                  <MotifCell
                    motif={m}
                    open={motifSlot?.open === m.motif}
                    onToggle={motifSlot?.onToggle ?? (() => {})}
                    panelId={motifSlot?.panelId ?? ''}
                  />
                </span>
              ))}
              {motifs.shown.length === 0 && (
                <span style={stripCell}>
                  <MotifCell motif={null} open={false} onToggle={() => {}} panelId="" />
                </span>
              )}
              {motifs.hidden.length > 0 && (
                <span style={stripCell}>
                  <MotifOverflowCell
                    hidden={motifs.hidden}
                    open={motifSlot?.open === OVERFLOW}
                    onToggle={motifSlot?.onToggle ?? (() => {})}
                    panelId={motifSlot?.panelId ?? ''}
                  />
                </span>
              )}
            </span>
            {motifSlot?.open && motifs.shown.length + motifs.hidden.length > 0 && (
              <MotifPanel
                id={motifSlot.panelId}
                motifs={[...motifs.shown, ...motifs.hidden]}
                focused={motifSlot.open === OVERFLOW ? null : motifSlot.open}
                onClose={motifSlot.onClose}
                onPivot={motifSlot.onPivot}
                menuRef={connections?.menuRef ?? { current: null }}
                onMouseEnter={connections?.onMouseEnter ?? (() => {})}
                onMouseLeave={connections?.onMouseLeave ?? (() => {})}
              />
            )}
            {connections && (
              <ConnectionsPanel
                id={connections.panelId}
                open={connections.open}
                incoming={connections.incoming}
                outgoing={connections.outgoing}
                error={connections.error}
                onClose={connections.onClose}
                onHop={connections.onHop}
                menuRef={connections.menuRef}
                onMouseEnter={connections.onMouseEnter}
                onMouseLeave={connections.onMouseLeave}
              />
            )}
          </span>
  );
}

/** Row 3's right group: the date, the version, the retraction badge, materialize
 *  and retract (a caller that offers none leaves it out); `children` come last. */
export function FactActions({ fact, histAnchor, onScrub, onRetract, readOnly = false, anchorCommit, readOnlyTitle = READ_ONLY_TITLE, onMaterialize, children }: {
  fact: Fact;
  /** The fact's history anchor: VersionWalker reads its versions from here. */
  histAnchor: { repo: string; branch: string; path: string };
  onScrub?: (commit: string) => void;
  onRetract?: () => void;
  readOnly?: boolean;
  anchorCommit?: string | null;
  readOnlyTitle?: string;
  onMaterialize?: () => void;
  children?: ReactNode;
}) {
  const retractDisabled = readOnly;
  const retractTitle = retractDisabled ? readOnlyTitle : 'Retract fact';
  const retractColor = retractDisabled ? '#444' : '#f66';
  // Retracted-version badge: only when anchorCommit is set (history+history)
  // and fact.commit_hash is a different commit (the backend's ?fallback=before
  // walked back to a pre-retraction version). Compare 7-char prefixes since
  // anchorCommit may already be short.
  const anchorShort = anchorCommit ? anchorCommit.slice(0, 7) : '';
  const factShort = fact.commit_hash ? fact.commit_hash.slice(0, 7) : '';
  const retractedAt = anchorShort && factShort && anchorShort !== factShort ? anchorShort : '';
  return (
    <>
          <span style={{ display: 'flex', alignItems: 'center', gap: 6, flexShrink: 0 }}>
            {fact.commit_date && (
              // WHEN THIS VERSION OF THE FACT WAS COMMITTED — not the anchor's
              // date. It sits here, left of the control strip, for two reasons:
              // the band never scrolls, so it survives a long fact; and it lands
              // beside the version chip, so "v4 · 3d ago" reads as one
              // statement. Deliberately NOT inside the strip — every cell in
              // there is a control, and this is a readout.
              //
              // Amber when anchored, matching the timeline rail and the status
              // pill, where amber already means "you are not at HEAD".
              <span
                data-testid="fact-version-date"
                title={new Date(fact.commit_date).toLocaleString()}
                style={{
                  color: anchorCommit ? '#e5a23c' : '#555',
                  fontSize: 11, fontFamily: 'var(--k-font-mono)',
                  whiteSpace: 'nowrap', flexShrink: 0,
                }}
              >
                {anchorCommit
                  ? new Date(fact.commit_date).toLocaleDateString(undefined,
                      { day: 'numeric', month: 'short', year: 'numeric' })
                  : relativeTime(fact.commit_date)}
              </span>
            )}
            {/*
              RETRACT AND VERSION ARE NOT PANEL-OPENERS, and they are drawn
              apart from the ones that are: pinned right, with the strip's
              cells pinned left. Retract destroys the fact where everything in
              the strip only inspects it, and sitting shoulder to shoulder with
              the navigation controls would make the destructive action look
              like one more place to click. The version walker puts the app
              into history and rotates the left rail into a timeline, and it
              removes itself while its own commits load and on a fact with no
              recorded versions — over here it can come and go without leaving
              a hole, beside the date it is read with, which is where
              "v2 · 3d ago" reads as one statement.

              This separation used to be carried by a border drawn around the
              openers. The border is gone (nothing else in the fact's chrome
              wears one) and the left/right split now carries it alone.
            */}
            {fact.commit_hash && (
              <VersionWalker
                repo={histAnchor.repo}
                branch={histAnchor.branch}
                factPath={histAnchor.path}
                currentCommit={fact.commit_hash}
                onScrub={onScrub ?? (() => {})}
              />
            )}
            {retractedAt && (
              <span
                data-testid="retracted-version-badge"
                title={`This fact was retracted at ${retractedAt}; showing its content from ${factShort}`}
                style={{ color: '#e5a23c', fontFamily: 'var(--k-font-mono)', fontSize: 11, background: 'rgba(229,162,60,0.12)', border: '1px solid rgba(229,162,60,0.35)', padding: '1px 5px', borderRadius: 3 }}
              >
                retracted at {retractedAt}
              </span>
            )}
            {onMaterialize && (
              <button
                data-testid="materialize-btn"
                title="Materialize"
                aria-label="Materialize"
                // .k-materialize: the results header's hover accent (App.css).
                className="k-bare k-materialize"
                onClick={onMaterialize}
                style={{
                  background: 'none', border: 'none', padding: 2,
                  ['--k-materialize-rest' as string]: '#8a96a8',
                  cursor: 'pointer', display: 'flex', alignItems: 'center', opacity: 0.75,
                }}
                onMouseEnter={e => { (e.currentTarget as HTMLElement).style.opacity = '1'; }}
                onMouseLeave={e => { (e.currentTarget as HTMLElement).style.opacity = '0.75'; }}
              ><NewspaperIcon color="currentColor" size={15} /></button>
            )}
            {onRetract && (
              <button
                data-testid="retract-btn"
                title={retractTitle}
                disabled={retractDisabled}
                onClick={onRetract}
                style={{
                  background: 'none', border: 'none', padding: 2,
                  color: retractColor, cursor: retractDisabled ? 'not-allowed' : 'pointer',
                  display: 'flex', alignItems: 'center',
                  opacity: retractDisabled ? 0.4 : 0.6,
                }}
                onMouseEnter={e => { if (!retractDisabled) (e.currentTarget as HTMLElement).style.opacity = '1'; }}
                onMouseLeave={e => { if (!retractDisabled) (e.currentTarget as HTMLElement).style.opacity = '0.6'; }}
              ><RetractIcon color={retractColor} size={15} /></button>
            )}
            {children}
          </span>
    </>
  );
}

/**
 * FactPage is the band over the scrolling body. The band pins once the fact's OWN
 * title has scrolled out of the body below it, and then carries the title itself.
 * An IntersectionObserver rather than a scroll listener: the question is literally
 * "is this element visible in that box", which is what the observer answers, and it
 * does not fire on every frame of a scroll.
 */
export function FactPage({ fact, dispatch, lensMeta, edges, actions, filterable, children }: {
  fact: Fact;
  dispatch: Dispatch<Action>;
  lensMeta?: { repo: string; branch: string };
  edges: ReactNode;
  actions: ReactNode;
  filterable?: boolean;
  children: ReactNode;
}) {
  const titleRef = useRef<HTMLDivElement | null>(null);
  const scrollRef = useRef<HTMLDivElement | null>(null);
  const [pinned, setPinned] = useState(false);
  useEffect(() => {
    const title = titleRef.current;
    const root = scrollRef.current;
    if (!title || !root || typeof IntersectionObserver === 'undefined') return;
    const io = new IntersectionObserver(
      ([entry]) => setPinned(!entry.isIntersecting),
      { root, threshold: 0 },
    );
    io.observe(title);
    return () => io.disconnect();
    // Re-observes per fact: a new fact re-renders a new title node, and opening
    // one always starts unpinned because its body starts at the top.
  }, [fact.path]);
  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%', minHeight: 0 }}>
      <FactBand fact={fact} dispatch={dispatch} lensMeta={lensMeta}
        pinned={pinned} edges={edges} actions={actions} filterable={filterable} />
      <div ref={scrollRef}
        style={{ flex: 1, minHeight: 0, overflowY: 'auto', padding: '20px 28px 24px', boxSizing: 'border-box' }}>
        <div ref={titleRef} data-testid="fact-title" style={{
          fontFamily: 'var(--k-font-display)', fontSize: 18, fontWeight: 600,
          color: '#eee', letterSpacing: '-0.3px', marginBottom: 18,
        }}>
          {fact.title || fact.path}
        </div>
        {children}
      </div>
    </div>
  );
}
