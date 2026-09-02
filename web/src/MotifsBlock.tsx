import { useEffect, useRef, useState } from 'react';
import type { MotifEntry, MotifHealth } from './api';
import type { MotifEndpoint } from './motifEndpoint';
import { canReadMotifs, motifEndpointKey, readMotifs } from './motifEndpoint';
import { useAsync } from './hooks';
import { MOTIF_GLYPH } from './utils';

/**
 * The motif vocabulary, as a fourth block in the summary panel.
 *
 * NOT A NAVIGATION ENTRY. A nav entry captures a WAY OF LOOKING at the corpus,
 * and this is not one — it is a list of names, and the way of looking it leads
 * to is the pivot, which already exists. A block simply absent from a panel is
 * a far smaller absence than a hole in the navigation, and this block is
 * absent in builds with no vocabulary endpoint to ask.
 *
 * It renders in a lens as readily as in a repo. The v1 exclusion — "there is
 * no single vocabulary across a lens" — was true of the SERVER at the time and
 * is not true now: /lenses/{lens}/motifs merges every mount's clusters into
 * one list in this same envelope, so the block reads a lens's vocabulary
 * through `endpoint` without knowing which kind it holds.
 *
 * It borrows the three facet columns' mechanics — ranked rows, a share bar, an
 * overflow row that opens the whole thing full-width with a search and a way
 * BACK, and a pick that closes it because picking means going somewhere. The
 * overflow behaviour is borrowed to the letter, and that is the point: `+N more`
 * that expanded in place here and opened a browser one block up would make the
 * same words mean two different things a few pixels apart.
 *
 * What it does NOT borrow is their data: those columns are fed by one statistics
 * payload, and motifs are deliberately not in it (a cluster carries a definition
 * and a member list, which is too heavy to ship with every panel load). This
 * fetches its own — which is also why it can show a meaning under each name and
 * they cannot.
 *
 * THAT DIFFERENCE IS WHY THE THREE STATES ARE HERE. Its sibling columns search a
 * histogram the panel is already holding: instant, and only ever full or empty.
 * This asks the server. So it has a third answer, and a vocabulary that failed
 * to load must never render as a vocabulary with nothing in it — "no names"
 * would claim this corpus has no shared motifs at all.
 */
