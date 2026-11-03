// The pieces the single-fact materialize view (MaterializeView) and the
// FactSet view (FactSetView) share: the option controls, a section of the
// article, the body, the Builds on / Sources disclosures, the nesting knobs and
// the Focus mode. Moved out of MaterializeView.tsx without a change in
// behaviour; web/src stays flat.

import { createElement, memo, useState } from 'react';
import ReactMarkdown from 'react-markdown';
import { markdownPlugins, markdownComponents } from './markdown';
import { ChevronDownIcon } from './icons';
import { MARK_LATEST_OTHER_KB } from './materializeApi';
import type { MatLink, MatNode, MatOmission } from './materializeApi';
import { NESTING, headingLevel, midTruncate, ribbonWidth } from './materializeOptions';
import type { PathItem } from './materializeOptions';

export { NESTING };
import { isHttpUrl, repoHue } from './utils';
import type { FactSetLink } from './factsetApi';
import { stubLabel } from './factsetAnchors';

// ── Controls ────────────────────────────────────────────────────────────────

export function Stepper({ label, value, max, min, step, onCommit, testid }: {
  label: string; value: number; min: number; max: number; step: number;
  onCommit: (n: number) => void; testid: string;
}) {
  // What the reader is typing, until they commit it; otherwise the value itself.
  // The range is the server's (depth 0..10, references 0..499) and is enforced HERE:
  // typing clamps as you type, blur and Enter clamp and commit, the buttons stop
  // at the ends, and an out-of-range number never reaches onCommit.
  const [draft, setDraft] = useState<string | null>(null);
  const text = draft ?? String(value);
  const clamp = (n: number) => Math.min(max, Math.max(min, Math.trunc(n)));
  const apply = (raw: string) => {
    setDraft(null);
    const n = Number(raw);
    if (raw.trim() === '' || !Number.isFinite(n)) return;
    const c = clamp(n);
    if (c !== value) onCommit(c);
  };
  const onType = (raw: string) => {
    const digits = raw.replace(/\D/g, '');
    if (digits === '') { setDraft(''); return; }
    setDraft(String(clamp(Number(digits))));
  };
  return (
    <span className="k-mat-ctl">
      {label}
      <span className="k-mat-step">
        <button type="button" aria-label={`Lower ${label}`} disabled={value <= min} onClick={() => apply(String(value - step))}>−</button>
        <input aria-label={label} data-testid={testid} inputMode="numeric" value={text}
          onChange={e => onType(e.target.value)} onBlur={e => apply(e.target.value)}
          onKeyDown={e => { if (e.key === 'Enter') apply((e.target as HTMLInputElement).value); }} />
        <button type="button" aria-label={`Raise ${label}`} disabled={value >= max} onClick={() => apply(String(value + step))}>+</button>
      </span>
    </span>
  );
}

export function Switch({ label, checked, onChange, testid }: { label: string; checked: boolean; onChange: (v: boolean) => void; testid: string }) {
  return (
    <label className="k-mat-switch">
      <input type="checkbox" role="switch" data-testid={testid} checked={checked} onChange={e => onChange(e.target.checked)} />
      <span aria-hidden="true" />
      {label}
    </label>
  );
}

// ── One section ─────────────────────────────────────────────────────────────

export const Body = memo(function Body({ text }: { text: string }) {
  return (
    <div className="k-mat-body k-prose">
      <ReactMarkdown remarkPlugins={markdownPlugins} components={markdownComponents}>{text}</ReactMarkdown>
    </div>
  );
});

/** The Builds on / Sources label: the same small uppercase label as before, now a
 *  button that opens and closes the list below it, with an arrow and the count. */
export function Disclosure({ label, count, open, onToggle, testid, flag }: {
  label: string; count: number; open: boolean; onToggle: () => void; testid: string;
  /** An amber note on the label itself, for what the closed list would hide. */
  flag?: string;
}) {
  return (
    <button type="button" className="k-mat-label k-mat-toggle" data-testid={testid} aria-expanded={open} onClick={onToggle}>
      <span aria-hidden="true" className="k-mat-arrow" data-open={open ? 'true' : 'false'}><ChevronDownIcon color="currentColor" size={10} /></span>
      {label} · {count}
      {flag && <span className="k-mat-dot" role="img" aria-label={flag} title={flag} />}
    </button>
  );
}

