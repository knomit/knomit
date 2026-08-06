import { useState } from 'react';
import type { Dispatch, ReactNode } from 'react';
import ReactMarkdown from 'react-markdown';
import type { Fact, FactRef } from './api';
import type { Action } from './state';
import { markdownPlugins, markdownComponents } from './markdown';
import { CopyIcon } from './icons';

// The hop resolves against the repo-relative path (see qualifyHopTarget in
// useTimeTravel.ts), which the server sends as `path` — deciding that a
// canonical kb://<id>/… ref and its bare equivalent name the same fact is
// ClassifyRef's job, and re-deriving it from `raw` here would be a second
// implementation of the rule in the one language the guard test cannot see.
// `raw` stays what the author wrote, for display. The fallback covers only an
// older server that sends no `path`.
function refTarget(r: FactRef): string {
  return r.path ?? r.raw;
}

// How much of a 40-hex git hash survives into the LABEL. 8 is git's own
// short-hash width: enough to recognise a commit, useless for fetching one —
// which is the point, since the stored ref keeps the full 40 and that is what a
// reader copies out of the title attribute.
const HASH_LABEL_LEN = 8;

// Splits a repo-qualified ref into the parts a LABEL needs.
//
// THE LINE THIS MUST NOT CROSS. Ref CLASSIFICATION — what a ref is, whether its
// target exists at the viewed commit, whether it is clickable — belongs to the
// server via fact.ClassifyRef, and nothing here may influence it. This regex is
// applied only INSIDE an already-decided `source_code` / `foreign` case, never
// to choose one, and a ref that fails to match falls back to its raw string.
// The worst a wrong match can do is render an ugly label; it cannot make a ref
// clickable, hoppable, or resolvable. That is the whole difference between this
// and the scheme regex the References switch below exists to keep out.
//
// Anchored and total by construction: scheme, then `[^/]+` for the repo
// segment, then a lazy path up to an optional `@commit[:blob]` and an optional
// `#L…` range.
const QUALIFIED_REF = /^(kb|src):\/\/([^/]+)\/(.+?)(?:@([0-9a-f]+)(?::([0-9a-f]+))?)?(#L.+)?$/i;

function abbrevHash(h: string): string {
  return h.length > HASH_LABEL_LEN ? `${h.slice(0, HASH_LABEL_LEN)}…` : h;
}

interface RefParts {
  scheme: string;
  repoID: string;
  path: string;
  commit?: string;
  blob?: string;
  lines?: string;
}

function parseQualifiedRef(raw: string): RefParts | null {
  const m = QUALIFIED_REF.exec(raw);
  if (!m) return null;
  const [, scheme, repoID, path, commit, blob, lines] = m;
  return { scheme, repoID, path, commit, blob, lines };
}

/**
 * The visible text for a repo-qualified ref: the same citation with its hashes
 * abbreviated, and — only when the id names a repo this instance has mounted —
 * the id replaced by that repo's name.
 *
 * WHY THE NAME RESOLVES FOR kb:// AND ESSENTIALLY NEVER FOR src://, which is
 * correct rather than a gap to fix here. `repoNames` is keyed by KB-STORE ids:
 * the root commit of a knowledge-base git store, which is exactly what
 * `kb://<id>/…` carries, so a foreign fact ref in another mounted repo resolves
 * to that repo's name. A `src://` ref carries the SOURCE CODE repo's root
 * commit — a different namespace. knomit stores no source objects and keeps no
 * registry of source repos (internal/okf/concept.go refuses to guess one,
 * because a KB repo is usually not the repo it documents), so nothing on either
 * side of the wire can name one. Those ids are left alone.
 *
 * Returns raw unchanged for anything that does not parse, so a ref shape this
 * regex has not met renders exactly as it always did.
 */
function refLabel(r: FactRef, repoNames: Record<string, string>): string {
  const p = parseQualifiedRef(r.raw);
  if (!p) return r.raw;
  const repo = repoNames[p.repoID.toLowerCase()] ?? p.repoID;
  const version = p.commit
    ? `@${abbrevHash(p.commit)}${p.blob ? `:${abbrevHash(p.blob)}` : ''}`
    : '';
  return `${p.scheme}://${repo}/${p.path}${version}${p.lines ?? ''}`;
}

/**
 * The git command that retrieves what a source citation points at, or null when
 * the ref carries nothing to run one against.
 *
 * The command varies because the ref does, and offering an unrunnable one is
 * worse than offering none:
 *   - blob present (the current src form) → `git cat-file blob <40-hex>`, which
 *     resolves even if the file was later renamed or deleted. That durability is
 *     why the blob is stored at all.
 *   - legacy form, commit but no blob → `git show <commit>:<path>`. Weaker on
 *     purpose: it FAILS if the path did not exist at that commit, which is
 *     exactly the failure mode the blob form removes.
 *   - neither → null.
 *
 * All of them assume the reader is in a checkout of the cited repo. knomit
 * stores no source objects and cannot resolve one for them.
 */
function sourceRefCommand(raw: string): string | null {
  const p = parseQualifiedRef(raw);
  if (p?.blob) return `git cat-file blob ${p.blob}`;
  if (p?.commit) return `git show ${p.commit}:${p.path}`;
  return null;
}

// Hover text for a source citation: the raw ref, then the command, spelled out
// with the real hash rather than a `<blob>` placeholder the reader would have to
// fill in by hand. The command is ALSO on the copy button beside the row —
// a native title is browser chrome and its text cannot be selected, which is the
// whole reason that button exists.
function sourceRefTitle(raw: string): string {
  const cmd = sourceRefCommand(raw);
  if (!cmd) return `${raw}\n\nSource citation.`;
  const caveat = cmd.startsWith('git show')
    ? '\n\n(legacy citation — no blob recorded; this fails if the path did not exist at that commit)'
    : '';
  return `${raw}\n\nRetrieve the exact bytes with:\n${cmd}${caveat}`;
}

/**
 * One `source_code` row: the label, and a button that copies the retrieval
 * command.
 *
 * WHY A BUTTON AT ALL. The command lived only in the `title` attribute, which
 * the browser renders as OS-level chrome — you can read it and not select it, so
 * a 40-hex hash had to be retyped. Copying is the only thing anyone does with
 * that line.
 *
 * The row itself stays INERT. Clicking the ref does nothing, exactly as before:
 * a src:// citation is not a fact path, and the invariant in
 * kb/invariants/ui/factbody/ref-scheme-branching exists because it was once fed
 * to onRefClick. Copying is not navigation, and the button is a separate target
 * with its own handler — clicking the ref text still does nothing at all.
 *
 * Own component because `copied` is per-row state and FactBody is a plain
 * function; the 1.5s reset mirrors RepoManager's ConnectPanel.
 */
function SourceRefRow({ raw, label }: { raw: string; label: string }) {
  const [copied, setCopied] = useState(false);
  const cmd = sourceRefCommand(raw);

  const copy = () => {
    if (!cmd) return;
    navigator.clipboard.writeText(cmd)
      .then(() => { setCopied(true); setTimeout(() => setCopied(false), 1500); })
      .catch(() => { /* clipboard unavailable — nothing to recover */ });
  };

  return (
    <span data-testid="source-ref" style={sourceRow} title={sourceRefTitle(raw)}>
      <span>{'→'} {label}</span>
      {cmd && (
        <button
          type="button"
          data-testid="source-ref-copy"
          // The command itself, not "Copy": this sits among several refs, and
          // the useful question a reader has is WHICH command they are taking.
          title={copied ? 'Copied' : cmd}
          aria-label={`Copy retrieval command: ${cmd}`}
          onClick={copy}
          style={copyBtn}
          onMouseEnter={e => { e.currentTarget.style.opacity = '1'; }}
          onMouseLeave={e => { e.currentTarget.style.opacity = copied ? '1' : '0.45'; }}
        >
          <CopyIcon color={copied ? '#7c9' : '#888'} size={12} />
        </button>
      )}
    </span>
  );
}

const sourceRow: React.CSSProperties = {
  color: '#666', fontSize: 12, fontFamily: 'var(--k-font-mono)',
  display: 'inline-flex', alignItems: 'center', gap: 6,
};

const copyBtn: React.CSSProperties = {
  background: 'none', border: 'none', padding: 0, cursor: 'pointer',
  display: 'inline-flex', alignItems: 'center', opacity: 0.45,
  transition: 'opacity 0.15s', flexShrink: 0,
};

interface Props {
  fact: Fact;
  dispatch: Dispatch<Action>;
  onRefClick?: (refPath: string) => void;
  /**
   * 12-hex KB-store id → mounted repo name, for the References labels. Built
   * from /api/v1/repos, which already carries both. Optional and defaulted to
   * empty: an unknown id keeps its id, which is also what an older server that
   * omits `id` produces.
   */
  repoNames?: Record<string, string>;
}

const NO_REPO_NAMES: Record<string, string> = {};

// What each server-side ref kind is called on screen. The words are the
// reader's, not the wire's: `source_code` is "source", and `foreign` — which
// means a fact in a knomit repo this lens does not mount — is "another repo".
// Only the kinds a reader cannot infer from the ref itself.
const REF_KIND_LABEL: Record<string, string> = {
  foreign: 'another repo',
  broken: 'unresolved',
};

const propKey: React.CSSProperties = {
  fontSize: 9.5, textTransform: 'uppercase', letterSpacing: '.11em',
  textAlign: 'right', paddingTop: 2,
};


// RefList renders a fact's references. Extracted from an inline IIFE inside
// FactBody: it is the only child of the metadata table that is not a flat list
// of words, and it needs its own name to sit in a grid cell.
function RefList({ refs, onRefClick, repoNames }: {
  refs: FactRef[];
  onRefClick?: (path: string) => void;
  repoNames: Record<string, string>;
}) {
  const mono = { fontSize: 12, fontFamily: 'var(--k-font-mono)' } as const;
  const hoverIn = (e: React.MouseEvent) => { (e.currentTarget as HTMLElement).style.color = '#adf'; };
  const hoverOut = (e: React.MouseEvent) => { (e.currentTarget as HTMLElement).style.color = '#8af'; };
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 4, minWidth: 0 }}>

            {/*
              Rendered from the server's `kind`, never from a regex over `raw`.
              The rule that used to live here — schemeless means clickable —
              could not know whether a target existed, could not tell a
              foreign repo from a typo, and made any schemeless string a live
              link. `kind` also encodes existence AT THE VIEWED COMMIT, which
              only the server can determine.
            */}
            {refs.map(r => (
              // A marker only where the ref itself cannot say it. "link" over
              // an https:// URL and "fact" over a kb/ path restate the string
              // beside them, and that gutter of repeated words was the widest
              // empty space on the panel. The two that remain say something the
              // text cannot: this target is not openable, and why.
              <span key={r.raw} style={{ display: 'flex', alignItems: 'center', gap: 10, minWidth: 0 }}>
                <span style={{ minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                  {renderRef(r)}
                </span>
                {REF_KIND_LABEL[r.kind] && (
                  <span style={{
                    marginLeft: 'auto', flex: 'none', fontSize: 9.5, letterSpacing: '.06em',
                    color: r.kind === 'broken' ? '#a66' : '#4f5765',
                  }}>{REF_KIND_LABEL[r.kind]}</span>
                )}
              </span>
            ))}
    </div>
  );

  function renderRef(r: FactRef) {
            switch (r.kind) {
              case 'url':
                // Only http(s) is openable; file:// and other schemes are inert.
                return /^https?:\/\//i.test(r.raw) ? (
                  <a key={r.raw} href={r.raw} target="_blank" rel="noopener noreferrer"
                    style={{ color: '#8af', fontSize: 12, textDecoration: 'none', transition: 'color 0.15s' }}
                    onMouseEnter={hoverIn} onMouseLeave={hoverOut}
                  >{'↗'} {r.raw}</a>
                ) : (
                  <span key={r.raw} style={{ color: '#666', ...mono }}>{'→'} {r.raw}</span>
                );

              case 'fact':
                // The server already confirmed this resolves at the version
                // being viewed; onRefClick hands it to the commit-anchored hop.
                return onRefClick ? (
                  <span key={r.raw} onClick={() => onRefClick(refTarget(r))}
                    style={{ color: '#8af', cursor: 'pointer', transition: 'color 0.15s', ...mono }}
                    onMouseEnter={hoverIn} onMouseLeave={hoverOut}
                  >{'→'} {refTarget(r)}</span>
                ) : (
                  <span key={r.raw} style={{ color: '#666', ...mono }}>{'→'} {refTarget(r)}</span>
                );

              case 'broken':
                return (
                  <span key={r.raw} style={{ color: '#a66', ...mono }}
                    title="No fact at this path in the version being viewed">
                    {'⚠'} {r.raw}
                  </span>
                );

              case 'foreign':
                // Not broken — just not ours to open.
                return (
                  <span key={r.raw} style={{ color: '#666', ...mono }}
                    title={`${r.raw}\n\nA fact in another knomit repo`}>
                    {'→'} {refLabel(r, repoNames)}
                  </span>
                );

              case 'source_code':
                // No "(source)" marker: src:// is already in the label, and
                // the marker cost a line of width on the one kind whose raw
                // form is already the longest here.
                return <SourceRefRow key={r.raw} raw={r.raw} label={refLabel(r, repoNames)} />;

              default:
                return <span key={r.raw} style={{ color: '#666', ...mono }}>{'→'} {r.raw}</span>;
            }
    }
}