export function MotifsBlock({ endpoint, path, onPick }: {
  /** Which surface to read from — a repo branch or a lens. The two answer with
   *  the same shape, so nothing below this prop branches on it. */
  endpoint: MotifEndpoint;
  /** The ontology path the panel is describing. SCOPE, not a filter: the block
   *  lists the vocabulary of the folder the reader is standing in, exactly as
   *  the facet columns beside it list that folder's domains and entities. A
   *  motif no fact here carries is absent — offering one would offer a pivot
   *  away from everything else on screen. */
  path: string;
  onPick: (motif: string) => void;
}) {
  const [entries, setEntries] = useState<MotifEntry[]>([]);
  const [health, setHealth] = useState<MotifHealth | null>(null);
  const [count, setCount] = useState(0);
  /** The count with NO search applied — the denominator of "N of M".
   *
   *  It used to be health.authored_clusters, which is a different population
   *  from the rows twice over: health is counted over AUTHORED facts while the
   *  list spans every origin, and in a lens health is a per-mount SUM while the
   *  list counts each merged cluster once. "12 of 73" then compared a number of
   *  merged clusters against a number of per-mount cluster instances. The
   *  unnarrowed count is the same query as the rows with the search taken off,
   *  which is exactly what the phrase claims. */
  const [total, setTotal] = useState<number | null>(null);
  const [status, setStatus] = useState<'loading' | 'ok' | 'error'>('loading');
  const [browsing, setBrowsing] = useState(false);
  const [q, setQ] = useState('');
  const [sort, setSort] = useState<'df' | 'name'>('df');
  const [reloads, setReloads] = useState(0);
  const searchRef = useRef<HTMLInputElement>(null);
  // The scope the held health figures were counted over — see the guard below.
  const healthFor = useRef('');

  // The search takes focus when the browser opens — the reader clicked `+N
  // more` to look for something, and the sibling facet browser does the same.
  useEffect(() => { if (browsing) searchRef.current?.focus(); }, [browsing]);

  const endpointKey = motifEndpointKey(endpoint);
  const readable = canReadMotifs(endpoint);

  useAsync((stale) => {
    if (!readable) return;
    setStatus('loading');
    // try/catch around the CALL and the subscription, not just the promise's
    // rejection: a client that throws synchronously — or hands back something
    // that is not a promise at all — would otherwise take the whole summary
    // panel down with it, and this block is a fourth thing on a dashboard
    // whose other three have nothing to do with motifs. A vocabulary that
    // cannot be read is this block's problem to report, not the panel's
    // problem to crash on.
    try {
      readMotifs(endpoint, { q: q || undefined, path: path || undefined, sort, limit: 200 })
        .then(r => {
          if (stale()) return;
          setEntries(r.motifs);
          setCount(r.count);
          // The health block is NOT narrowed by the search — it describes the
          // vocabulary, not the list under it — so a narrowing query must not
          // overwrite it with a figure counted over the matches. The PATH is a
          // different thing and does reach it: the server counts health over the
          // same subtree it counted the list over, so the strip and the rows are
          // always about one population. Which is why the held figures are KEYED
          // by scope: a folder click mid-search refetches the list but leaves
          // the query set, and a bare `if (!q)` would hold the old folder's
          // figures over the new folder's rows — the exact two-populations row
          // the pairing rule forbids. Better no strip than that strip; clearing
          // the search refills it.
          const scope = `${endpointKey}\n${path}`;
          // `total` is held under the SAME scope rule as health, and for the
          // same reason: a folder click mid-search refetches the list but
          // leaves the query set, so a denominator counted over the previous
          // folder would sit over the new folder's rows. Better no "of M".
          if (!q) { setHealth(r.health); setTotal(r.count); healthFor.current = scope; }
          else if (healthFor.current !== scope) { setHealth(null); setTotal(null); }
          setStatus('ok');
        })
        .catch(() => { if (!stale()) setStatus('error'); });
    } catch {
      setStatus('error');
    }
    // `endpointKey`, not `endpoint`: the object is rebuilt on every render of
    // the panel, and depending on it would re-issue the request each time.
  }, [endpointKey, path, q, sort, reloads]);

  // ABSENT, not broken, where there is no vocabulary endpoint to ask.
  //
  // The public /explore build vendors these components and swaps api.ts for a
  // static bundle; browsing a vocabulary needs a live endpoint that bundle does
  // not carry. "Unavailable" is a third thing from "failed" and "empty" — an
  // error message there would report a fault in a build that is working
  // exactly as intended — so the block simply is not there, and the panel has
  // three columns instead of three columns and a block. The pivot still works,
  // because a pivot is a filter.
  //
  // The same absence covers an endpoint that is not addressable yet: App picks
  // a repo from /repos on mount, so the first render has no repo to ask about.
  if (!readable) return null;

  // Names used once are half the vocabulary and none of the reading: a name
  // minted once says something about authoring hygiene and nothing about the
  // corpus's shape. Folded into a band rather than paginated through — N rows
  // with N empty bars is the list telling you about itself instead of the
  // corpus.
  //
  // THE FOLD IS BY THE BRANCH-WIDE COUNT, never the scoped one. Under a path,
  // `df` of 1 means "one fact HERE carries it", which is the ordinary case in
  // any small folder and says nothing at all about hygiene — folding on it
  // would hide a motif carried by twenty-six facts because this folder holds
  // one of them. `df_total` is the number the fold was designed around, and on
  // an unscoped read it IS `df`.
  const branchWide = (e: MotifEntry) => e.df_total ?? e.df;
  const reused = entries.filter(e => branchWide(e) > 1);
  const once = entries.filter(e => branchWide(e) <= 1);
  const shown = browsing ? reused : reused.slice(0, TOP_N);
  const max = reused[0]?.df ?? 1;

  // The band folds ONLY where the overflow row can open it again. Nothing this
  // block holds may be unreachable, and the two are the same condition: with
  // six ranked names or fewer there is no `+N more`, so a folded band would be
  // content with no door — at the extreme, a folder whose every motif is used
  // once renders "Motifs 1" over empty space. A path scope makes that the
  // ordinary case; a whole-repo view never met it, which is why the fold could
  // be unconditional before.
  const bandInline = once.length > 0 && reused.length <= TOP_N;

  // Leaving takes the browser's state with it. The search and the a–z ordering
  // belong to the browser; a collapsed summary rendered under either is six
  // rows claiming to be the top six when they are not — sliced from an
  // alphabetical list, with `max` above no longer the maximum and the share
  // bars scaled past their track.
  const close = () => { setBrowsing(false); setQ(''); setSort('df'); };

  // Picking CLOSES the browser, exactly as picking a facet value does. A pivot
  // is a move: the list refetches, the first fact opens, and this panel is not
  // what the reader is looking at any more — so coming back to a panel still
  // holding an open browser would be the app remembering a gesture that
  // finished. Adding a second motif means opening it again. Same door out as
  // ← Back, so the same state leaves with it.
  const pick = (motif: string) => { close(); onPick(motif); };

  return (
    <div data-testid="motifs-block" style={{ marginTop: 18 }}
      onKeyDown={browsing ? (e => { if (e.key === 'Escape') { e.stopPropagation(); close(); } }) : undefined}>
      <div style={{ display: 'flex', alignItems: 'baseline', gap: 9, marginBottom: 9 }}>
        {browsing ? (
          <button type="button" data-testid="motifs-back" onClick={close}
            style={{
              background: 'none', border: 0, padding: '2px 6px 2px 0', cursor: 'pointer',
              font: 'inherit', fontSize: 11, color: '#7a8593',
            }}>← Back</button>
        ) : (
          <span style={{ fontFamily: 'var(--k-font-mono)', fontSize: 11, color: '#6d7788' }}>{MOTIF_GLYPH}</span>
        )}
        <span style={{ fontSize: 10, textTransform: 'uppercase', letterSpacing: 1.5, color: '#8a93a3' }}>
          Motifs
        </span>
        {/* The block count IS narrowed by the search; the health figures beside
            it are not. They sit close together, so each says which it is — and
            the "of M" is the unnarrowed COUNT, never a health figure, so both
            halves of the fraction describe the same population. With no
            denominator held for THIS scope the fraction is dropped rather than
            defaulted to the numerator — "3 of 3" would claim the search matched
            everything. */}
        <span data-testid="motifs-count" style={{ fontSize: 10, color: '#5f6a7c', fontFamily: 'var(--k-font-mono)' }}>
          {status !== 'ok' ? '' : q && total !== null ? `${count} of ${total}` : count}
        </span>
        {health && (
          <span data-testid="motifs-health" style={{
            marginLeft: 'auto', fontSize: 9.5, color: '#5f6a7c', fontFamily: 'var(--k-font-mono)',
          }} title={path
            ? `Counted over the authored facts under ${path} — the same facts as the list, never narrowed by the search`
            : 'Counted over authored facts across the whole vocabulary — never narrowed by the search'}>
            {health.authored_recurring} reused · {ratio(health)}× each
          </span>
        )}
      </div>

      {browsing && (
        <div style={{ display: 'flex', alignItems: 'center', gap: 9, marginBottom: 9 }}>
          <input
            data-testid="motifs-search"
            value={q}
            onChange={e => setQ(e.target.value)}
            // The wait buys what the sibling boxes cannot do: this searches the
            // MEANINGS as well as the names, so "silent" finds a motif whose
            // name never says it.
            placeholder="Search names and meanings…"
            aria-label="Search names and meanings"
            ref={searchRef}
            style={{
              flex: 1, padding: '3px 9px', font: 'inherit', fontSize: 11,
              background: '#14171f', border: '1px solid #262c35', borderRadius: 4,
              color: '#cfd6e2', outline: 'none',
            }}
          />
          <span style={{ display: 'inline-flex', border: '1px solid #2a2a2a', borderRadius: 3, overflow: 'hidden' }}>
            {(['df', 'name'] as const).map(s => (
              <button key={s} type="button" data-testid={`motifs-sort-${s}`}
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
        <div data-testid="motifs-loading" style={{ display: 'flex', flexDirection: 'column', gap: 11, padding: '4px 0' }}>
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
        // would say this corpus has no shared motifs — the most misleading
        // thing this surface could claim.
        <div data-testid="motifs-error" style={{ display: 'flex', flexDirection: 'column', gap: 8, padding: '4px 0' }}>
          <span style={{ fontSize: 11.5, color: '#e0a0a0' }}>The vocabulary could not be read.</span>
          <span>
            <button type="button" data-testid="motifs-retry" onClick={() => setReloads(n => n + 1)}
              style={{
                fontFamily: 'var(--k-font-mono)', fontSize: 11, color: '#dbe2ec',
                background: 'none', border: '1px solid #3a4150', borderRadius: 3,
                padding: '3px 10px', cursor: 'pointer', outline: 'none',
              }}>Try again</button>
          </span>
        </div>
      )}

      {status === 'ok' && shown.length === 0 && once.length === 0 && (
        <div data-testid="motifs-empty" style={{ fontSize: 11.5, color: '#5a6675', padding: '4px 0' }}>
          {q ? `No name or meaning matches “${q}”.`
             : path ? 'No motifs named in these facts.'
             : 'No motifs named yet.'}
        </div>
      )}

      {/* The browser SCROLLS inside a fixed height rather than growing the
          panel, for the reason FacetBrowser does: opening it must not shove
          Highlights — the thing the panel exists to show — down the page, and
          the clipped last row is the only honest signal that the list goes on.
          Fixed, not max-height, so narrowing the search does not make the panel
          bounce. */}
      <div style={browsing
        ? { height: 232, overflowY: 'auto', paddingRight: 4 }
        : undefined}>
        {status === 'ok' && shown.map(e => {
          // The number shown is the one this scope holds — the same rule the
          // facet columns follow. But the pivot this row opens DROPS the path
          // (a motif cuts across the ontology), so where the branch holds more
          // than the folder does, the title says both rather than letting the
          // reader discover the widening by landing in it.
          const total = e.df_total ?? e.df;
          return (
            <div key={e.cluster_key} data-testid="motifs-row" data-motif={e.canonical}
              onClick={() => pick(e.canonical)}
              title={total > e.df
                ? `Open the facts that share this motif — ${e.df} here, ${total} in the repo`
                : `Open the facts that share this motif — ${e.df} of them`}
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
              {browsing && e.definition && (
                <div data-testid="motifs-definition" style={{
                  // WRAPS. This is the sentence the reader opened the browser to
                  // read, and it is a claim whose point is usually in its second
                  // half — "…so callers and monitoring record it as having worked"
                  // is the whole motif. Clipping it mid-clause is the same failure
                  // that banned the ellipsis on motif names, one level up: what
                  // survives the cut reads as a complete and different statement.
                  fontSize: 11, lineHeight: 1.5, color: '#8a93a3', marginTop: 3,
                }}>{e.definition}</div>
              )}
            </div>
          );
        })}

        {status === 'ok' && (browsing || bandInline) && once.length > 0 && (
          // The rule above the band separates it from the ranked names. With no
          // ranked names above it there is nothing to separate, and a hairline
          // straight under the heading reads as a second heading.
          <div data-testid="motifs-singletons" style={shown.length > 0
            ? { marginTop: 10, paddingTop: 9, borderTop: '1px solid #1a1e24' }
            : { paddingTop: 2 }}>
            <div style={{ display: 'flex', alignItems: 'baseline', gap: 8, fontSize: 11, color: '#8a93a3' }}>
              <span>{once.length} {once.length === 1 ? 'name' : 'names'} used once</span>
              <span style={{ color: '#5a6675' }}>— minted, not yet reused</span>
            </div>
            <div style={{
              fontFamily: 'var(--k-font-mono)', fontSize: 9.5, lineHeight: 1.85,
              color: '#4d5665', marginTop: 6,
            }}>{once.map(e => e.canonical).join(' · ')}</div>
          </div>
        )}
      </div>

      {status === 'ok' && !browsing && reused.length > TOP_N && (
        <div data-testid="motifs-more" onClick={() => setBrowsing(true)}
          style={{ fontSize: 10.5, color: '#5f6a7c', padding: '4px 0', cursor: 'pointer' }}>
          +{reused.length - TOP_N} more
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