/** The height (px) of the sticky section path. */
export const PATH_H = 28;

// ── The sticky section path (shared by both views) ──────────────────────────

/** The sticky section path at the top of the article: `lead` (the FactSet's
 *  progress), then the items with " › " between, the last one current. */
export function SectionPath({ items, budget, onPick, lead, prefix }: {
  items: PathItem[]; budget: number; onPick: (it: PathItem) => void;
  /** Shown before the items, with a " · " after it; its length comes off the budget. */
  lead?: { text: string; testid: string };
  /** The test ids' prefix: "mat" (single-fact view) or "fs" (FactSet view). */
  prefix: 'mat' | 'fs';
}) {
  const shown = midTruncate(items, budget - (lead ? lead.text.length + NESTING.SEP_COST : 0));
  return (
    <nav className="k-mat-path" data-testid={`${prefix}-path`} aria-label="Section path">
      {lead && <span className="k-mat-path-lead" data-testid={lead.testid} role="status">{lead.text}</span>}
      {lead && shown.length > 0 && <span className="sep" aria-hidden="true">·</span>}
      {shown.map((it, i, all) => (
        <span key={`${it.unit ?? ''}-${it.n}-${i}`} style={{ display: 'contents' }}>
          {i > 0 && <span className="sep" aria-hidden="true">›</span>}
          {it.gap
            ? <span className="gap" data-testid={`${prefix}-path-gap`}>…</span>
            : <button type="button" data-testid={`${prefix}-path-item`} data-n={it.n} data-unit={it.unit} title={it.full}
                className={i === all.length - 1 ? 'cur' : undefined} onClick={() => onPick(it)}>{it.label}</button>}
        </span>
      ))}
    </nav>
  );
}

// ── Marks (user ruling Q-UI12, 2026-11-01: both read "changed since cited",
// each with its own tooltip, verbatim) ──────────────────────────────────────

/** The words of both version marks (user's wording). */
export const CHANGED_SINCE_CITED = 'changed since cited';
/** Tooltip of `different_version`: the version cited at this appearance is not
 *  the version written in the document (user's wording). */
export const TIP_DIFFERENT_VERSION = 'the cited version differs from the one shown here';
/** Tooltip of RULE 1 `changed_since_cited`: the version shown or cited is not
 *  what the reading branch holds now (user's wording). */
export const TIP_RULE1 = 'the cited version differs from what the branch holds now';
/** RULE 1 marks on a FactSet section (computed when the page is served). */
export const MARK_CHANGED_SINCE_CITED = 'changed_since_cited';
export const MARK_DELETED = 'deleted';

/** What a FactSet section adds to a materialize section: its unit, the link
 *  of a heading-only stub, the repo name on a lens unit root, an unavailable
 *  root. */
export interface FsSectionExtra {
  unit: number;
  link?: FactSetLink;
  repoName?: string;
  unavailable?: boolean;
}

/** The number as a heading prefix: "1." for a top-level section, "1.1" below it. */
/** A "Related, not included" entry. A lens FactSet's not-followed leaf
 *  (reason lens_repo) reads "<repo name> `<path>`", the fact path without its
 *  kb://<id12>/ prefix and no reason code, as the server's export writes it
 *  (render_agent.go omissionText, wording S2). Every other reason reads as it
 *  always has. */
