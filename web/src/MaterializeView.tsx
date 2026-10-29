import { createElement, memo, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import ReactMarkdown from 'react-markdown';
import { markdownPlugins, markdownComponents } from './markdown';
import { ArrowUpIcon, ChevronDownIcon, ChevronLeftIcon, DownloadIcon, FocusIcon } from './icons';
import { canChooseLocation, saveOutcomeLine, saveTextFileAs } from './saveFile';
import type { SaveOutcome } from './saveFile';
import { MARK_LATEST_OTHER_KB, MAX_DEPTH, MAX_FACTS, materializeApi } from './materializeApi';
import type { MaterializeApi, MatLink, MatNode, MatOptions, MatTarget } from './materializeApi';
import { clampDepth, clampFacts, exportFilename, headingLevel, id8, optionChange } from './materializeOptions';
import { isHttpUrl } from './utils';
import { useMaterializeStream } from './useMaterializeStream';
import { numberSections } from './materializeNumbering';
import './materialize.css';

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
  /** This browser cannot ask where to save: say where the file will go, first. */
  | { kind: 'confirm' }
  /** Fetched, but the browser's save dialog needs a fresh click to open. */
  | { kind: 'ready'; text: string; name: string }
  | { kind: 'done'; line: string };

/** Where to scroll once the section is on screen. 'restore' is the place the
 *  reader was at before an option change; 'jump' is a clicked builds-on link. */
interface Goto { anchor: string; n: number; kind: 'restore' | 'jump'; title: string; epoch: number }

// ── Controls ────────────────────────────────────────────────────────────────

function Stepper({ label, value, max, min, step, onCommit, testid }: {
  label: string; value: number; min: number; max: number; step: number;
  onCommit: (n: number) => void; testid: string;
}) {
  // What the reader is typing, until they commit it; otherwise the value itself.
  // The range is the server's (depth 0..10, facts 1..500) and is enforced HERE:
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

function Switch({ label, checked, onChange, testid }: { label: string; checked: boolean; onChange: (v: boolean) => void; testid: string }) {
  return (
    <label className="k-mat-switch">
      <input type="checkbox" role="switch" data-testid={testid} checked={checked} onChange={e => onChange(e.target.checked)} />
      <span aria-hidden="true" />
      {label}
    </label>
  );
}

// ── One section ─────────────────────────────────────────────────────────────

const Body = memo(function Body({ text }: { text: string }) {
  return (
    <div className="k-mat-body k-prose">
      <ReactMarkdown remarkPlugins={markdownPlugins} components={markdownComponents}>{text}</ReactMarkdown>
    </div>
  );
});

/** The Builds on / Sources label: the same small uppercase label as before, now a
 *  button that opens and closes the list below it, with an arrow and the count. */
function Disclosure({ label, count, open, onToggle, testid, flag }: {
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

/** How many levels of the outline carry a printed number: "1.", "1.1", "1.1.1".
 *  Deeper sections are unnumbered and show their depth as an indented guide line;
 *  their full position is in the heading's tooltip. */
export const NUMBERED_LEVELS = 3;

/** The number as a heading prefix: "1." for a top-level section, "1.1" below it. */
function numberPrefix(num: string): string {
  return num.includes('.') ? num : `${num}.`;
}

const Section = memo(function Section({ node, group, num, onGoto }: {
  node: MatNode; group: string | null;
  /** The outline number ('' for the top and a frame's ancestors). */
  num: string;
  onGoto: (l: MatLink) => void;
}) {
  const level = headingLevel(node.depth);
  // The outline level of this section (1.1.1 is level 3); beyond NUMBERED_LEVELS it is
  // unnumbered and indented one guide-line step per level past it.
  const outline = num ? num.split('.').length : 0;
  const deep = Math.max(0, outline - NUMBERED_LEVELS);
  const cross = node.marks?.includes(MARK_LATEST_OTHER_KB);
  // Open/closed is the reader's, per section, and not remembered anywhere.
  const [builtOpen, setBuiltOpen] = useState(false);
  const [srcOpen, setSrcOpen] = useState(false);
  const built = node.builds_on ?? [];
  const sources = node.sources ?? [];
  return (
    <>
      {group && <div className="k-mat-group" data-testid="mat-group">{group}</div>}
      <section className={deep > 0 ? 'k-mat-sec k-mat-deep' : 'k-mat-sec'} data-mat-n={node.n}
        data-deep={deep > 0 ? deep : undefined} style={deep > 0 ? ({ '--k-deep': deep } as React.CSSProperties) : undefined} data-anchor={node.anchor_id} data-testid="mat-section"
        aria-labelledby={`${node.anchor_id}-h`}>
        {createElement(`h${level}`, { id: `${node.anchor_id}-h`, className: 'k-mat-h', 'data-level': level, title: deep > 0 ? num : undefined },
          num && deep === 0 ? <span className="k-mat-hnum" data-testid="mat-num">{numberPrefix(num)}{' '}</span> : null,
          node.title || node.path)}
        <div className="k-mat-meta" data-testid="mat-meta">
          <span>{node.path}</span>
          {cross && <span className="k-mat-repo" data-testid="mat-repo-badge">repo {node.repo.slice(0, 8)}</span>}
          {cross && <span className="k-mat-frozen" data-testid="mat-frozen">latest · frozen</span>}
          <span>confidence {node.confidence.toFixed(2)}</span>
          {node.type && <span>{node.type}</span>}
          {node.commit && <span title={node.commit}>@{node.commit.slice(0, 7)}</span>}
          {node.date && <span>{node.date}</span>}
        </div>
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
                <button type="button" className="k-mat-link" data-testid="mat-builds-link" onClick={() => onGoto(l)}
                  title="Go to this section">
                  ↪ {l.title} <span className="k-mat-n">{l.id.slice(0, 8)}</span>
                </button>
                {l.different_version && (
                  <span className="k-mat-changed" data-testid="mat-changed"
                    title="The version cited here has a different body from the one written in this document">changed since cited</span>
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
            {node.not_included.map(o => <span key={o.path} className="k-mat-src">{o.title || o.id || o.path} ({o.reason})</span>)}
          </div>
        )}
      </section>
    </>
  );
}, (a, b) => a.node === b.node && a.group === b.group && a.num === b.num && a.onGoto === b.onGoto);

// ── The view ────────────────────────────────────────────────────────────────

/** How far (px) the reader must scroll before the back-to-top arrow appears. */
export const BACK_TO_TOP_PX = 600;

export function MaterializeView({ target, title, options, onOptionsChange, onBack, api = materializeApi, save = saveTextFileAs, canChoose = canChooseLocation }: {
  target: MatTarget;
  /** The viewed fact's title; the stream's own root_title wins when it sends one. */
  title: string;
  options: MatOptions;
  onOptionsChange: (o: MatOptions) => void;
  onBack: () => void;
  api?: MaterializeApi;
  /** Saves AFTER asking where (saveTextFileAs). */
  save?: Save;
  /** Whether this environment can ask where to save (else the reader is told first). */
  canChoose?: () => boolean;
}) {
  const s = useMaterializeStream(api, target, options);
  const scroller = useRef<HTMLDivElement>(null);
  const sentinel = useRef<HTMLDivElement>(null);
  const [goto, setGoto] = useState<Goto | null>(null);
  const [scrolled, setScrolled] = useState(false);
  const [exp, setExp] = useState<ExportState>({ kind: 'idle' });
  const exportCtl = useRef<AbortController | null>(null);

  const rootTitle = s.env?.root_title || title;
  const nums = useMemo(() => numberSections(s.sections), [s.sections]);

  // Focus: opening Materialize takes over the screen. The app shell's rail,
  // library column, browse toolbar and facet tabs are hidden (CSS keyed on this
  // attribute, see materialize.css); the app's top bar and the article's own bar
  // remain. ON by default; with it off NOTHING is hidden and the view only
  // replaces the right-hand panel. Not remembered and not in the URL (it is how
  // the reader chooses to read, not what is materialized). The toggle, Esc and
  // Back end it; Back and leaving the view also restore everything.
  const [focus, setFocus] = useState(true);
  useEffect(() => {
    if (!focus) return;
    const root = document.documentElement;
    root.dataset.matFocus = '1';
    // Esc never reaches the shell while focus is on (its Escape clears the filters,
    // which would close the open fact and this view), so this listens first, in
    // the capture phase, and keeps the key to itself. In a text/number field or any
    // input it only leaves the field; anywhere else it exits Focus.
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return;
      e.stopImmediatePropagation();
      const a = document.activeElement as HTMLElement | null;
      if (a && (a.tagName === 'INPUT' || a.tagName === 'TEXTAREA' || a.tagName === 'SELECT' || a.isContentEditable)) {
        a.blur();
        return;
      }
      setFocus(false);
    };
    window.addEventListener('keydown', onKey, true);
    return () => { delete root.dataset.matFocus; window.removeEventListener('keydown', onKey, true); };
  }, [focus]);

  // The section at the top of the reading pane, by anchor and ordinal.
  const topSection = useCallback((): Goto | null => {
    const box = scroller.current;
    if (!box) return null;
    const top = box.getBoundingClientRect().top;
    for (const el of Array.from(box.querySelectorAll<HTMLElement>('[data-mat-n]'))) {
      if (el.getBoundingClientRect().bottom > top + 4) {
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
    box.scrollTop += el.getBoundingClientRect().top - box.getBoundingClientRect().top - 8;
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

  const onGoto = useCallback((l: MatLink) => {
    setGoto({ anchor: l.anchor_id, n: l.n, kind: 'jump', title: l.title, epoch: 0 });
  }, []);

  // ── Export: always the whole document, never what has scrolled in, and always
  // to a place the READER chose (the native dialog or showSaveFilePicker); a
  // cancelled dialog writes nothing and says nothing. ──
  const finishSave = useCallback((out: SaveOutcome, text: string, name: string) => {
    if (out.kind === 'cancelled') setExp({ kind: 'idle' });
    else if (out.kind === 'needs-gesture') setExp({ kind: 'ready', text, name });
    else setExp({ kind: 'done', line: out.kind === 'saved' ? `Saved ${out.where}` : saveOutcomeLine(out) });
  }, []);

  const doExport = useCallback(async () => {
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
      finishSave(await save(name, doc.text, 'text/markdown'), doc.text, name);
    } catch (e) {
      if (ctl.signal.aborted) { setExp({ kind: 'idle' }); return; }
      setExp({ kind: 'failed', message: e instanceof Error ? e.message : String(e) });
    }
  }, [api, finishSave, options, rootTitle, save, target]);

  const runExport = useCallback(() => {
    if (exp.kind === 'running') { exportCtl.current?.abort(); return; }
    // A browser that cannot ask where to save (Safari, Firefox) would drop the file
    // into its download folder: say so and let the reader decide, before fetching.
    if (!canChoose() && exp.kind !== 'confirm') { setExp({ kind: 'confirm' }); return; }
    void doExport();
  }, [canChoose, doExport, exp.kind]);

  const saveReady = useCallback(async () => {
    if (exp.kind !== 'ready') return;
    finishSave(await save(exp.name, exp.text, 'text/markdown'), exp.text, exp.name);
  }, [exp, finishSave, save]);

  useEffect(() => () => exportCtl.current?.abort(), []);

  // ── Render ──
  const total = s.env?.total ?? 0;
  const shown = sections.length;
  const raiseTo = Math.min(MAX_FACTS, Math.max(options.maxFacts + 1, options.maxFacts * 2));
  const trunc = s.env?.truncated;
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
            <Stepper label="Max facts" value={options.maxFacts} min={1} max={MAX_FACTS} step={50} testid="mat-facts"
              onCommit={n => change({ ...options, maxFacts: clampFacts(n) })} />
            <Switch label="Cross-KB" checked={options.crossKb} testid="mat-cross-kb" onChange={v => change({ ...options, crossKb: v })} />
            <Switch label="Unpin" checked={options.unpin} testid="mat-unpin" onChange={v => change({ ...options, unpin: v })} />
          </div>
        </div>
        <div className="k-mat-meta2">
          <span>{id8(target.path)}</span>
          {s.env && <span>at commit {s.env.anchor.slice(0, 7)} ({s.env.unpinned ? 'latest' : 'pinned'})</span>}
          <span className="k-mat-progress" data-testid="mat-progress" role="status">
            {s.env ? `${shown} of ≤${total} facts · depth ${s.env.depth}` : 'Materializing…'}
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
        {exp.kind === 'confirm' && (
          <div className="k-mat-status" data-testid="mat-export-confirm" role="status">
            This browser cannot ask where to save. The file will go to your browser's download location.
            <button type="button" className="k-mat-btn" data-testid="mat-export-go" onClick={() => void doExport()}>Download</button>
            <button type="button" className="k-mat-btn" data-testid="mat-export-nevermind" onClick={() => setExp({ kind: 'idle' })}>Cancel</button>
          </div>
        )}
        {exp.kind === 'ready' && (
          <div className="k-mat-status" data-testid="mat-export-ready" role="status">
            The export is ready.
            <button type="button" className="k-mat-btn" data-testid="mat-export-saveas" onClick={() => void saveReady()}>Save as…</button>
            <button type="button" className="k-mat-btn" onClick={() => setExp({ kind: 'idle' })}>Discard</button>
          </div>
        )}
      </div>

      <div className="k-mat-scroll" ref={scroller} data-testid="mat-scroll"
        onScroll={e => setScrolled(e.currentTarget.scrollTop > BACK_TO_TOP_PX)}>
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
            <Section key={n.n} node={n} onGoto={onGoto} num={nums.get(n.n) ?? ''}
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
                    <span>Stopped at your limit of {options.maxFacts} facts · {trunc.nodes} more not written.</span>
                    {options.maxFacts < MAX_FACTS
                      ? <button type="button" className="k-mat-btn" data-testid="mat-raise"
                          onClick={() => change({ ...options, maxFacts: raiseTo })}>Show up to {raiseTo}</button>
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
