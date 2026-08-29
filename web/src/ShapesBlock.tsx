import { useState } from 'react';
import { api } from './api';
import type { MotifEntry, MotifHealth } from './api';
import { useAsync } from './hooks';
import { MOTIF_GLYPH } from './utils';

/**
 * The motif vocabulary, as a fourth block in the summary panel.
 *
 * NOT A NAVIGATION ENTRY. A nav entry captures a WAY OF LOOKING at the corpus,
 * and this is not one — it is a list of names, and the way of looking it leads
 * to is the pivot, which already exists. It is also per-repository: there is no
 * single vocabulary across a lens, so an entry would have to go dark whenever
 * one was open, and an entry live for two of the four things above it is a
 * feature with a switch on it. A block simply absent from a panel is a far
 * smaller absence than a hole in the navigation.
 *
 * It borrows the three facet columns' mechanics — ranked rows, a share bar, an
 * overflow row that opens the whole thing full-width with a search, and a pick
 * that closes it because picking means going somewhere. What it does NOT borrow
 * is their data: those columns are fed by one statistics payload, and motifs are
 * deliberately not in it (a cluster carries a definition and a member list,
 * which is too heavy to ship with every panel load). This fetches its own —
 * which is also why it can show a meaning under each name and they cannot.
 *
 * THAT DIFFERENCE IS WHY THE THREE STATES ARE HERE. Its sibling columns search a
 * histogram the panel is already holding: instant, and only ever full or empty.
 * This asks the server. So it has a third answer, and a vocabulary that failed
 * to load must never render as a vocabulary with nothing in it — "no names"
 * would claim this corpus has no shared shapes at all.
 */
