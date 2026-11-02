import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { ArrowUpIcon, ChevronLeftIcon, DownloadIcon, FocusIcon } from './icons';
import { saveExport, saveOutcomeLine } from './saveFile';
import type { SaveOutcome } from './saveFile';
import { MAX_DEPTH, MAX_REFERENCES, materializeApi } from './materializeApi';
import type { MaterializeApi, MatLink, MatNode, MatOptions, MatTarget } from './materializeApi';
import { clampDepth, clampReferences, exportFilename, id8, optionChange } from './materializeOptions';
import { useMaterializeStream } from './useMaterializeStream';
import { treeInfo } from './materializeNumbering';
import type { TreeInfo } from './materializeNumbering';
import { NESTING, PATH_H, Section, Stepper, Switch } from './materializeParts';
import { useMatFocus } from './useMatFocus';
import './materialize.css';

export { NESTING, PATH_H };

/** How far below the viewport the next page is asked for. */
export const NEAR_BOTTOM_PX = 600;

/** The last path segment of a header-supplied name, so a name can never
 *  carry a directory into the save. */
function fileNameOnly(n: string | undefined): string {
  return (n ?? '').split(/[\\/]/).pop()?.trim() ?? '';
}

type Save = (name: string, text: string, type?: string) => Promise<SaveOutcome>;

type ExportState =
  | { kind: 'idle' }
  | { kind: 'running'; bytes: number }
  | { kind: 'failed'; message: string }
  | { kind: 'done'; line: string };

/** Where to scroll once the section is on screen. 'restore' is the place the
 *  reader was at before an option change; 'jump' is a clicked builds-on link. */
interface Goto { anchor: string; n: number; kind: 'restore' | 'jump'; title: string; epoch: number }

const clip = (t: string, n: number) => (t.length > n ? t.slice(0, n - 1).trimEnd() + '…' : t);

interface PathItem { n: number; full: string; label: string; gap?: boolean }

/** Middle-truncate a path: keep the first PATH_KEEP_HEAD items and as many of the
 *  last as fit the budget, with a gap marker between. */
function midTruncate(items: PathItem[], budget: number): PathItem[] {
  const cost = (i: PathItem) => i.label.length + NESTING.SEP_COST;
  const total = items.reduce((t, i) => t + cost(i), 0);
  const keep = NESTING.PATH_KEEP_HEAD;
  if (total <= budget || items.length <= keep + 1) return items;
  const head = items.slice(0, keep);
  let used = head.reduce((t, i) => t + cost(i), 0) + 1 + NESTING.SEP_COST;
  const tail: PathItem[] = [];
  for (let i = items.length - 1; i >= keep; i--) {
    const c = cost(items[i]);
    if (used + c > budget && tail.length) break;
    tail.unshift(items[i]);
    used += c;
  }
  return [...head, { n: -1, full: '', label: '…', gap: true }, ...tail];
}

/** The path root..n through the walk tree. */
function pathTo(n: number, tree: Map<number, TreeInfo>, byN: Map<number, MatNode>): PathItem[] {
  const chain: number[] = [];
  for (let c: number | null = n; c !== null && tree.has(c); c = tree.get(c)!.parent) chain.unshift(c);
  return chain.map(k => {
    const ti = tree.get(k)!;
    const t = byN.get(k)?.title ?? '';
    const num = ti.num && ti.depth <= NESTING.NUMBER_LEVELS ? `${ti.num} ` : '';
    return { n: k, full: `${ti.num ? `${ti.num} ` : ''}${t}`, label: num + clip(t, NESTING.PATH_TITLE_CHARS) };
  });
}

// ── The view ────────────────────────────────────────────────────────────────

/** How far (px) the reader must scroll before the back-to-top arrow appears. */
export const BACK_TO_TOP_PX = 600;

