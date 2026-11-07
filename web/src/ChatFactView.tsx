import { useEffect, useMemo, useRef, useState } from 'react';
import type { CSSProperties, ReactNode } from 'react';
import { api } from './api';
import type { Fact, RefGroup } from './api';
import { FactBody } from './FactBody';
import { FactActions, FactEdgesGroup, FactPage } from './FactChrome';
import { CHAT_RESULT_LOADING } from './chatCopy';
import type { BrowseFactRequest } from './chatPanelState';
import { NoticeLine } from './ChatNotice';
import { MaterializeView } from './MaterializeView';
import { parseMaterializeParams } from './materializeOptions';
import { orderMotifs } from './MotifRow';
import { useMotifClusters } from './useMotifClusters';

const noDispatch = () => {};
const nothing = () => {};

interface Loaded {
  key: string;
  fact?: Fact;
  incoming?: RefGroup[];
  outgoing?: RefGroup[];
  edgesError?: string;
  error?: string;
}

/** The server's sentence, not the request URL fetchJSON puts in front of it. */
export function PanelLoadError({ error, testId }: { error: string; testId: string }) {
  const sentence = /→ \d{3} ([\s\S]+)$/.exec(error)?.[1] ?? error;
  return <div role="alert" style={{ padding: '0 18px' }}><NoticeLine tone="blocked" testId={testId} lead={sentence} /></div>;
}

const NO_REFS: RefGroup[] = [];

/**
 * ChatFactView shows one fact in the chat's side panel with Browse's own chrome
 * (FactChrome: the band, its cells and actions, the title that flows up into the
 * band as the body scrolls) and body. It has no filters or history of its own, so
 * what opens something in Browse here sends the reader there instead: the version,
 * cited by, cites and a motif each land in Browse on this fact, with `onBrowse`.
 * Materialize is the one thing it does in place, as Browse does. It offers no
 * retract, which destroys rather than reads.
 *
 * With `commit` everything on screen is that version's, and each of those landings
 * is at that commit. `removed` says the commit retracted the fact: a plain read
 * there is 404 by design, so it reads the last version before it (?fallback=before)
 * and the band's badge says so, as Browse's does.
 *
 * It fetches the fact itself (api.fact), so whoever opens it only names
 * {repo, branch, path}. The edges come from a separate read that may fail on its
 * own: the cells then show the failure, as they do in Browse.
 */
export function ChatFactView({ repo, branch, path, commit, removed = false, closeControl, onBrowse }: {
  repo: string;
  branch: string;
  path: string;
  commit?: string;
  removed?: boolean;
  /** The panel's ×: this view has no header, so it sits in the band. */
  closeControl: ReactNode;
  /** Send the reader to Browse on this fact. Absent where the page cannot navigate. */
  onBrowse?: (req: BrowseFactRequest) => void;
}) {
  const key = `${repo}\0${branch}\0${path}\0${commit ?? ''}`;
  const [loaded, setLoaded] = useState<Loaded | null>(null);
  useEffect(() => {
    let cancelled = false;
    const opts = removed ? { fallback: 'before' as const } : undefined;
    api.fact(repo, branch, path, commit, opts)
      .then(fact => { if (!cancelled) setLoaded(l => ({ ...(l?.key === key ? l : {}), key, fact })); })
      .catch(e => { if (!cancelled) setLoaded({ key, error: e instanceof Error ? e.message : String(e) }); });
    api.explain(repo, branch, path, commit, opts)
      .then(r => { if (!cancelled) setLoaded(l => ({ ...(l?.key === key ? l : { key }), incoming: r.incoming, outgoing: r.outgoing })); })
      .catch(e => { if (!cancelled) setLoaded(l => ({ ...(l?.key === key ? l : { key }), edgesError: String(e) })); });
    return () => { cancelled = true; };
  }, [repo, branch, path, commit, removed, key]);

  // An answer for another fact than the one asked for is the previous one's.
  const shown = loaded?.key === key ? loaded : null;
  const endpoint = useMemo(() => ({ kind: 'repo' as const, repo, branch }), [repo, branch]);
  const resolvedMotifs = useMotifClusters(endpoint, shown?.fact?.motifs);
  const motifs = useMemo(() => orderMotifs(resolvedMotifs), [resolvedMotifs]);
  const menuRef = useRef<HTMLSpanElement>(null);
  const [materialize, setMaterialize] = useState(false);
  const [matOptions, setMatOptions] = useState(() => parseMaterializeParams('').options);

  if (!shown?.fact && !shown?.error) {
    return <><div style={placeholderBar}>{closeControl}</div><div data-testid="chat-fact-loading" style={{ color: '#777', fontSize: 13, padding: '14px 18px' }}>{CHAT_RESULT_LOADING}</div></>;
  }
  if (shown.error) {
    return <><div style={placeholderBar}>{closeControl}</div><PanelLoadError error={shown.error} testId="chat-fact-error" /></>;
  }
  const fact = shown.fact!;
  if (materialize) {
    return (
      <MaterializeView
        target={{ repo, branch, path, commit }}
        title={fact.title || fact.path}
        options={matOptions}
        onOptionsChange={setMatOptions}
        onBack={() => setMaterialize(false)}
      />
    );
  }
  const send = (req: Omit<BrowseFactRequest, 'repo' | 'branch' | 'path'>) => {
    onBrowse?.({ repo, branch, path, ...(commit ? { commit } : {}), ...req });
  };
  return (
    <div data-testid="chat-fact" style={{ height: '100%', minHeight: 0 }}>
      <FactPage fact={fact} dispatch={noDispatch} filterable={false}
        edges={
          <FactEdgesGroup
            connections={{
              panelId: 'chat-connections', open: null,
              incoming: shown.incoming ?? NO_REFS, outgoing: shown.outgoing ?? NO_REFS, error: shown.edgesError ?? null,
              onToggle: dir => send({ panel: { connections: dir } }),
              onClose: nothing, onHop: nothing, menuRef, onMouseEnter: nothing, onMouseLeave: nothing,
            }}
            motifs={motifs}
            motifSlot={{ open: null, onToggle: motif => send({ panel: { motif } }), onClose: nothing, onPivot: nothing, panelId: '' }}
          />
        }
        actions={
          <FactActions fact={fact} histAnchor={{ repo, branch, path }} anchorCommit={commit ?? null}
            // The version opens Browse's history of this fact. Browse's walker opens it
            // at the newest version; a fact read at a commit stays at that commit.
            onScrub={newest => send({ commit: commit ?? newest })}
            onMaterialize={() => setMaterialize(true)} exactVersion={!!commit}>
            {closeControl}
          </FactActions>
        }>
        <FactBody fact={fact} dispatch={noDispatch} filterable={false} />
      </FactPage>
    </div>
  );
}

// Only while there is no band to hold the ×.
const placeholderBar: CSSProperties = { display: 'flex', justifyContent: 'flex-end', padding: '7px 8px', flexShrink: 0 };