export function ShapesBlock({ repo, branch, onPick }: {
  repo: string;
  branch: string;
  onPick: (motif: string) => void;
}) {
  const [entries, setEntries] = useState<MotifEntry[]>([]);
  const [health, setHealth] = useState<MotifHealth | null>(null);
  const [count, setCount] = useState(0);
  const [status, setStatus] = useState<'loading' | 'ok' | 'error'>('loading');
  const [expanded, setExpanded] = useState(false);
  const [q, setQ] = useState('');
  const [sort, setSort] = useState<'df' | 'name'>('df');
  const [reloads, setReloads] = useState(0);

  useAsync((stale) => {
    if (!repo || !branch) return;
    setStatus('loading');
    // try/catch around the CALL, not just the promise: a client that throws
    // synchronously would otherwise take the whole summary panel down with it,
    // and this block is a fourth thing on a dashboard whose other three have
    // nothing to do with motifs. A vocabulary that cannot be read is this
    // block's problem to report, not the panel's problem to crash on.
    let pending: ReturnType<typeof api.motifs>;
    try {
      pending = api.motifs(repo, branch, { q: q || undefined, sort, limit: 200 });
    } catch {
      setStatus('error');
      return;
    }
    pending
      .then(r => {
        if (stale()) return;
        setEntries(r.motifs);
        setCount(r.count);
        // The health block is NOT narrowed by the search — it describes the
        // vocabulary, not the list under it — so a narrowing query must not
        // overwrite it with a figure counted over the matches.
        if (!q) setHealth(r.health);
        setStatus('ok');
      })
      .catch(() => { if (!stale()) setStatus('error'); });
  }, [repo, branch, q, sort, reloads]);

  if (!repo) return null;

  // df=1 names are half the vocabulary and none of the reading: a name minted
  // once says something about authoring hygiene and nothing about the corpus's
  // shape. Folded into a band rather than paginated through — N rows with N
  // empty bars is the list telling you about itself instead of the corpus.
  const reused = entries.filter(e => e.df > 1);
  const once = entries.filter(e => e.df <= 1);
  const shown = expanded ? reused : reused.slice(0, TOP_N);
  const max = reused[0]?.df ?? 1;

  return (
    <div data-testid="shapes-block" style={{ marginTop: 18 }}>
      <div style={{ display: 'flex', alignItems: 'baseline', gap: 9, marginBottom: 9 }}>
        <span style={{ fontFamily: 'var(--k-font-mono)', fontSize: 11, color: '#6d7788' }}>{MOTIF_GLYPH}</span>
        <span style={{ fontSize: 10, textTransform: 'uppercase', letterSpacing: 1.5, color: '#8a93a3' }}>
          Shapes
        </span>
        {/* The block count IS narrowed by the search; the health figures beside
            it are not. They sit close together, so each says which it is. */}
        <span data-testid="shapes-count" style={{ fontSize: 10, color: '#5f6a7c', fontFamily: 'var(--k-font-mono)' }}>
          {status === 'ok' ? (q ? `${count} of ${health?.authored_clusters ?? count}` : count) : ''}
        </span>
        {health && (
          <span data-testid="shapes-health" style={{
            marginLeft: 'auto', fontSize: 9.5, color: '#5f6a7c', fontFamily: 'var(--k-font-mono)',
          }} title="Counted over authored facts across the whole vocabulary — never narrowed by the search">
            {health.authored_recurring} reused · {ratio(health)}× each
          </span>
        )}
      </div>

      {expanded && (
        <div style={{ display: 'flex', alignItems: 'center', gap: 9, marginBottom: 9 }}>
          <input
            data-testid="shapes-search"
            value={q}
            onChange={e => setQ(e.target.value)}
            // The wait buys what the sibling boxes cannot do: this searches the
            // MEANINGS as well as the names, so "silent" finds a motif whose
            // name never says it.
            placeholder="Search names and meanings…"
            aria-label="Search names and meanings"
            style={{
              flex: 1, padding: '3px 9px', font: 'inherit', fontSize: 11,
              background: '#14171f', border: '1px solid #262c35', borderRadius: 4,
              color: '#cfd6e2', outline: 'none',
            }}
          />
          <span style={{ display: 'inline-flex', border: '1px solid #2a2a2a', borderRadius: 3, overflow: 'hidden' }}>
            {(['df', 'name'] as const).map(s => (
              <button key={s} type="button" data-testid={`shapes-sort-${s}`}
                onClick={() => setSort(s)}
                style={{
                  fontFamily: 'var(--k-font-mono)', fontSize: 10, padding: '3px 9px',
                  border: 'none', outline: 'none', borderRadius: 0, cursor: 'pointer',
                  background: sort === s ? '#20232a' : 'transparent',
                  color: sort === s ? '#e8eef6' : '#5f6a7c',
                }}>{s === 'df' ? 'most used' : 'a–z'}</button>
            ))}
          </span>
        </div>
      )}

      {status === 'loading' && (
        <div data-testid="shapes-loading" style={{ display: 'flex', flexDirection: 'column', gap: 11, padding: '4px 0' }}>
          {[0, 1, 2].map(i => (
            <div key={i}>
              <div style={{ height: 9, width: `${52 - i * 6}%`, borderRadius: 2, background: '#20242b' }} />
              <div style={{ height: 7, width: `${76 - i * 8}%`, borderRadius: 2, background: '#191c22', marginTop: 6 }} />
            </div>
          ))}
        </div>
      )}

      {status === 'error' && (
        // Not an empty list, and not a zero. A failure rendering as "no names"
        // would say this corpus has no shared shapes — the most misleading
        // thing this surface could claim.
        <div data-testid="shapes-error" style={{ display: 'flex', flexDirection: 'column', gap: 8, padding: '4px 0' }}>
          <span style={{ fontSize: 11.5, color: '#e0a0a0' }}>The vocabulary could not be read.</span>
          <span>
            <button type="button" data-testid="shapes-retry" onClick={() => setReloads(n => n + 1)}
              style={{
                fontFamily: 'var(--k-font-mono)', fontSize: 11, color: '#dbe2ec',
                background: 'none', border: '1px solid #3a4150', borderRadius: 3,
                padding: '3px 10px', cursor: 'pointer', outline: 'none',
              }}>Try again</button>
          </span>
        </div>
      )}

      {status === 'ok' && shown.length === 0 && once.length === 0 && (
        <div data-testid="shapes-empty" style={{ fontSize: 11.5, color: '#5a6675', padding: '4px 0' }}>
          {q ? `No name or meaning matches “${q}”.` : 'No shapes named yet.'}
        </div>
      )}

      {status === 'ok' && shown.map(e => (
        <div key={e.cluster_key} data-testid="shapes-row" data-motif={e.canonical}
          onClick={() => onPick(e.canonical)}
          title={`Open the facts that share this shape — ${e.df} of them`}
          style={{ padding: '4px 0', cursor: 'pointer' }}>
          <div style={{ display: 'flex', alignItems: 'baseline', gap: 10 }}>
            <span style={{ fontFamily: 'var(--k-font-mono)', fontSize: 11.5, color: '#c8cfdb' }}>{e.canonical}</span>
            <span aria-hidden style={{ flex: 1, height: 1, background: '#1e222a', position: 'relative', top: -3 }}>
              <span style={{
                position: 'absolute', left: 0, top: 0, height: 1, borderRadius: 1,
                width: `${Math.max(4, (e.df / max) * 100)}%`, background: '#4a5262',
              }} />
            </span>
            {/* df, the VOCABULARY count — a share of the leader, not the promise
                a pivot makes. That number is carrier_count and lives on the
                fact header and the pivot heading. */}
            <span style={{
              fontFamily: 'var(--k-font-mono)', fontSize: 10, color: '#6d7788',
              fontVariantNumeric: 'tabular-nums',
            }}>{e.df}</span>
          </div>
          {expanded && e.definition && (
            <div style={{
              fontSize: 11, lineHeight: 1.5, color: '#8a93a3', marginTop: 3,
              overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
            }}>{e.definition}</div>
          )}
        </div>
      ))}

      {status === 'ok' && !expanded && reused.length > TOP_N && (
        <div data-testid="shapes-more" onClick={() => setExpanded(true)}
          style={{ fontSize: 10.5, color: '#5f6a7c', padding: '4px 0', cursor: 'pointer' }}>
          +{reused.length - TOP_N} more
        </div>
      )}

      {status === 'ok' && expanded && once.length > 0 && (
        <div data-testid="shapes-singletons" style={{ marginTop: 10, paddingTop: 9, borderTop: '1px solid #1a1e24' }}>
          <div style={{ display: 'flex', alignItems: 'baseline', gap: 8, fontSize: 11, color: '#8a93a3' }}>
            <span>{once.length} names used once</span>
            <span style={{ color: '#5a6675' }}>— minted, not yet reused</span>
          </div>
          <div style={{
            fontFamily: 'var(--k-font-mono)', fontSize: 9.5, lineHeight: 1.85,
            color: '#4d5665', marginTop: 6,
          }}>{once.map(e => e.canonical).join(' · ')}</div>
        </div>
      )}
    </div>
  );
}

/** Rows before the overflow row, matching the facet columns beside this. */
const TOP_N = 6;

/** Reuses per name — the figure that says whether the vocabulary is alive
 *  (names being reused) or noise (every fact minting its own). */
function ratio(h: MotifHealth): string {
  if (!h.authored_mints) return '0';
  return (h.authored_links / h.authored_mints).toFixed(1);
}