export function FactBody({ fact, dispatch, onRefClick, repoNames = NO_REPO_NAMES }: Props) {
  const hasTags = (fact.domain?.length || 0) > 0 || (fact.entities?.length || 0) > 0;
  const hasRefs = (fact.refs?.length || 0) > 0;

  return (
    <>
      {/* The type/origin chips and the Confidence/Sources boxes used to live
          here. They were never body — they describe the fact, so they moved to
          FactMetaLine, which says all of it (plus the mount and the path) on one
          line in the header. This component is now content: prose, tags, refs. */}
      <div data-testid="fact-body" className="k-prose" style={{ color: '#ccc', lineHeight: 1.7, fontSize: 14, marginBottom: 8 }}>
        <ReactMarkdown remarkPlugins={markdownPlugins} components={markdownComponents}>{fact.body || ''}</ReactMarkdown>
      </div>

      {/* One metadata block, ruled off from the prose. Without the rule the
          first heading read as another section of the body — these are FACTS
          ABOUT the fact, not more of it. The whole block is conditional: an
          empty bordered div is a line drawn across the panel under nothing. */}
      {(hasTags || hasRefs) && (
        <div data-testid="fact-metadata" style={{
          marginTop: 26, paddingTop: 18, borderTop: '1px solid #1e222a',
        }}>
          {/* A PROPERTY TABLE: the label is a column BESIDE its values, not a
              heading above them. A heading on its own line costs a line and
              leaves the width next to it empty — which the stacked version did
              three times over. This is the shape Notion, Jira and Linear all
              converged on for the properties of a thing, and for the same
              reason. */}
          <div style={{
            display: 'grid', gridTemplateColumns: '88px minmax(0, 1fr)',
            gap: '14px 16px', alignItems: 'baseline',
          }}>
            {(fact.domain?.length ?? 0) > 0 && (
              <>
                <div style={{ ...propKey, color: 'rgba(119,204,153,0.85)' }}>Domains</div>
                <FacetValues entries={fact.domain} color="119,204,153"
                  onTagClick={d => dispatch({ type: 'ADD_FILTER', chip: { category: 'domain', value: d } })} />
              </>
            )}
            {(fact.entities?.length ?? 0) > 0 && (
              <>
                <div style={{ ...propKey, color: 'rgba(136,170,255,0.85)' }}>Entities</div>
                <FacetValues entries={fact.entities} color="136,170,255"
                  onTagClick={e => dispatch({ type: 'ADD_FILTER', chip: { category: 'entity', value: e } })} />
              </>
            )}
            {hasRefs && (
              <>
                <div style={{ ...propKey, color: '#8a93a3' }}>References</div>
                <RefList refs={fact.refs} onRefClick={onRefClick} repoNames={repoNames} />
              </>
            )}
          </div>
        </div>
      )}
    </>
  );
}

