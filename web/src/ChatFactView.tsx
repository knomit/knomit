import { useEffect, useState } from 'react';
import type { CSSProperties } from 'react';
import { api } from './api';
import type { Fact } from './api';
import { FactBand } from './FactBand';
import { FactBody } from './FactBody';
import { CHAT_RESULT_LOADING } from './chatCopy';
import { NoticeLine } from './ChatNotice';
import { EDGE_ACCENT, EDGE_GLYPH, EDGE_LABEL, relativeTime } from './utils';
import type { EdgeDir } from './utils';

const noDispatch = () => {};

interface Loaded {
  key: string;
  fact?: Fact;
  incoming?: number;
  outgoing?: number;
  versions?: number;
  error?: string;
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
 */
export function ChatFactView({ repo, branch, path, commit }: { repo: string; branch: string; path: string; commit?: string }) {
  const key = `${repo}\0${branch}\0${path}\0${commit ?? ''}`;
  const [loaded, setLoaded] = useState<Loaded | null>(null);
  useEffect(() => {
    let cancelled = false;
    api.fact(repo, branch, path, commit)
      .then(fact => { if (!cancelled) setLoaded(l => ({ ...(l?.key === key ? l : {}), key, fact })); })
      .catch(e => { if (!cancelled) setLoaded({ key, error: e instanceof Error ? e.message : String(e) }); });
    api.explain(repo, branch, path, commit)
      .then(r => { if (!cancelled) setLoaded(l => ({ ...(l?.key === key ? l : { key }), incoming: r.incoming.length, outgoing: r.outgoing.length })); })
      .catch(() => { /* the counts are left out */ });
    api.factCommits(repo, branch, path)
      .then(r => { if (!cancelled) setLoaded(l => ({ ...(l?.key === key ? l : { key }), versions: (r.entries || []).length })); })
      .catch(() => { /* the version is left out */ });
    return () => { cancelled = true; };
  }, [repo, branch, path, commit, key]);

  // An answer for another fact than the one asked for is the previous one's.
  const shown = loaded?.key === key ? loaded : null;
  if (!shown?.fact && !shown?.error) {
    return <div data-testid="chat-fact-loading" style={{ color: '#777', fontSize: 13, padding: '14px 18px' }}>{CHAT_RESULT_LOADING}</div>;
  }
  if (shown.error) {
    // The server's sentence, not the request URL fetchJSON puts in front of it.
    const sentence = /→ \d{3} ([\s\S]+)$/.exec(shown.error)?.[1] ?? shown.error;
    return <div role="alert" style={{ padding: '0 18px' }}><NoticeLine tone="blocked" testId="chat-fact-error" lead={sentence} /></div>;
  }
  const fact = shown.fact!;
  const cell = (dir: EdgeDir, n: number | undefined) => n === undefined ? null : (
    <span key={dir} data-testid={`chat-fact-${dir === 'in' ? 'cited-by' : 'cites'}`} style={edge(n > 0 ? EDGE_ACCENT[dir] : '#444')}>
      <span style={{ fontSize: 10 }}>{EDGE_GLYPH[dir]}</span> {EDGE_LABEL[dir]} <b>{n}</b>
    </span>
  );
  const edges = <>{cell('in', shown.incoming)}{cell('out', shown.outgoing)}</>;
  const actions = (
    <span style={{ display: 'flex', alignItems: 'center', gap: 6, fontFamily: 'var(--k-font-mono)', fontSize: 11, color: '#555' }}>
      {fact.commit_date && <span data-testid="chat-fact-date">{relativeTime(fact.commit_date)}</span>}
      {shown.versions ? <span data-testid="chat-fact-version" style={{ color: '#7c9' }}>v{shown.versions}</span> : null}
    </span>
  );
  return (
    <div data-testid="chat-fact" style={{ display: 'flex', flexDirection: 'column', height: '100%', minHeight: 0 }}>
      <FactBand fact={fact} dispatch={noDispatch} pinned={false} edges={edges} actions={actions} filterable={false} />
      <div style={{ flex: 1, minHeight: 0, overflowY: 'auto', padding: '16px 18px 20px', boxSizing: 'border-box' }}>
        <div data-testid="chat-fact-title" style={{ fontFamily: 'var(--k-font-display)', fontSize: 17, fontWeight: 600, color: '#eee', marginBottom: 14 }}>
          {fact.title || fact.path}
        </div>
        <FactBody fact={fact} dispatch={noDispatch} filterable={false} />
      </div>
    </div>
  );
}

const edge = (color: string): CSSProperties => ({ color, fontFamily: 'var(--k-font-mono)', fontSize: 11, whiteSpace: 'nowrap' });
