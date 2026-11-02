import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { ArrowUpIcon, ChevronLeftIcon, DownloadIcon, FocusIcon } from './icons';
import { saveExport, saveOutcomeLine } from './saveFile';
import type { SaveOutcome } from './saveFile';
import { MAX_DEPTH, MAX_REFERENCES } from './materializeApi';
import type { MatLink, MatOptions } from './materializeApi';
import { clampDepth, clampReferences } from './materializeOptions';
import { PATH_H, Section, Stepper, Switch } from './materializeParts';
import { useMatFocus } from './useMatFocus';
import type { FsSectionExtra } from './materializeParts';
import { FsError, factsetApi } from './factsetApi';
import type { FactSetApi, FactSetLink, FactSetNode, FsSearch } from './factsetApi';
import { FactSetAnchors } from './factsetAnchors';
import { useFactSetStream } from './useFactSetStream';
import type { FsFault } from './useFactSetStream';
import './materialize.css';

/** How far below (and above) the viewport the next page is asked for: the
 *  single-fact view's sentinel margin (ruling a8607940). */
export const FS_NEAR_PX = 600;
/** How long the "FactSet refreshed" notice stays. */
export const NOTICE_MS = 6000;
/** The user's wording for the expired-cursor notice (Q-UI13), verbatim. */
export const NOTICE_REFRESHED = 'FactSet refreshed';
/** The export's file name when the server sends none. PLACEHOLDER copy. */
export const FALLBACK_EXPORT_NAME = 'factset.md'; // PLACEHOLDER copy

type Save = (name: string, text: string, type?: string) => Promise<SaveOutcome>;

type ExportState =
  | { kind: 'idle' }
  | { kind: 'running'; bytes: number }
  | { kind: 'failed'; message: string }
  | { kind: 'done'; line: string };

function fileNameOnly(n: string | undefined): string {
  return (n ?? '').split(/[\\/]/).pop()?.trim() ?? '';
}

/** A fault as one line. PLACEHOLDER copy: the server's detail after a short
 *  lead per kind. */
function faultLine(f: FsFault): string {
  switch (f.kind) {
    case 'busy': return `The server is busy: ${f.detail}`; // PLACEHOLDER copy
    case 'invalid': return `The request was refused: ${f.detail}`; // PLACEHOLDER copy
    case 'expired': return `This FactSet is gone: ${f.detail}`; // PLACEHOLDER copy
    default: return `Could not load: ${f.detail}`; // PLACEHOLDER copy
  }
}

/** The FactSet of a search: one article, its units in search order, each a
 *  numbered unit heading (the root) and what it builds on. Same bar, Focus,
 *  Export, ladder and gutter as the single-fact view; links are stubs that say
 *  "see <short title>" and scroll or seek to where the fact is written. */