function omissionLabel(o: MatOmission) {
  if (o.reason === 'lens_repo') {
    return <>{o.repo_name} <code>{o.path.replace(/^kb:\/\/[^/]+\//, '')}</code></>;
  }
  return `${o.title || o.id || o.path} (${o.reason})`;
}

function numberPrefix(num: string): string {
  return num.includes('.') ? num : `${num}.`;
}



export const Section = memo(function Section({ node, group, num, td, onGoto, fs, onLink }: {
  node: MatNode; group: string | null;
  /** The full outline number ('' for the top and a frame's ancestors). */
  num: string;
  /** TREE depth (0 for the top): what the ladder, the gutter and the numbers follow.
   *  Not node.depth, which is the BFS distance from the root. */
  td: number;
  /** A builds-on link's click; a FactSet section also passes its unit (builds_on
   *  point inside the unit). */
  onGoto: (l: MatLink, unit?: number) => void;
  /** Set on a FactSet section (FactSetView); absent on the single-fact view. */
  fs?: FsSectionExtra;
  /** A FactSet stub's click (stable). */
  onLink?: (l: FactSetLink) => void;
}) {
  const lv = Math.min(td + 1, NESTING.LADDER_MAX_LEVEL);
  const tag = `h${headingLevel(td)}`;
  // A FactSet unit root carries its unit number in the ribbon in the gutter,
  // not in the heading (book design §5): the heading is the title only.
  const unitRoot = !!fs && td === 0;
  const numbered = num !== '' && td <= NESTING.NUMBER_LEVELS && !unitRoot;
  const ticks = Math.min(td, NESTING.GUTTER_CAP);
  const cross = node.marks?.includes(MARK_LATEST_OTHER_KB);
  // Open/closed is the reader's, per section, and not remembered anywhere.
  const [builtOpen, setBuiltOpen] = useState(false);
  const [srcOpen, setSrcOpen] = useState(false);
  const built = node.builds_on ?? [];
  const sources = node.sources ?? [];
  // A FactSet stub shares its fact's anchor with the written section: only the
  // written one (or an unavailable root) is a scroll target and owns the id.
  const stub = fs?.link;
  const hid = stub ? `${node.anchor_id}-${fs.unit}-${node.n}-h` : `${node.anchor_id}-h`;
  const rule1 = fs ? (node.marks ?? []).filter(m => m === MARK_CHANGED_SINCE_CITED || m === MARK_DELETED) : [];
  return (
    <>
      {group && <div className="k-mat-group" data-testid="mat-group">{group}</div>}
      <section className={unitRoot ? 'k-mat-sec k-fs-unit' : 'k-mat-sec'} data-mat-n={node.n} data-lv={lv} data-td={td}
        data-anchor={stub ? undefined : node.anchor_id} data-testid="mat-section"
        data-fs-unit={fs?.unit} data-fs-stub={stub ? 'true' : undefined}
        aria-labelledby={hid}>
        <div className="k-mat-gut" data-testid="mat-gutter" data-ticks={ticks} title={num ? `Position ${num} · depth ${td}` : undefined}>
          {unitRoot && (
            <span className="k-fs-ribbon" data-testid="fs-ribbon" style={{ width: ribbonWidth(fs.unit) }}>{fs.unit}</span>
          )}
          {ticks > 0 && (
            <div className="k-mat-gut-m">
              {Array.from({ length: ticks }, (_, i) => <i key={i} className={td > NESTING.GUTTER_CAP && i === ticks - 1 ? 'cap' : undefined} />)}
            </div>
          )}
        </div>
        <div className="k-mat-main">
        {createElement(tag, { id: hid, className: 'k-mat-h', 'data-lv': lv, title: num ? `Position ${num}` : undefined },
          numbered ? <span className="k-mat-hnum" data-testid="mat-num">{numberPrefix(num)}{' '}</span> : null,
          node.title || node.path)}
        <div className="k-mat-meta" data-testid="mat-meta">
          {fs?.repoName && (
            // The member a lens unit comes from, drawn as lens search rows draw a
            // mount: a dot in the repo's hue and its plain name.
            <span className="k-fs-repo" data-testid="fs-repo-name" data-repo={fs.repoName}>
              <span aria-hidden="true" style={{ background: repoHue(fs.repoName) }} />{fs.repoName}
            </span>
          )}
          <span>{node.path}</span>
          {cross && <span className="k-mat-repo" data-testid="mat-repo-badge">repo {node.repo.slice(0, 8)}</span>}
          {cross && <span className="k-mat-frozen" data-testid="mat-frozen">latest · frozen</span>}
          <span>confidence {node.confidence.toFixed(2)}</span>
          {node.type && <span>{node.type}</span>}
          {node.commit && <span title={node.commit}>@{node.commit.slice(0, 7)}</span>}
          {node.date && <span>{node.date}</span>}
          {/* RULE 1 marks: an inline chip after the meta line (Q-UI11), annotations
              only. They are about NOW and are kept apart from different_version. */}
          {rule1.map(m => m === MARK_DELETED
            ? <span key={m} className="k-mat-changed k-fs-rule1" data-testid="fs-mark-deleted"
                title="the branch no longer holds this fact" // PLACEHOLDER copy
              >deleted{/* PLACEHOLDER copy */}</span>
            : <span key={m} className="k-mat-changed k-fs-rule1" data-testid="fs-mark-rule1" title={TIP_RULE1}>{CHANGED_SINCE_CITED}</span>)}
        </div>
        {fs?.unavailable && (
          <div className="k-fs-sub" data-testid="fs-unavailable">
            no longer available{/* PLACEHOLDER copy */}
          </div>
        )}
        {stub && (
          <div className="k-mat-rel" data-testid="fs-stub">
            <button type="button" className="k-mat-link" data-testid="fs-stub-link" data-direction={stub.direction}
              onClick={() => onLink?.(stub)}>{stubLabel(node.title || node.path)}</button>
            {stub.different_version && (
              <span className="k-mat-changed" data-testid="fs-different-version" title={TIP_DIFFERENT_VERSION}>{CHANGED_SINCE_CITED}</span>
            )}
          </div>
        )}
        {node.body ? <Body text={node.body} /> : null}
        {(built.length > 0 || sources.length > 0) && (
          <div className="k-mat-toggles">
            {built.length > 0 && (
              <Disclosure label="Builds on" count={built.length} open={builtOpen} onToggle={() => setBuiltOpen(o => !o)} testid="mat-builds-toggle"
                flag={built.some(l => l.different_version) ? 'a cited version changed since it was cited' : undefined} />
            )}
            {sources.length > 0 && (
              <Disclosure label="Sources" count={sources.length} open={srcOpen} onToggle={() => setSrcOpen(o => !o)} testid="mat-sources-toggle" />
            )}
          </div>
        )}
        {builtOpen && built.length > 0 && (
          <div className="k-mat-rel" data-testid="mat-builds-on">
            {built.map(l => (
              <span key={l.anchor_id}>
                <button type="button" className="k-mat-link" data-testid="mat-builds-link" onClick={() => onGoto(l, fs?.unit)}
                  title="Go to this section">
                  ↪ {l.title} <span className="k-mat-n">{l.id.slice(0, 8)}</span>
                </button>
                {l.different_version && (
                  <span className="k-mat-changed" data-testid="mat-changed" title={TIP_DIFFERENT_VERSION}>{CHANGED_SINCE_CITED}</span>
                )}
              </span>
            ))}
          </div>
        )}
        {srcOpen && sources.length > 0 && (
          <div className="k-mat-rel" data-testid="mat-sources">
            {sources.map(src => (
              <span key={src.label + (src.url ?? '')} className="k-mat-src">
                {isHttpUrl(src.url) ? <a href={src.url} target="_blank" rel="noopener noreferrer">{src.label}</a> : src.label}
              </span>
            ))}
          </div>
        )}
        {node.not_included && node.not_included.length > 0 && (
          <div className="k-mat-rel">
            <span className="k-mat-label">Related, not included</span>
            {node.not_included.map(o => <span key={o.path} className="k-mat-src">{omissionLabel(o)}</span>)}
          </div>
        )}
        </div>
      </section>
    </>
  );
}, (a, b) => a.node === b.node && a.group === b.group && a.num === b.num && a.td === b.td && a.onGoto === b.onGoto
  && a.fs?.unit === b.fs?.unit && a.fs?.link === b.fs?.link && a.fs?.repoName === b.fs?.repoName
  && a.fs?.unavailable === b.fs?.unavailable && a.onLink === b.onLink);
