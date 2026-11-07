import { useContext, useEffect, useRef, useState } from 'react';
import type { MouseEvent as ReactMouseEvent, ReactNode } from 'react';
import { TypeIcon } from './icons';
import { RepoDot } from './settingsParts';
import { defaultTypeStyle, typeStyles } from './utils';
import { FactLinkContext, factKey } from './chatFactLinks';
import type { ResolvedFact } from './chatFactLinks';
import { PANEL_OPEN_IN_BROWSE } from './chatCopy';
import { createPortal } from 'react-dom';
import { FactMetaLine } from './FactMetaLine';
import { FactBandPath } from './FactBand';
import { CommitHash } from './TimelineNav';
import type { Fact } from './api';

// The fact links of a chat answer (design §3): the chip, its hover card, the
// repo chip, and the per-turn resolution behind them. The answer's markdown
// marks the code spans (chatFactLinks.ts, remarkFactLinks); these draw them.

/** How long the pointer rests on a link before its card shows. */
const FACT_CARD_DELAY_MS = 300;
/** How long the card waits for the pointer to reach it after leaving the link. */
const FACT_CARD_LEAVE_MS = 150;

const ICON_EXTERNAL = (
  <svg width={11} height={11} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
    <path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6" /><polyline points="15 3 21 3 21 9" /><line x1="10" y1="14" x2="21" y2="3" />
  </svg>
);

/** A fact link: the fact type's TypeIcon on its colour, then the title. */
export function FactLink({ handle }: { handle: string }) {
  const env = useContext(FactLinkContext);
  const f = env.facts.get(handle);
  const chip = useRef<HTMLButtonElement>(null);
  const [card, setCard] = useState<{ left: number; top: number } | null>(null);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);
  if (!f) return <code>{handle}</code>;
  const ts = typeStyles[f.type] ?? defaultTypeStyle;
  const open = env.openKey === factKey(f.repo, f.branch, f.path);

  const show = () => {
    clearTimeout(timer.current);
    timer.current = setTimeout(() => {
      const r = chip.current?.getBoundingClientRect();
      if (r) setCard({ left: r.left, top: r.bottom + 6 });
    }, FACT_CARD_DELAY_MS);
  };
  const hide = () => {
    clearTimeout(timer.current);
    timer.current = setTimeout(() => setCard(null), FACT_CARD_LEAVE_MS);
  };
  const stay = () => clearTimeout(timer.current);
  const onClick = (e: ReactMouseEvent) => {
    e.preventDefault();
    clearTimeout(timer.current);
    setCard(null);
    if (e.metaKey || e.ctrlKey) env.onBrowse?.(f.repo, f.path);
    else env.onOpen?.(f);
  };

  return (
    <>
      <button ref={chip} type="button" data-testid="fact-link" data-type={f.type} data-open={open ? 'true' : undefined}
        className={open ? 'k-fact-link k-fact-link--open' : 'k-fact-link'} title={f.title}
        onClick={onClick} onMouseEnter={show} onMouseLeave={hide} onFocus={show} onBlur={hide}>
        <span data-testid="fact-link-glyph" className="k-fact-link-glyph" style={{ background: ts.bg }}>
          <TypeIcon type={f.type} color={ts.color} size={11} />
        </span>
        <span className="k-fact-link-title">{f.title}</span>
      </button>
      {card && <FactCard f={f} at={card} onEnter={stay} onLeave={hide} onBrowse={env.onBrowse} />}
    </>
  );
}

/** The hover card: type badge, repo chip, confidence, the full title, a
 *  two-line excerpt, the path and Open in Browse. Fixed to the viewport, so a
 *  table's scroll box does not clip it. */
function FactCard({ f, at, onEnter, onLeave, onBrowse }: {
  f: ResolvedFact; at: { left: number; top: number };
  onEnter: () => void; onLeave: () => void; onBrowse?: (repo: string, path: string) => void;
}) {
  const left = Math.max(8, Math.min(at.left, (typeof window !== 'undefined' ? window.innerWidth : 0) - CARD_W - 8));
  // The meta line and the path row are Browse's own (FactMetaLine, FactBand's
  // path row), so the card says what Browse says, in Browse's words; the repo
  // is drawn as Browse draws a lens mount. It is portalled to the body: the
  // link sits inside a paragraph, which may not hold the meta line's <div>.
  return createPortal(
    <div role="tooltip" data-testid="fact-card" className="k-fact-card" style={{ left, top: at.top }} onMouseEnter={onEnter} onMouseLeave={onLeave}>
      <FactMetaLine fact={asFact(f)} dispatch={noDispatch} lensMeta={{ repo: f.repo, branch: f.branch }} filterable={false} />
      <div data-testid="fact-card-title" className="k-fact-card-title">{f.title}</div>
      {f.excerpt && <div data-testid="fact-card-excerpt" className="k-fact-card-excerpt">{f.excerpt}</div>}
      <div className="k-fact-card-path">
        <FactBandPath path={f.path} />
        {onBrowse && (
          <button type="button" className="k-fact-card-browse" onClick={() => onBrowse(f.repo, f.path)}>
            {PANEL_OPEN_IN_BROWSE} {ICON_EXTERNAL}
          </button>
        )}
      </div>
    </div>,
    document.body,
  );
}

const noDispatch = () => {};

/** The resolver's answer as the Fact the Browse components read. */
const asFact = (f: ResolvedFact): Fact => ({
  path: f.path, title: f.title, type: f.type, kind: f.kind || undefined, origin: f.origin || undefined,
  confidence: f.confidence, sources: f.sources ?? 0, body: '', domain: [], entities: [], refs: [],
});

const CARD_W = 340;

/** A commit link: the hash as the timeline draws it (CommitHash), opening
 *  Browse on its repo as of that commit. The full hash is the title. */
export function CommitLink({ handle }: { handle: string }) {
  const env = useContext(FactLinkContext);
  const f = env.facts.get(handle);
  if (!f?.commit) return <code>{handle}</code>;
  const { repo, commit } = f;
  return (
    <button type="button" data-testid="commit-link" className="k-commit-link" title={commit}
      onClick={e => { e.preventDefault(); env.onCommit?.(repo, commit); }}>
      <CommitHash commit={commit} />
    </button>
  );
}

/** A repo chip: the repo's colour dot (RepoDot) and its name, in a pill. */
export function RepoLink({ name }: { name: string }) {
  return (
    <span data-testid="repo-link" className="k-repo-link">
      <RepoDot repo={name} size={7} testId="repo-link-dot" />{name}
    </span>
  );
}

/** "+N" after the first fact link of a table cell. */
export function FactLinksMore({ children, title }: { children: ReactNode; title?: string }) {
  return <span data-testid="fact-link-more" className="k-fact-more" title={title}>{children}</span>;
}