export function StatBox({ label, value, color }: { label: string; value: ReactNode; color: string }) {
  return (
    <div style={{ borderLeft: `3px solid ${color}`, padding: '10px 16px', background: '#1a1a2a', borderRadius: '0 6px 6px 0', minWidth: 90 }}>
      <div style={{ fontSize: 10, color: '#666', textTransform: 'uppercase', letterSpacing: 1 }}>{label}</div>
      <div style={{ fontFamily: 'var(--k-font-display)', fontSize: 22, fontWeight: 600, color: '#eee', letterSpacing: '-0.01em', marginTop: 2 }}>{value}</div>
    </div>
  );
}

export function FacetValues({ entries, color, onTagClick, focusedValue }: {
  entries: [string, number][] | string[];
  color: string;
  onTagClick: (value: string) => void;
  focusedValue?: string;
}) {
  if (entries.length === 0) return null;

  const items: [string, number][] = typeof entries[0] === 'string'
    ? (entries as string[]).map(s => [s, 1])
    : entries as [string, number][];
  // Counts only when they say something: a fact's own tags all carry 1, and a
  // row of 1s is noise.
  const weighted = items.some(([, n]) => n !== items[0][1]);

  return (
    // INLINE, not a column grid. These values are short, unordered and few —
    // there is nothing to align them FOR, and equal-width cells spent most of
    // their width on the space beside "ui" and "web". Each value takes what it
    // needs; the run wraps.
    <div style={{ display: 'flex', flexWrap: 'wrap', gap: '3px 18px', minWidth: 0 }}>
      {items.map(([name, n]) => (
        <span key={name} data-testid="tag-item" data-value={name}
          // NOT gated on readOnly: filtering is navigation, not an edit. See
          // the note on the metadata table.
          onClick={() => onTagClick(name)}
          style={{
            display: 'inline-flex', alignItems: 'baseline', gap: 5,
            fontSize: 11.5, cursor: 'pointer', whiteSpace: 'nowrap',
            color: name === focusedValue ? `rgb(${color})` : '#b9c1cd',
            transition: 'color 0.12s',
          }}
          onMouseEnter={e => { e.currentTarget.style.color = '#fff'; }}
          onMouseLeave={e => { e.currentTarget.style.color = name === focusedValue ? `rgb(${color})` : '#b9c1cd'; }}
        >
          {name}
          {weighted && (
            <span style={{
              fontFamily: 'var(--k-font-mono)', fontSize: 10, color: '#6d7788',
              fontVariantNumeric: 'tabular-nums',
            }}>{n}</span>
          )}
        </span>
      ))}
    </div>
  );
}
