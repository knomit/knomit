import { useEffect, useState } from 'react';
import type { CSSProperties } from 'react';
import { api } from './api';
import type { Fact } from './api';
import { FactBand } from './FactBand';
import { FactBody } from './FactBody';
import { CHAT_RESULT_LOADING, factRetractedAt } from './chatCopy';
import { NoticeLine } from './ChatNotice';
import { CommitHash } from './TimelineNav';
import { EDGE_ACCENT, EDGE_GLYPH, EDGE_LABEL, relativeTime, sameCommit } from './utils';
import type { EdgeDir } from './utils';

const noDispatch = () => {};

interface Loaded {
  key: string;
  fact?: Fact;
  incoming?: number;
  outgoing?: number;
  /** The fact's version commits, newest first. */
  versions?: string[];
  error?: string;
}

/** The server's sentence, not the request URL fetchJSON puts in front of it. */
export function PanelLoadError({ error, testId }: { error: string; testId: string }) {
  const sentence = /→ \d{3} ([\s\S]+)$/.exec(error)?.[1] ?? error;
  return <div role="alert" style={{ padding: '0 18px' }}><NoticeLine tone="blocked" testId={testId} lead={sentence} /></div>;
}

/**
 * ChatFactView shows one fact in the chat's side panel the way Browse shows it:
 * the same band (FactMetaLine's values, the path, cited by / cites, the version)
 * and the same body. It is read-only: the band's controls that act on Browse's
 * state (filters, panels, history, retract) are not offered here, and "Open in
 * Browse" in the panel header is the way to them.
 *
 * It fetches the fact itself (api.fact), so whoever opens it only names
 * {repo, branch, path}. The edge counts and the version come from separate
 * reads that may fail on their own: the fact is shown without them.
 *
 * With `commit` everything on screen is that version's: the text, the counts,
 * the date and the vN (its place in the history, not the history's length),
 * and the band names the commit. `removed` says the commit retracted the fact:
 * a plain read there is 404 by design, so it reads the last version before it
 * (?fallback=before) and says it was retracted.
 */
export function ChatFactView({ repo, branch, path, commit, removed = false }: { repo: string; branch: string; path: string; commit?: string; removed?: boolean }) {
  const key = `${repo}\0${branch}\0${path}\0${commit ?? ''}`;
  const [loaded, setLoaded] = useState<Loaded | null>(null);
  useEffect(() => {
    let cancelled = false;
    const opts = removed ? { fallback: 'before' as const } : undefined;
    api.fact(repo, branch, path, commit, opts)
      .then(fact => { if (!cancelled) setLoaded(l => ({ ...(l?.key === key ? l : {}), key, fact })); })
      .catch(e => { if (!cancelled) setLoaded({ key, error: e instanceof Error ? e.message : String(e) }); });
    api.explain(repo, branch, path, commit, opts)
      .then(r => { if (!cancelled) setLoaded(l => ({ ...(l?.key === key ? l : { key }), incoming: r.incoming.length, outgoing: r.outgoing.length })); })
      .catch(() => { /* the counts are left out */ });
    api.factCommits(repo, branch, path)
      .then(r => { if (!cancelled) setLoaded(l => ({ ...(l?.key === key ? l : { key }), versions: (r.entries || []).map(e => e.commit) })); })
      .catch(() => { /* the version is left out */ });
    return () => { cancelled = true; };
  }, [repo, branch, path, commit, removed, key]);

  // An answer for another fact than the one asked for is the previous one's.
  const shown = loaded?.key === key ? loaded : null;
  if (!shown?.fact && !shown?.error) {
    return <div data-testid="chat-fact-loading" style={{ color: '#777', fontSize: 13, padding: '14px 18px' }}>{CHAT_RESULT_LOADING}</div>;
  }
  if (shown.error) {
    return <PanelLoadError error={shown.error} testId="chat-fact-error" />;
  }
  const fact = shown.fact!;
  const cell = (dir: EdgeDir, n: number | undefined) => n === undefined ? null : (
    <span key={dir} data-testid={`chat-fact-${dir === 'in' ? 'cited-by' : 'cites'}`} style={edge(n > 0 ? EDGE_ACCENT[dir] : '#444')}>
      <span style={{ fontSize: 10 }}>{EDGE_GLYPH[dir]}</span> {EDGE_LABEL[dir]} <b>{n}</b>
    </span>
  );
  const edges = <>{cell('in', shown.incoming)}{cell('out', shown.outgoing)}</>;
  // The commit this text is from: the one asked for, or, past a retraction, the
  // last version before it (what the server read instead).
  const readAt = commit && (removed ? fact.commit_hash ?? commit : commit);
  const history = shown.versions ?? [];
  const at = readAt ? history.findIndex(c => sameCommit(c, readAt)) : 0;
  const version = at >= 0 ? history.length - at : 0;
  const actions = (
    <span style={{ display: 'flex', alignItems: 'center', gap: 6, fontFamily: 'var(--k-font-mono)', fontSize: 11, color: '#555' }}>
      {readAt && <span data-testid="chat-fact-commit" style={{ display: 'inline-flex' }}><CommitHash commit={readAt} /></span>}
      {fact.commit_date && <span data-testid="chat-fact-date">{relativeTime(fact.commit_date)}</span>}
      {version ? <span data-testid="chat-fact-version" style={{ color: '#7c9' }}>v{version}</span> : null}
    </span>
  );
  return (
    <div data-testid="chat-fact" style={{ display: 'flex', flexDirection: 'column', height: '100%', minHeight: 0 }}>
      <FactBand fact={fact} dispatch={noDispatch} pinned={false} edges={edges} actions={actions} filterable={false} />
      <div style={{ flex: 1, minHeight: 0, overflowY: 'auto', padding: '16px 18px 20px', boxSizing: 'border-box' }}>
        {removed && commit && <div style={{ marginBottom: 12 }}><NoticeLine tone="info" testId="chat-fact-retracted" lead={factRetractedAt(commit.slice(0, 7))} /></div>}
        <div data-testid="chat-fact-title" style={{ fontFamily: 'var(--k-font-display)', fontSize: 17, fontWeight: 600, color: '#eee', marginBottom: 14 }}>
          {fact.title || fact.path}
        </div>
        <FactBody fact={fact} dispatch={noDispatch} filterable={false} />
      </div>
    </div>
  );
}

const edge = (color: string): CSSProperties => ({ color, fontFamily: 'var(--k-font-mono)', fontSize: 11, whiteSpace: 'nowrap' });
