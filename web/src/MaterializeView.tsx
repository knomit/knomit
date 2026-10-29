import { createElement, memo, useCallback, useEffect, useRef, useState } from 'react';
import ReactMarkdown from 'react-markdown';
import { markdownPlugins, markdownComponents } from './markdown';
import { ChevronLeftIcon, DownloadIcon } from './icons';
import { saveOutcomeLine, saveTextFile } from './saveFile';
import type { SaveOutcome } from './saveFile';
import { MARK_LATEST_OTHER_KB, MAX_DEPTH, MAX_FACTS, materializeApi } from './materializeApi';
import type { MaterializeApi, MatLink, MatNode, MatOptions, MatTarget } from './materializeApi';
import { clampDepth, clampFacts, exportFilename, headingLevel, id8, optionChange } from './materializeOptions';
import { isHttpUrl } from './utils';
import { useMaterializeStream } from './useMaterializeStream';
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
  const [draft, setDraft] = useState<string | null>(null);
  const text = draft ?? String(value);
  const apply = (raw: string) => {
    setDraft(null);
    const n = Number(raw);
    if (raw.trim() === '' || !Number.isFinite(n)) return;
    const c = Math.min(max, Math.max(min, Math.trunc(n)));
    if (c !== value) onCommit(c);
  };
  return (
    <span className="k-mat-ctl">
      {label}
      <span className="k-mat-step">
        <button type="button" aria-label={`Lower ${label}`} disabled={value <= min} onClick={() => apply(String(value - step))}>−</button>
        <input aria-label={label} data-testid={testid} inputMode="numeric" value={text}
          onChange={e => setDraft(e.target.value)} onBlur={e => apply(e.target.value)}
          onKeyDown={e => { if (e.key === 'Enter') apply((e.target as HTMLInputElement).value); }} />
        <button type="button" aria-label={`Raise ${label}`} disabled={value >= max} onClick={() => apply(String(value + step))}>+</button>
      </span>
      <span className="k-mat-of">of {max}</span>
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

const Section = memo(function Section({ node, group, onGoto }: {
  node: MatNode; group: string | null; onGoto: (l: MatLink) => void;
}) {
  const level = headingLevel(node.depth);
  const cross = node.marks?.includes(MARK_LATEST_OTHER_KB);
  return (
    <>
      {group && <div className="k-mat-group" data-testid="mat-group">{group}</div>}
      <section className="k-mat-sec" data-mat-n={node.n} data-anchor={node.anchor_id} data-testid="mat-section"
        aria-labelledby={`${node.anchor_id}-h`}>
        {createElement(`h${level}`, { id: `${node.anchor_id}-h`, className: 'k-mat-h', 'data-level': level }, node.title || node.path)}
        <div className="k-mat-meta" data-testid="mat-meta">
          <span className="k-mat-n">§{String(node.n).padStart(2, '0')}</span>
          <span>{node.path}</span>
          {cross && <span className="k-mat-repo" data-testid="mat-repo-badge">repo {node.repo.slice(0, 8)}</span>}
          {cross && <span className="k-mat-frozen" data-testid="mat-frozen">latest · frozen</span>}
          <span>confidence {node.confidence.toFixed(2)}</span>
          {node.type && <span>{node.type}</span>}
          {node.commit && <span title={node.commit}>@{node.commit.slice(0, 7)}</span>}
          {node.date && <span>{node.date}</span>}
        </div>
        {node.body ? <Body text={node.body} /> : null}
        {node.builds_on && node.builds_on.length > 0 && (
          <div className="k-mat-rel" data-testid="mat-builds-on">
            <span className="k-mat-label">Builds on</span>
            {node.builds_on.map(l => (
              <span key={l.anchor_id}>
                <button type="button" className="k-mat-link" data-testid="mat-builds-link" onClick={() => onGoto(l)}
                  title={`Go to §${l.n}`}>
                  ↪ {l.title} <span className="k-mat-n">§{String(l.n).padStart(2, '0')} · {l.id.slice(0, 8)}</span>
                </button>
                {l.different_version && (
                  <span className="k-mat-changed" data-testid="mat-changed"
                    title="The version cited here has a different body from the one written in this document">changed since cited</span>
                )}
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
        {node.sources && node.sources.length > 0 && (
          <div className="k-mat-rel">
            <span className="k-mat-label">Sources</span>
            {node.sources.map(s => (
              <span key={s.label + (s.url ?? '')} className="k-mat-src">
                {isHttpUrl(s.url) ? <a href={s.url} target="_blank" rel="noopener noreferrer">{s.label}</a> : s.label}
              </span>
            ))}
          </div>
        )}
      </section>
    </>
  );
});

// ── The view ────────────────────────────────────────────────────────────────

export function MaterializeView({ target, title, options, onOptionsChange, onBack, api = materializeApi, save = saveTextFile }: {
  target: MatTarget;
  /** The viewed fact's title; the stream's own root_title wins when it sends one. */
  title: string;
  options: MatOptions;
  onOptionsChange: (o: MatOptions) => void;
  onBack: () => void;
  api?: MaterializeApi;
  save?: Save;
}) {
  const s = useMaterializeStream(api, target, options);
  const scroller = useRef<HTMLDivElement>(null);
  const sentinel = useRef<HTMLDivElement>(null);
  const [goto, setGoto] = useState<Goto | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [exp, setExp] = useState<ExportState>({ kind: 'idle' });
  const exportCtl = useRef<AbortController | null>(null);

  const rootTitle = s.env?.root_title || title;

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
      if (goto.kind === 'restore') {
        setNotice(`“${goto.title}” is not in this walk any more. Showing the nearest earlier section${earlier.title ? `, “${earlier.title}”` : ''}.`);
      }
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
    setNotice(null);
    setGoto({ anchor: l.anchor_id, n: l.n, kind: 'jump', title: l.title, epoch: 0 });
  }, []);

  // ── Export: always the whole document, never what has scrolled in. ──
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
      setExp({ kind: 'done', line: out.kind === 'saved' ? `Saved ${out.where}` : saveOutcomeLine(out) });
    } catch (e) {
      if (ctl.signal.aborted) { setExp({ kind: 'idle' }); return; }
      setExp({ kind: 'failed', message: e instanceof Error ? e.message : String(e) });
    }
  }, [api, exp.kind, options, rootTitle, save, target]);

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
          <button type="button" className="k-mat-btn" data-testid="mat-back" onClick={onBack} title="Back to the fact" aria-label="Back to the fact">
            <ChevronLeftIcon color="currentColor" size={13} /> Fact
          </button>
          <span className="k-mat-badge">MATERIALIZED</span>
          <span className="k-mat-title" data-testid="mat-title" title={rootTitle}>{rootTitle}</span>
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
          <span style={{ flex: 1 }} />
          <button type="button" className="k-mat-btn k-mat-primary" data-testid="mat-export" onClick={runExport}
            title={exporting ? 'Cancel the export' : 'Download the whole materialization as markdown'}>
            <DownloadIcon color="currentColor" size={13} />
            {exporting ? (exp.bytes > 0 ? `Exporting ${Math.max(1, Math.round(exp.bytes / 1024))} KB · Cancel` : 'Exporting · Cancel') : 'Export .md'}
          </button>
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
      </div>

      <div className="k-mat-scroll" ref={scroller} data-testid="mat-scroll">
        <article className="k-mat-article" data-stale={stale ? 'true' : 'false'} aria-busy={stale || phase === 'loading'}>
          {notice && (
            <div className="k-mat-notice" data-testid="mat-notice" role="status">
              {notice}
              <button type="button" aria-label="Dismiss" onClick={() => setNotice(null)}>×</button>
            </div>
          )}

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
            <Section key={n.n} node={n} onGoto={onGoto}
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
                      <button type="button" className="k-mat-btn" data-testid="mat-deeper"
                        onClick={() => change({ ...options, depth: options.depth + 1 })}>Go deeper</button>
                    )}
                  </div>
                )}
              </div>
            )}
            <div ref={sentinel} data-testid="mat-sentinel" style={{ height: 1 }} />
          </div>
        </article>
      </div>
    </div>
  );
}