export function FactSetView({ search, options, onOptionsChange, onBack, api = factsetApi, save = saveExport }: {
  search: FsSearch;
  options: MatOptions;
  onOptionsChange: (o: MatOptions) => void;
  onBack: () => void;
  api?: FactSetApi;
  save?: Save;
}) {
  const s = useFactSetStream(api, search, options);
  const scroller = useRef<HTMLDivElement>(null);
  const bottom = useRef<HTMLDivElement>(null);
  const top = useRef<HTMLDivElement>(null);
  // The FactSet view exists only in Focus (user ruling 2026-11-02, #710 M2):
  // leaving Focus by the toggle or by Esc closes it exactly as Back does (the
  // view unmounts, the stream closes the FactSet with a best-effort DELETE,
  // the URL flag is cleared). Esc in a text or number field still only leaves
  // the field (3fedfce6).
  const close = onBack;
  useMatFocus(close);
  const [scrolled, setScrolled] = useState(false);
  const [exp, setExp] = useState<ExportState>({ kind: 'idle' });
  const exportCtl = useRef<AbortController | null>(null);
  const [pending, setPending] = useState<string | null>(null);
  const [topUnit, setTopUnit] = useState(1);
  const lens = search.target.kind === 'lens';

  const { nodes, env, phase, tail, head, stale, epoch, land, loadMore, loadBefore, seek, reach, reportTop } = s;

  // Every written section ever loaded: anchor -> unit (outlives the run). The
  // view is mounted once per search (RightPanel keys it), so one map per FactSet.
  const [anchors] = useState(() => new FactSetAnchors());
  useEffect(() => { for (const n of nodes) anchors.register(n); }, [nodes, anchors]);

  // The top sentinel is armed per run: a run that landed (a seek, a restart,
  // a refresh) can put the sentinel in view at once, and asking for the unit
  // above then is a call nobody asked for. It is armed only by the reader's
  // own move UP in this run (armedEpoch === epoch): an upward scroll, or a
  // wheel or swipe up at the very top, where no scroll event fires. A scroll
  // down never arms it, and neither does a scroll the view makes itself
  // (landing, keeping the place on a prepend) nor the browser's clamp when a
  // run is swapped: scroll events are ignored until the run has settled
  // (settledEpoch === epoch, set one frame after the landing effect, so the
  // scroll events of the swap and the landing have been dispatched).
  const armedEpoch = useRef(-1);
  const settledEpoch = useRef(-1);
  const touchY = useRef<number | null>(null);
  const viewScroll = useRef<number | null>(null);
  const lastTop = useRef(0);
  const setScrollTop = useCallback((box: HTMLElement, v: number) => {
    box.scrollTop = v;
    viewScroll.current = box.scrollTop;
  }, []);

  const scrollToEl = useCallback((el: HTMLElement) => {
    const box = scroller.current;
    if (!box) return;
    setScrollTop(box, box.scrollTop + el.getBoundingClientRect().top - box.getBoundingClientRect().top - PATH_H - 8);
  }, [setScrollTop]);
  const findAnchor = useCallback((anchor: string) =>
    scroller.current?.querySelector<HTMLElement>(`[data-anchor="${CSS.escape(anchor)}"]`) ?? null, []);

  // A link's click: scroll to the written section when it is on screen, else
  // seek to the link's unit (the stream reads on inside it until the anchor
  // has arrived) and scroll there once the new run is in.
  const onLink = useCallback((l: FactSetLink) => {
    const move = anchors.resolve(l, a => !!findAnchor(a));
    if (move.kind === 'scroll') { const el = findAnchor(move.anchor); if (el) scrollToEl(el); return; }
    if (move.kind === 'seek') { setPending(move.anchor); seek(move.unit, move.anchor); }
  }, [anchors, findAnchor, scrollToEl, seek]);

  // A builds-on link points inside its unit: to the written section when it is
  // on screen, else through the unit's own appearance of it (a stub's link),
  // and when that section has not been read yet, the stream reads on to it.
  const byUnitN = useMemo(() => new Map(nodes.map(n => [`${n.unit}.${n.n}`, n])), [nodes]);
  const onGoto = useCallback((l: MatLink, unit?: number) => {
    const el = findAnchor(l.anchor_id);
    if (el) { scrollToEl(el); return; }
    if (!unit) return;
    const target = byUnitN.get(`${unit}.${l.n}`);
    if (target?.link) { onLink(target.link); return; }
    if (!target) { setPending(l.anchor_id); reach({ unit, n: l.n }); }
  }, [byUnitN, findAnchor, scrollToEl, onLink, reach]);

  // The extras of each FactSet section, stable per node object so the memoized
  // Section re-renders only when its node changed.
  const extras = useMemo(() => {
    const m = new Map<FactSetNode, FsSectionExtra>();
    for (const n of nodes) {
      m.set(n, { unit: n.unit, link: n.link, unavailable: n.unavailable, repoName: n.tree_depth === 0 ? n.repo_name : undefined });
    }
    return m;
  }, [nodes]);

  // Land where the stream says once that run is on screen: a clicked link's
  // anchor, or the unit the reader was at before a restart, a seek or a refresh.
  /* eslint-disable react-hooks/set-state-in-effect -- landing is a reaction to the stream's state (following a stub may start a seek) */
  // Exactly once per landing request: `land` stays set for the whole run while
  // byUnitN is rebuilt on every append and prepend, so without this every new
  // page would snap the reader back to the landing target.
  const landed = useRef<typeof land>(null);
  useEffect(() => {
    if (!land || land.epoch !== epoch || stale || landed.current === land) return;
    const box = scroller.current;
    if (!box) return;
    landed.current = land;
    setPending(null);
    // Section n of the unit (a builds-on target): a stub is followed, a
    // written section is the scroll target.
    if (land.n !== undefined) {
      const t = byUnitN.get(`${land.unit}.${land.n}`);
      if (t?.link) { onLink(t.link); return; }
      const sec = box.querySelector<HTMLElement>(`[data-fs-unit="${land.unit}"][data-mat-n="${land.n}"]`);
      if (sec) { scrollToEl(sec); return; }
    }
    const el = (land.anchor && findAnchor(land.anchor))
      || box.querySelector<HTMLElement>(`[data-fs-unit="${land.unit}"]`);
    if (el) scrollToEl(el);
  }, [land, epoch, stale, findAnchor, scrollToEl, byUnitN, onLink]);
  /* eslint-enable react-hooks/set-state-in-effect */

  // The run is settled one frame after it (and its landing) is on screen.
  useEffect(() => {
    if (stale || phase !== 'ready') return;
    const id = requestAnimationFrame(() => {
      settledEpoch.current = epoch;
      if (scroller.current) lastTop.current = scroller.current.scrollTop;
    });
    return () => cancelAnimationFrame(id);
  }, [epoch, stale, phase, land]);

  // Prepending a unit must not move what the reader is looking at, and
  // neither must the head skeleton shown above the run while it loads. This
  // is the ONLY place-keeping: native scroll anchoring is off on this
  // scroller (.k-fs-scroll in materialize.css), or the two would add up.
  // - the skeleton appears (head -> loading, same first section): scrollTop
  //   grows by its height, so it mounts above the view;
  // - the unit is prepended (the skeleton goes with it): scrollTop grows by
  //   the net height change, so the reader ends at scrollTop + the prepended
  //   height;
  // - the skeleton goes with nothing prepended (a failure): its height is
  //   given back, so the head error at the top is where the reader was.
  const prevHeight = useRef<{ first: string; height: number; head: string } | null>(null);
  const skelH = useRef(0);
  const firstKey = nodes[0] ? `${nodes[0].unit}.${nodes[0].n}` : '';
  useLayoutEffect(() => {
    const box = scroller.current;
    const p = prevHeight.current;
    if (box && p) {
      if (p.first !== firstKey && nodes.some(n => `${n.unit}.${n.n}` === p.first) && head !== 'loading') {
        setScrollTop(box, box.scrollTop + box.scrollHeight - p.height);
        skelH.current = 0;
      } else if (p.first === firstKey && head === 'loading' && p.head !== 'loading') {
        skelH.current = box.scrollHeight - p.height;
        if (skelH.current > 0) setScrollTop(box, box.scrollTop + skelH.current);
      } else if (p.first === firstKey && p.head === 'loading' && head !== 'loading' && skelH.current > 0) {
        setScrollTop(box, box.scrollTop - skelH.current);
        skelH.current = 0;
      }
    }
    prevHeight.current = box ? { first: firstKey, height: box.scrollHeight, head } : null;
  }, [firstKey, nodes, head, setScrollTop]);

  // Infinite scroll down (the next page) and up (the previous unit, Q-UI9).
  useEffect(() => {
    const box = scroller.current;
    if (!box || stale || phase !== 'ready') return;
    const io = new IntersectionObserver(entries => {
      for (const e of entries) {
        if (!e.isIntersecting) continue;
        if (e.target === bottom.current) loadMore();
        if (e.target === top.current && armedEpoch.current === epoch) loadBefore();
      }
    }, { root: box, rootMargin: `${FS_NEAR_PX}px 0px ${FS_NEAR_PX}px 0px` });
    if (bottom.current && tail === 'idle') io.observe(bottom.current);
    if (top.current && head === 'idle') io.observe(top.current);
    return () => io.disconnect();
  }, [tail, head, stale, phase, nodes.length, epoch, loadMore, loadBefore]);

  // The reader moved up: arm the top sentinel and, when it is already in
  // range (the observer reports only changes), ask for the unit above.
  const readerUp = useCallback((box: HTMLElement) => {
    armedEpoch.current = epoch;
    const t = top.current;
    if (t && t.getBoundingClientRect().bottom >= box.getBoundingClientRect().top - FS_NEAR_PX) loadBefore();
  }, [epoch, loadBefore]);

  // A scroll event: the reader's own scroll up arms (readerUp); a scroll down,
  // one the view made, or any before the run settled does not.
  const onScroll = useCallback((box: HTMLElement) => {
    const st = box.scrollTop;
    const up = st < lastTop.current;
    lastTop.current = st;
    const mine = viewScroll.current !== null && Math.abs(st - viewScroll.current) < 2;
    viewScroll.current = null;
    if (mine || settledEpoch.current !== epoch) return;
    if (up) readerUp(box);
  }, [epoch, readerUp]);

  // A wheel or swipe up at the very top scrolls nothing, so no scroll event
  // fires: it is the reader's move up all the same.
  const atTopUp = useCallback((box: HTMLElement) => {
    if (settledEpoch.current === epoch && box.scrollTop <= 0) readerUp(box);
  }, [epoch, readerUp]);

  // The unit at the top of the pane, for the stream (restart / refresh land
  // there) and the unit control.
  const trackTop = useCallback(() => {
    const box = scroller.current;
    if (!box) return;
    const line = box.getBoundingClientRect().top + PATH_H + 24;
    let cur = 0;
    for (const el of Array.from(box.querySelectorAll<HTMLElement>('[data-fs-unit]'))) {
      if (el.getBoundingClientRect().top <= line) cur = Number(el.dataset.fsUnit); else break;
    }
    if (!cur && nodes[0]) cur = nodes[0].unit;
    if (cur) { reportTop(cur); setTopUnit(u => (u === cur ? u : cur)); }
  }, [nodes, reportTop]);
  useEffect(() => { trackTop(); }, [trackTop, nodes.length, epoch]);

  // The refreshed notice goes away on its own.
  const { notice, clearNotice } = s;
  useEffect(() => {
    if (!notice) return;
    const t = window.setTimeout(clearNotice, NOTICE_MS);
    return () => window.clearTimeout(t);
  }, [notice, clearNotice]);

  // ── Export: the whole FactSet, from any live cursor, never partial. A 404
  // means the FactSet expired: it is re-created (the stream's recovery) and the
  // export is offered again. ──
  const cursor = s.cursor;
  const recover = s.recover;
  const runExport = useCallback(async () => {
    if (exp.kind === 'running') { exportCtl.current?.abort(); return; }
    if (!cursor) return;
    const ctl = new AbortController();
    exportCtl.current = ctl;
    setExp({ kind: 'running', bytes: 0 });
    try {
      const doc = await api.exportMarkdown(search.target, cursor, ctl.signal, bytes => {
        if (!ctl.signal.aborted) setExp({ kind: 'running', bytes });
      });
      if (ctl.signal.aborted) { setExp({ kind: 'idle' }); return; }
      const out = await save(fileNameOnly(doc.serverName) || FALLBACK_EXPORT_NAME, doc.text, 'text/markdown');
      if (out.kind === 'cancelled') setExp({ kind: 'idle' });
      else setExp({ kind: 'done', line: out.kind === 'saved' ? `Saved ${out.where}` : saveOutcomeLine(out) });
    } catch (e) {
      if (ctl.signal.aborted) { setExp({ kind: 'idle' }); return; }
      if (e instanceof FsError && e.kind === 'expired') { setExp({ kind: 'idle' }); recover(cursor); return; }
      setExp({ kind: 'failed', message: e instanceof Error ? e.message : String(e) });
    }
  }, [api, cursor, exp.kind, recover, save, search.target]);
  useEffect(() => () => exportCtl.current?.abort(), []);

  const units = env?.units ?? 0;
  const exporting = exp.kind === 'running';
  const canExport = !!cursor && phase === 'ready' && units > 0;

  return (
    <div className="k-mat" data-testid="factset-view">
      <div className="k-mat-bar" data-testid="fs-bar">
        <div className="k-mat-row">
          <button type="button" data-testid="fs-back" onClick={close}
            title="Back" aria-label="Back" // PLACEHOLDER copy
            style={{
              background: 'none', border: 'none', outline: 'none',
              width: 20, height: 26, padding: 0, flexShrink: 0,
              display: 'inline-flex', alignItems: 'center', justifyContent: 'center', cursor: 'pointer',
            }}>
            <ChevronLeftIcon color="#888" size={13} />
          </button>
          <span className="k-mat-title" data-testid="fs-title" title={env?.title}>{env?.title ?? ''}</span>
          <span className="k-mat-actions" data-testid="fs-actions">
            {/* An exit control: Focus is always on while the view is open, so it
                is always pressed and always reads "Exit focus" (the single-fact
                view's wording, reused). */}
            <button type="button" className="k-bare k-mat-icon" data-testid="fs-focus" aria-pressed={true} onClick={close}
              title="Exit focus" aria-label="Exit focus">
              <FocusIcon color="currentColor" size={15} />
            </button>
            <button type="button" className="k-bare k-mat-icon" data-testid="fs-export" onClick={runExport} disabled={!canExport && !exporting}
              data-busy={exporting ? 'true' : 'false'}
              title={exporting ? (exp.bytes > 0 ? `Cancel export (${Math.max(1, Math.round(exp.bytes / 1024))} KB so far)` : 'Cancel export') : 'Export'}
              aria-label={exporting ? 'Cancel export' : 'Export'}>
              {exporting
                ? <svg className="icon-spin" width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" aria-hidden="true"><path d="M21 12a9 9 0 1 1-6.219-8.56"/></svg>
                : <DownloadIcon color="currentColor" size={15} />}
            </button>
          </span>
        </div>
        <div className="k-mat-row">
          <div className="k-mat-controls">
            <Stepper label="Max depth" value={options.depth} min={0} max={MAX_DEPTH} step={1} testid="fs-depth"
              onCommit={n => onOptionsChange({ ...options, depth: clampDepth(n) })} />
            <Stepper label="References" value={options.references} min={0} max={MAX_REFERENCES} step={50} testid="fs-refs"
              onCommit={n => onOptionsChange({ ...options, references: clampReferences(n) })} />
            {/* A lens refuses cross_kb (a lens of any size): the switch is not offered there. */}
            {!lens && <Switch label="Cross-KB" checked={options.crossKb} testid="fs-cross-kb" onChange={v => onOptionsChange({ ...options, crossKb: v })} />}
            <Switch label="Unpin" checked={options.unpin} testid="fs-unpin" onChange={v => onOptionsChange({ ...options, unpin: v })} />
            {units > 1 && (
              <Stepper label="Unit" // PLACEHOLDER copy
                value={topUnit} min={1} max={units} step={1} testid="fs-unit"
                onCommit={u => {
                  const el = scroller.current?.querySelector<HTMLElement>(`[data-fs-unit="${u}"]`);
                  if (el) scrollToEl(el); else seek(u);
                }} />
            )}
          </div>
        </div>
        <div className="k-mat-meta2">
          {env && <span className="k-fs-sub" data-testid="fs-search-line">{env.search_line}</span>}
          <span className="k-mat-progress" data-testid="fs-progress" role="status">
            {/* The unit the reader is on (the one at the top of the pane, as the Unit
                control shows), not the last one loaded. */}
            {env && units > 0 ? `unit ${topUnit} of ${units}` /* PLACEHOLDER copy */ : ''}
          </span>
          {s.notice === 'refreshed' && <span className="k-fs-notice" data-testid="fs-notice" role="status">{NOTICE_REFRESHED}</span>}
        </div>
        {exp.kind === 'failed' && (
          <div className="k-mat-status k-mat-bad" data-testid="fs-export-failed" role="alert">
            Export failed: {exp.message}. No file was written.{/* PLACEHOLDER copy */}
            <button type="button" className="k-mat-btn" data-testid="fs-export-retry" onClick={runExport}>Retry{/* PLACEHOLDER copy */}</button>
          </div>
        )}
        {exp.kind === 'done' && <div className="k-mat-status" data-testid="fs-export-done" role="status">{exp.line}</div>}
      </div>

      <div className="k-mat-scroll k-fs-scroll" ref={scroller} data-testid="fs-scroll"
        onScroll={e => { setScrolled(e.currentTarget.scrollTop > FS_NEAR_PX); trackTop(); onScroll(e.currentTarget); }}
        onWheel={e => { if (e.deltaY < 0) atTopUp(e.currentTarget); }}
        onTouchStart={e => { touchY.current = e.touches[0]?.clientY ?? null; }}
        onTouchMove={e => {
          const y = e.touches[0]?.clientY;
          if (y === undefined || touchY.current === null) return;
          // A finger moving down the screen scrolls the content up.
          if (y > touchY.current) atTopUp(e.currentTarget);
          touchY.current = y;
        }}>
        <article className="k-mat-article" data-stale={stale ? 'true' : 'false'} aria-busy={stale || phase === 'loading'} data-pending={pending ?? undefined}>
          <div ref={top} data-testid="fs-top-sentinel" style={{ height: 1 }} />
          {head === 'loading' && <div className="k-mat-skel" data-testid="fs-head-loading"><i style={{ width: '55%', height: 18 }} /><i /></div>}
          {head === 'failed' && s.headError && (
            // Where it happened: reading the unit above the run.
            <div className="k-mat-fail" data-testid="fs-head-error" role="alert">
              {faultLine(s.headError)}
              {s.headError.kind !== 'invalid' && (
                <button type="button" className="k-mat-btn" data-testid="fs-head-retry" onClick={s.retry}>Retry{/* PLACEHOLDER copy */}</button>
              )}
            </div>
          )}
          {phase === 'loading' && nodes.length === 0 && (
            <div data-testid="fs-loading" aria-label="Loading" // PLACEHOLDER copy
            >
              <div className="k-mat-skel"><i style={{ width: '60%', height: 20 }} /><i style={{ width: '30%' }} /><i /><i style={{ width: '92%' }} /></div>
            </div>
          )}
          {phase === 'error' && s.error && (
            <div className="k-mat-fail" data-testid="fs-error" role="alert">
              {faultLine(s.error)}
              {s.error.kind !== 'invalid' && (
                <button type="button" className="k-mat-btn" data-testid="fs-retry" onClick={s.retry}>Retry{/* PLACEHOLDER copy */}</button>
              )}
            </div>
          )}
          {phase === 'ready' && units === 0 && (
            <div className="k-mat-state" data-testid="fs-empty">The search found nothing to materialize.{/* PLACEHOLDER copy */}</div>
          )}

          {nodes.map(n => (
            <Section key={`${n.unit}.${n.n}`} node={n} num={n.number} td={n.tree_depth}
              group={n.section && n.section !== byUnitN.get(`${n.unit}.${n.n - 1}`)?.section ? n.section : null}
              onGoto={onGoto} fs={extras.get(n)} onLink={onLink} />
          ))}

          <div className="k-mat-tail">
            {tail === 'loading' && phase === 'ready' && nodes.length > 0 && (
              <div className="k-mat-skel" data-testid="fs-skeleton"><i style={{ width: '55%', height: 18 }} /><i /><i style={{ width: '88%' }} /></div>
            )}
            {tail === 'failed' && s.tailError && (
              <div className="k-mat-fail" data-testid="fs-tail-error" role="alert">
                {faultLine(s.tailError)}
                {s.tailError.kind !== 'invalid' && (
                  <button type="button" className="k-mat-btn" data-testid="fs-tail-retry" onClick={s.retry}>Retry{/* PLACEHOLDER copy */}</button>
                )}
              </div>
            )}
            <div ref={bottom} data-testid="fs-sentinel" style={{ height: 1 }} />
          </div>
        </article>
      </div>
      {scrolled && (
        <button type="button" className="k-bare k-mat-top" data-testid="fs-top" title="Back to top" aria-label="Back to top"
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
