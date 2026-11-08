import { useContext, useEffect, useRef, useState } from 'react';
import type { MouseEvent as ReactMouseEvent, ReactNode } from 'react';
import { TypeIcon } from './icons';
import { RepoDot } from './settingsParts';
import { defaultTypeStyle, typeStyles } from './utils';
import { FactLinkContext, commitKey, factKey } from './chatFactLinks';
import type { ResolvedFact } from './chatFactLinks';
import { createPortal } from 'react-dom';
import { FactMetaLine } from './FactMetaLine';
import { FactBandPath } from './FactBand';
import { CommitHash } from './TimelineNav';
import { noMouseFocus } from './utils';
import type { Fact } from './api';

// The fact links of a chat answer (design §3): the chip, its hover card, the
// repo chip, and the per-turn resolution behind them. The answer's markdown
// marks the code spans (chatFactLinks.ts, remarkFactLinks); these draw them.

/** How long the pointer rests on a link before its card shows. */
const FACT_CARD_DELAY_MS = 300;
/** How long the card waits for the pointer to reach it after leaving the link. */
const FACT_CARD_LEAVE_MS = 150;

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
    if (e.metaKey || e.ctrlKey) env.onBrowse?.(f);
    else env.onOpen?.(f);
  };

  return (
    <>
      {/* data-open says the panel shows this fact; it is NOT drawn: a clicked chip
          looks like any other (no selected border). A mouse press takes no focus
          (noMouseFocus), so only the keyboard's :focus-visible draws a ring. */}
      <button ref={chip} type="button" data-testid="fact-link" data-type={f.type} data-open={open ? 'true' : undefined}
        className="k-fact-link" title={f.title}
        onMouseDown={noMouseFocus} onClick={onClick} onMouseEnter={show} onMouseLeave={hide} onFocus={show} onBlur={hide}>
        <span data-testid="fact-link-glyph" className="k-fact-link-glyph" style={{ background: ts.bg }}>
          <TypeIcon type={f.type} color={ts.color} size={11} />
        </span>
        <span className="k-fact-link-title">{f.title}</span>
      </button>
      {card && <FactCard f={f} at={card} onEnter={stay} onLeave={hide} />}
    </>
  );
}

/** The hover card: type badge, repo chip, confidence, the full title, a
 *  two-line excerpt and the path. Cmd-click on the link goes to Browse. Fixed to the viewport, so a
 *  table's scroll box does not clip it. */
function FactCard({ f, at, onEnter, onLeave }: {
  f: ResolvedFact; at: { left: number; top: number };
  onEnter: () => void; onLeave: () => void;
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
      </div>
    </div>,
    document.body,
  );
}

const noDispatch = () => {};

/** The resolver's answer as the Fact the Browse components read. A fact the
 *  server could not parse has no confidence: it stays ABSENT here, as Browse's
 *  own fact read leaves it (normalizeFactResponse), so FactMetaLine omits it
 *  instead of showing "conf 0.00". */
const asFact = (f: ResolvedFact): Fact => ({
  path: f.path, title: f.title, type: f.type, kind: f.kind || undefined, origin: f.origin || undefined,
  sources: f.sources ?? 0, body: '', domain: [], entities: [], refs: [],
  confidence: f.confidence as number, // undefined passes through, exactly as in normalizeFactResponse
});

const CARD_W = 340;

/** A commit link: the hash as the timeline draws it (CommitHash), opening the
 *  commit in the side panel. The full hash is the title. It stays highlighted
 *  while that commit is on screen, as a fact link does. */
export function CommitLink({ handle }: { handle: string }) {
  const env = useContext(FactLinkContext);
  const f = env.facts.get(handle);
  if (!f?.commit) return <code>{handle}</code>;
  const { repo, branch, commit } = f;
  const open = env.openKey === commitKey(repo, branch, commit);
  return (
    <button type="button" data-testid="commit-link" data-open={open ? 'true' : undefined} className={open ? 'k-commit-link k-commit-link--open' : 'k-commit-link'} title={commit}
      onClick={e => { e.preventDefault(); env.onCommit?.(repo, branch, commit); }}>
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