export function MaterializeView({ target, title, options, onOptionsChange, onBack, api = materializeApi, save = saveExport }: {
  target: MatTarget;
  /** The viewed fact's title; the stream's own root_title wins when it sends one. */
  title: string;
  options: MatOptions;
  onOptionsChange: (o: MatOptions) => void;
  onBack: () => void;
  api?: MaterializeApi;
  /** Saves the exported text: the desktop's native save dialog, or a browser download (saveExport). */
  save?: Save;
}) {
  const s = useMaterializeStream(api, target, options);
  const scroller = useRef<HTMLDivElement>(null);
  const sentinel = useRef<HTMLDivElement>(null);
  const [goto, setGoto] = useState<Goto | null>(null);
  const [scrolled, setScrolled] = useState(false);
  const [exp, setExp] = useState<ExportState>({ kind: 'idle' });
  const exportCtl = useRef<AbortController | null>(null);

  const rootTitle = s.env?.root_title || title;
  // The walk TREE (parents, numbers, tree depth). The payload's `depth` is BFS distance and
  // says nothing about nesting, so nothing below uses it for presentation.
  const tree = useMemo(() => treeInfo(s.sections), [s.sections]);
  const byN = useMemo(() => new Map(s.sections.map(x => [x.n, x])), [s.sections]);
  const [curN, setCurN] = useState<number | null>(null);
  const [pathBudget, setPathBudget] = useState<number>(NESTING.PATH_MAX_CHARS);

  // Focus: opening Materialize takes over the screen. The app shell's rail,
  // library column, browse toolbar and facet tabs are hidden (CSS keyed on this
  // attribute, see materialize.css); the app's top bar and the article's own bar
  // remain. ON by default; with it off NOTHING is hidden and the view only
  // replaces the right-hand panel. Not remembered and not in the URL (it is how
  // the reader chooses to read, not what is materialized). The toggle, Esc and
  // Back end it; Back and leaving the view also restore everything.
  const [focus, setFocus] = useMatFocus();

  // The section at the top of the reading pane, by anchor and ordinal.
  const topSection = useCallback((): Goto | null => {
    const box = scroller.current;
    if (!box) return null;
    const top = box.getBoundingClientRect().top;
    for (const el of Array.from(box.querySelectorAll<HTMLElement>('[data-mat-n]'))) {
      if (el.getBoundingClientRect().bottom > top + PATH_H + 4) {
        return {
          anchor: el.dataset.anchor ?? '', n: Number(el.dataset.matN), kind: 'restore', epoch: 0,
          title: el.querySelector('.k-mat-h')?.textContent ?? '',
        };
      }
    }
    return null;
  }, []);

  // Scroll INSIDE the reading pane. scrollIntoView would also scroll every
  // overflow:hidden ancestor, which is how the fact pane ended up sideways.
  const scrollToEl = useCallback((el: HTMLElement) => {
    const box = scroller.current;
    if (!box) return;
    box.scrollTop += el.getBoundingClientRect().top - box.getBoundingClientRect().top - PATH_H - 8;
  }, []);

  const epoch = s.epoch;
  const placeNow = useCallback((): Goto | null => {
    const t = topSection();
    return t && { ...t, epoch };
  }, [topSection, epoch]);

  const change = useCallback((next: MatOptions) => {
    if (optionChange(options, next) === 'restart') setGoto(placeNow());
    onOptionsChange(next);
  }, [options, onOptionsChange, placeNow]);

  const restartKeepingPlace = useCallback(() => {
    setGoto(placeNow());
    s.restart();
  }, [s, placeNow]);

  // Resolve a pending goto against what is on screen: scroll to it, fetch on
  // toward it, or (a restored place that the new walk no longer has) land on
  // the nearest earlier section and say so.
  const { sections, stale, phase, tail, loadMore } = s;
  /* eslint-disable react-hooks/set-state-in-effect -- resolving a pending scroll target is a reaction to the stream's state */
  useEffect(() => {
    if (!goto || stale || phase !== 'ready') return;
    // A restored place waits for the NEW walk's first page, not the old text.
    if (goto.kind === 'restore' && epoch <= goto.epoch) return;
    const box = scroller.current;
    if (!box) return;
    const hit = box.querySelector<HTMLElement>(`[data-anchor="${CSS.escape(goto.anchor)}"]`);
    if (hit) { scrollToEl(hit); setGoto(null); return; }
    const last = sections[sections.length - 1];
    // A jump knows its ordinal. A restored place does not: the new walk may have
    // put other facts before it, so read on until it shows up or the walk ends.
    if (tail === 'idle' && last && (goto.kind === 'restore' || last.n < goto.n)) { loadMore(); return; }
    if (tail === 'loading') return;
    if (tail === 'failed') { setGoto(null); return; }
    // The walk is exhausted (or has already gone past it) and it is not there.
    const earlier = [...sections].reverse().find(x => x.n <= goto.n) ?? sections[0];
    if (earlier) {
      const el = box.querySelector<HTMLElement>(`[data-anchor="${CSS.escape(earlier.anchor_id)}"]`);
      if (el) scrollToEl(el);
    }
    setGoto(null);
  }, [goto, epoch, stale, phase, tail, sections, loadMore, scrollToEl]);
  // A restart the reader did not start (the count raise whose longer walk does
  // not begin with what was read) dims the text like any other: remember the
  // place now, while the old text is still on screen, and return to it.
  useEffect(() => {
    if (stale && !goto) setGoto(placeNow());
  }, [stale, goto, placeNow]);
  /* eslint-enable react-hooks/set-state-in-effect */

  // Infinite scroll: ask for the next page only when the tail sentinel comes
  // within NEAR_BOTTOM_PX of the pane. The observer is rebuilt after every
  // page so a sentinel that is STILL near (a short page) reports again.
  useEffect(() => {
    const el = sentinel.current;
    const box = scroller.current;
    if (!el || !box || tail !== 'idle' || stale || phase !== 'ready') return;
    const io = new IntersectionObserver(entries => {
      if (entries.some(e => e.isIntersecting)) loadMore();
    }, { root: box, rootMargin: `0px 0px ${NEAR_BOTTOM_PX}px 0px` });
    io.observe(el);
    return () => io.disconnect();
  }, [tail, stale, phase, sections.length, loadMore]);

  // The section at the top of the pane, for the sticky path (below the bar itself).
  const trackCurrent = useCallback(() => {
    const box = scroller.current;
    if (!box) return;
    const line = box.getBoundingClientRect().top + PATH_H + 24;
    let cur: number | null = null;
    for (const el of Array.from(box.querySelectorAll<HTMLElement>('[data-mat-n]'))) {
      if (el.getBoundingClientRect().top <= line) cur = Number(el.dataset.matN); else break;
    }
    setCurN(prev => (prev === cur ? prev : cur));
    const w = box.clientWidth;
    const b = Math.max(20, Math.min(NESTING.PATH_MAX_CHARS, Math.floor((w - 90) / NESTING.CHAR_PX)));
    setPathBudget(prev => (prev === b ? prev : b));
  }, []);
  useEffect(() => { trackCurrent(); }, [trackCurrent, sections.length, stale]);
  useEffect(() => {
    window.addEventListener('resize', trackCurrent);
    return () => window.removeEventListener('resize', trackCurrent);
  }, [trackCurrent]);

  const onGoto = useCallback((l: MatLink) => {
    setGoto({ anchor: l.anchor_id, n: l.n, kind: 'jump', title: l.title, epoch: 0 });
  }, []);

  // ── Export: always the whole document, never what has scrolled in. The desktop app
  // asks where to save (native dialog, opened in Downloads); a browser downloads into
  // its own download location. A cancelled dialog writes nothing and says nothing. ──
  const runExport = useCallback(async () => {
    if (exp.kind === 'running') { exportCtl.current?.abort(); return; }
    const ctl = new AbortController();
    exportCtl.current = ctl;
    setExp({ kind: 'running', bytes: 0 });
    try {
      const doc = await api.exportMarkdown(target, options, ctl.signal, bytes => {
        if (!ctl.signal.aborted) setExp({ kind: 'running', bytes });
      });
      if (ctl.signal.aborted) { setExp({ kind: 'idle' }); return; }
      // The server names its own download (Content-Disposition, and it sanitizes
      // the id part); our builder is only the fallback for a missing header.
      const name = fileNameOnly(doc.serverName) || exportFilename(rootTitle, target.path);
      const out = await save(name, doc.text, 'text/markdown');
      if (out.kind === 'cancelled') setExp({ kind: 'idle' });
      else setExp({ kind: 'done', line: out.kind === 'saved' ? `Saved ${out.where}` : saveOutcomeLine(out) });
    } catch (e) {
      if (ctl.signal.aborted) { setExp({ kind: 'idle' }); return; }
      setExp({ kind: 'failed', message: e instanceof Error ? e.message : String(e) });
    }
  }, [api, exp.kind, options, rootTitle, save, target]);

  useEffect(() => () => exportCtl.current?.abort(), []);

  // ── Render ──
  const total = s.env?.total ?? 0;
  const shown = sections.length;
  const raiseTo = Math.min(MAX_REFERENCES, Math.max(options.references + 1, options.references * 2));
  const trunc = s.env?.truncated;
  // The root is not a reference: count the references shown out of the most the walk can hold.
  const progress = `${Math.max(0, shown - 1)} of ≤${Math.max(0, total - 1)} references`;
  const exporting = exp.kind === 'running';

  return (
    <div className="k-mat" data-testid="materialize-view">
      <div className="k-mat-bar" data-testid="mat-bar">
        <div className="k-mat-row">
          {/* The same bare chevron the library header and the timeline use for Back
              (LibraryHeader's library-back, TimelineNav's timeline-back): the app's
              ChevronLeftIcon at 13px in #888, no text, no border, 20x26. */}
          <button type="button" data-testid="mat-back" onClick={() => { setFocus(false); onBack(); }}
            title="Back to fact" aria-label="Back to fact"
            style={{
              background: 'none', border: 'none', outline: 'none',
              width: 20, height: 26, padding: 0, flexShrink: 0,
              display: 'inline-flex', alignItems: 'center', justifyContent: 'center', cursor: 'pointer',
            }}>
            <ChevronLeftIcon color="#888" size={13} />
          </button>
          <span className="k-mat-title" data-testid="mat-title" title={rootTitle}>{rootTitle}</span>
          {/* The two actions on the materialization sit together: Focus, then Export. */}
          <span className="k-mat-actions" data-testid="mat-actions">
          <button type="button" className="k-bare k-mat-icon" data-testid="mat-focus" aria-pressed={focus} onClick={() => setFocus(f => !f)}
            title={focus ? 'Exit focus' : 'Focus'} aria-label={focus ? 'Exit focus' : 'Focus'}>
            <FocusIcon color="currentColor" size={15} />
          </button>
          {/* Busy is the app's own spinning arc (as the search box shows it), in place
              of the glyph; the tooltip carries the bytes so far, and a second click cancels. */}
          <button type="button" className="k-bare k-mat-icon" data-testid="mat-export" onClick={runExport} data-busy={exporting ? 'true' : 'false'}
            title={exporting ? (exp.bytes > 0 ? `Cancel export (${Math.max(1, Math.round(exp.bytes / 1024))} KB so far)` : 'Cancel export') : 'Export'}
            aria-label={exporting ? 'Cancel export' : 'Export'}>
            {exporting
              ? <svg className="icon-spin" data-testid="mat-export-spinner" width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" aria-hidden="true"><path d="M21 12a9 9 0 1 1-6.219-8.56"/></svg>
              : <DownloadIcon color="currentColor" size={15} />}
          </button>
          </span>
        </div>
        <div className="k-mat-row">
          <div className="k-mat-controls">
            <Stepper label="Max depth" value={options.depth} min={0} max={MAX_DEPTH} step={1} testid="mat-depth"
              onCommit={n => change({ ...options, depth: clampDepth(n) })} />
            <Stepper label="References" value={options.references} min={0} max={MAX_REFERENCES} step={50} testid="mat-refs"
              onCommit={n => change({ ...options, references: clampReferences(n) })} />
            <Switch label="Cross-KB" checked={options.crossKb} testid="mat-cross-kb" onChange={v => change({ ...options, crossKb: v })} />
            <Switch label="Unpin" checked={options.unpin} testid="mat-unpin" onChange={v => change({ ...options, unpin: v })} />
          </div>
        </div>
        <div className="k-mat-meta2">
          <span>{id8(target.path)}</span>
          {s.env && <span>at commit {s.env.anchor.slice(0, 7)} ({s.env.unpinned ? 'latest' : 'pinned'})</span>}
          <span className="k-mat-progress" data-testid="mat-progress" role="status">
            {s.env ? `${progress} · depth ${s.env.depth}` : 'Materializing…'}
            {s.env && total > 0 && <span className="k-mat-bartrack"><span className="k-mat-barfill" style={{ width: `${Math.min(100, (shown / total) * 100)}%`, display: 'block' }} /></span>}
          </span>
        </div>
        {exp.kind === 'failed' && (
          <div className="k-mat-status k-mat-bad" data-testid="mat-export-failed" role="alert">
            Export failed: {exp.message}. No file was written.
            <button type="button" className="k-mat-btn" data-testid="mat-export-retry" onClick={runExport}>Retry</button>
          </div>
        )}
        {exp.kind === 'done' && <div className="k-mat-status" data-testid="mat-export-done" role="status">{exp.line}</div>}
      </div>

      <div className="k-mat-scroll" ref={scroller} data-testid="mat-scroll"
        onScroll={e => { setScrolled(e.currentTarget.scrollTop > BACK_TO_TOP_PX); trackCurrent(); }}>
        {sections.length > 0 && (
          <nav className="k-mat-path" data-testid="mat-path" aria-label="Section path">
            {midTruncate(pathTo(curN ?? sections[0].n, tree, byN), pathBudget).map((it, i, all) => (
              <span key={`${it.n}-${i}`} style={{ display: 'contents' }}>
                {i > 0 && <span className="sep" aria-hidden="true">›</span>}
                {it.gap
                  ? <span className="gap" data-testid="mat-path-gap">…</span>
                  : <button type="button" data-testid="mat-path-item" data-n={it.n} title={it.full} className={i === all.length - 1 ? 'cur' : undefined}
                      onClick={() => {
                        const el = scroller.current?.querySelector<HTMLElement>(`[data-mat-n="${it.n}"]`);
                        if (el) scrollToEl(el);
                      }}>{it.label}</button>}
              </span>
            ))}
          </nav>
        )}
        <article className="k-mat-article" data-stale={stale ? 'true' : 'false'} aria-busy={stale || phase === 'loading'}>
          {phase === 'loading' && sections.length === 0 && (
            <div data-testid="mat-loading" aria-label="Loading">
              <div className="k-mat-skel"><i style={{ width: '60%', height: 20 }} /><i style={{ width: '30%' }} /><i /><i style={{ width: '92%' }} /><i style={{ width: '75%' }} /></div>
            </div>
          )}

          {phase === 'error' && (
            <div className="k-mat-fail" data-testid="mat-error" role="alert">
              Could not materialize: {s.error}
              <button type="button" className="k-mat-btn" data-testid="mat-retry" onClick={s.retry}>Retry</button>
            </div>
          )}

          {phase === 'ready' && sections.length === 0 && tail === 'done' && (
            <div className="k-mat-state" data-testid="mat-empty">Nothing to materialize: this fact has nothing to write.</div>
          )}

          {sections.map((n, i) => (
            <Section key={n.n} node={n} onGoto={onGoto} num={tree.get(n.n)?.num ?? ''} td={tree.get(n.n)?.depth ?? 0}
              group={n.section && n.section !== sections[i - 1]?.section ? n.section : null} />
          ))}

          <div className="k-mat-tail">
            {(tail === 'idle' || tail === 'loading') && phase === 'ready' && sections.length > 0 && (
              <div data-testid="mat-skeleton">
                <div className="k-mat-skel"><i style={{ width: '55%', height: 18 }} /><i /><i style={{ width: '88%' }} /><i style={{ width: '64%' }} /></div>
                <span className="k-mat-reading">Reading on · §{String(shown + 1).padStart(2, '0')} of {total}</span>
              </div>
            )}
            {tail === 'failed' && (
              <div className="k-mat-fail" data-testid="mat-tail-error" role="alert">
                {s.tailExpired
                  ? 'The server no longer holds this read.'
                  : <>Could not load the next page: {s.tailError}</>}
                {s.tailExpired
                  ? <button type="button" className="k-mat-btn" data-testid="mat-restart" onClick={restartKeepingPlace}>Restart and keep my place</button>
                  : <button type="button" className="k-mat-btn" data-testid="mat-tail-retry" onClick={s.retry}>Retry</button>}
              </div>
            )}
            {tail === 'done' && sections.length > 0 && (
              <div className="k-mat-end" data-testid="mat-end">
                <div className="k-mat-endline">
                  <span>End · {shown} {shown === 1 ? 'fact' : 'facts'} · depth {s.env?.depth} reached</span>
                </div>
                {trunc && trunc.nodes > 0 && (
                  <div className="k-mat-endline" data-testid="mat-limit">
                    <span>Stopped at your limit of {options.references} {options.references === 1 ? 'reference' : 'references'} · {trunc.nodes} more not written.</span>
                    {options.references < MAX_REFERENCES
                      ? <button type="button" className="k-mat-btn" data-testid="mat-raise"
                          onClick={() => change({ ...options, references: raiseTo })}>Show up to {raiseTo} references</button>
                      : <span>That is the most a read can hold.</span>}
                  </div>
                )}
                {trunc && trunc.depth > 0 && (
                  <div className="k-mat-endline" data-testid="mat-depth-note">
                    <span>{trunc.depth} cited {trunc.depth === 1 ? 'fact lies' : 'facts lie'} beyond depth {s.env?.depth}.</span>
                    {options.depth < MAX_DEPTH && (
                      <button type="button" className="k-mat-more" data-testid="mat-deeper" title="Go one level deeper"
                        onClick={() => change({ ...options, depth: options.depth + 1 })}>more</button>
                    )}
                  </div>
                )}
              </div>
            )}
            <div ref={sentinel} data-testid="mat-sentinel" style={{ height: 1 }} />
          </div>
        </article>
      </div>
      {scrolled && (
        <button type="button" className="k-bare k-mat-top" data-testid="mat-top" title="Back to top" aria-label="Back to top"
          onClick={() => {
            const box = scroller.current;
            if (!box) return;
            if (typeof box.scrollTo === 'function') box.scrollTo({ top: 0, behavior: 'smooth' });
            else box.scrollTop = 0;
          }}>
          <ArrowUpIcon color="currentColor" size={15} />
        </button>
      )}
    </div>
  );
}
