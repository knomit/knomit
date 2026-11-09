import type { ExperimentInfo, Lens, LensSource } from './api';
import { displayLensPath } from './utils';

export type View = 'library';

export type LibrarySort = 'path' | 'recent' | 'relevance';

// BrowseContext is the surface the app is currently browsing: a whole repo, or
// a lens's read union. It is the single source of truth for "what am I looking
// at" — SET_REPO is a thin wrapper that dispatches SET_CONTEXT {kind:'repo'}.
export type BrowseContext =
  | { kind: 'repo'; repo: string }
  | { kind: 'lens'; name: string };

export interface FilterChip {
  // No 'repo' here: scoping a lens to some of its mounts is state.lensSources,
  // driven by the sources dropdown and the summary's Repos rows. It was briefly
  // also a chip, which meant two controls over one scope that could disagree.
  // `motif` combines OR, like domain/type/ep — two motif chips WIDEN the list
  // (the server's `motifs` param is a single CSV read with splitCSV), which is
  // the opposite of entity's AND. See kb/conventions/ui/filter-multi-chip-semantics.
  category: 'domain' | 'entity' | 'type' | 'kind' | 'origin' | 'ep' | 'path' | 'motif';
  value: string;
  // Motif chips minted by PIVOT_MOTIF only: the path scope the pivot displaced,
  // so EXIT_MOTIF can put the reader back in the folder they pivoted from. ON
  // THE CHIP, not a top-level field, deliberately: pushNav snapshots filters,
  // so the stash rides through NAV_BACK for free and dies with the chip —
  // a separate field would need its own lifecycle in every arm that touches
  // history or filters. A motif chip typed into the FilterBar never carries it
  // (ADD_FILTER displaces no path), and that absence is load-bearing: absent
  // means "restore nothing", never "restore root".
  returnPath?: string;
}

export type AsOf =
  | { mode: 'live' }
  | { mode: 'history'; commit: string }
  | { mode: 'diff'; from: string; to: string };

interface NavEntry {
  // Set when this departure was to Browse from a chat: going back to this entry
  // goes back to that chat (chatReturn). It stands in for everything older on
  // the trail, because Back from here is the chat and not what Browse held.
  fromChat?: string;
  // The browse context this entry was left in. Back to an entry from another
  // context switches back to it (navReturn) before anything else is restored:
  // a fact path and filters mean nothing in another repo or lens.
  context: BrowseContext;
  repo: string;
  // The branch this entry was left on, with what the app holds about it. Inside
  // one context only a chat's landing (fromChat) restores these: it can move the
  // branch, and factPath and filters mean nothing on another one. Back inside
  // Browse leaves the branch. Back across contexts always returns to this branch
  // (navReturn), read again rather than trusted.
  branch: string;
  head: string;
  experiment: ExperimentInfo | null;
  branchWritable: boolean;
  view: View;
  filters: FilterChip[];
  freeText: string;
  factPath: string | null;
  asOf: AsOf;
  // The sort axis in force when this entry was left. Restored on NAV_BACK
  // because Path and Recent are different WAYS OF LOOKING, not a preference:
  // backing out of a chrono-sorted list into an ontology-sorted one silently
  // rearranges the rows under the cursor. SET_SORT itself does not push — a
  // sort change is a refinement of the current view, not a move.
  sort: LibrarySort;
  // The sources selection in force when this entry was left, captured for the
  // same reason as the sort: WHICH MOUNTS you are reading is a way of looking,
  // and a back that restored the path and the sort but left the union pinned to
  // one mount would return you to a view you were never in. SET_LENS_SOURCES
  // does not push either — a dropdown toggle refines the current view.
  lensSources: string[] | null;
  // Since-fork in force when this entry was left. A chat's landing clears it
  // (Library would demote the tree), and going back to the chat restores it.
  sinceFork: boolean;
}

export interface AppState {
  // NAV_BACK popped an entry that came from this chat. App shows the chat (the
  // view state is not ours, see facets.ts) if it is still there, and clears this.
  chatReturn: string | null;
  // Going back to a chat restored a branch from its entry. App reads that branch's
  // status (it may be gone by now, and the entry may be stale), and clears this.
  branchRecheck: boolean;
  // Back popped an entry from another browse context. App first checks that the
  // context still exists and switches to it (entered), then, once its branch is
  // known, reads the entry's branch and restores the entry on it (NAV_RETURN_LAND).
  navReturn: { entry: NavEntry; entered: boolean } | null;
  repo: string;
  // context is the browse surface (repo | lens). In a repo context, repo/branch
  // are authoritative for both reads and writes. In a lens context, reads come
  // via the lens endpoints (Task 14); repo/branch are kept valid pointing at the
  // lens's write mount + its agent branch so write routing never breaks.
  context: BrowseContext;
  lens: Lens | null;            // resolved lens doc; null in repo context
  // lensWriteId is the lens write member's 12-char root-commit id (RepoInfo.id,
  // matched by uid when the lens resolves) — the identity a LensSource.id
  // carries. The lens doc names its members by uid and name only. Meaningful
  // only while `lens` is set; null when the repo listing gave no id.
  lensWriteId: string | null;
  lensSources: string[] | null; // sources-dropdown selection; null = all mounts
  // factSource is the source mount of the currently open lens fact — set by the
  // fact-open path when a lens fact loads (Task 14/16), and cleared on context
  // switch. It is the temporal/write anchor for the open fact in a lens context.
  factSource: LensSource | null;
  view: View;
  factPath: string | null;       // right panel: fact to display (all modes)
  asOf: AsOf;                    // global "as of when" anchor (live | history | diff)
  filters: FilterChip[];
  freeText: string;              // unprefixed search text
  tasks: Record<string, { status: 'idle' | 'running' | 'done' | 'error'; message: string }>;
  headCommit: string;
  branch: string;
  embeddingsEnabled: boolean;
  ontologyRoot: string;
  indexState: string;  // "ready" | "indexing" | "error"
  indexDone: number;
  indexTotal: number;
  indexPercent: number;  // 0–100; 100 when ready
  navStack: NavEntry[];
  // The remote's two halves fail INDEPENDENTLY — a fetch can recover while a
  // push is still rejected — and each maps 1:1 onto the column Sync/Push write
  // (remotes.last_status / last_push_status). Tracking them apart is what stops
  // a sync_ok from clearing a banner a still-broken push raised. Read them
  // through remoteErrorText, never directly.
  remoteSyncError: string;
  remotePushError: string;
  rightPanelFocused: boolean;
  librarySort: LibrarySort;
  notice: string;
  searching: boolean;            // a relevance (free-text) search request is in flight
  serverReadOnly: boolean;       // instance-level read-only (demo mode)
  // branchWritable is the CURRENT branch's own eligibility, straight from the
  // branch row's `writable`. It is a THIRD read-only dimension, independent of
  // the other two: the consensus branch is not writable while the anchor is
  // live and the instance is not a demo.
  //
  // Keyed on `writable`, never on is_agent_branch — an experiment is writable
  // and is NOT the agent branch, so gating on the latter would lock the user
  // out of exactly the branch this feature exists to let them work on.
  branchWritable: boolean;
  // sinceFork narrows the fact list to what changed since the experiment's
  // fork point. Only meaningful inside an experiment, and cleared on the way
  // out (see SET_STATUS).
  sinceFork: boolean;
  // experiment is the branch row's experiment object, null when the current
  // branch is not one. Presence drives the in-experiment marker and the
  // since-fork filter; fork_commit is what that filter anchors on.
  experiment: ExperimentInfo | null;
  repoReadOnly: boolean;         // the browsed repo is a subscription (read-only)
  // factTitles caches the human title of every fact the RightPanel has loaded,
  // keyed by factTitleKey(path, commit). The breadcrumb reads from it so it can
  // label a crumb with the title we ALREADY read when navigating there — rather
  // than re-fetching, which fails for a retracted fact (the live single-fact
  // endpoint 404s a tombstone). Session-only: the trail is session-only too, so
  // every crumb was visited and is therefore cached.
  factTitles: Record<string, string>;
}

// factTitleKey is the shared cache key for a fact's title: its path plus the
// commit it was read at (undefined = live/HEAD). The breadcrumb's crumb key and
// the RightPanel's cache write MUST agree, so both go through this.
export function factTitleKey(path: string, commit?: string): string {
  return `${path}@${commit ?? 'HEAD'}`;
}

export type Action =
  | { type: 'NAVIGATE'; path: string }
  | { type: 'GO_UP' }
  | { type: 'ADD_FILTER'; chip: FilterChip }
  | { type: 'REMOVE_FILTER'; index: number }
  | { type: 'SET_FREE_TEXT'; text: string }
  | { type: 'CLEAR_FILTERS' }
  | { type: 'NAV_BACK' }
  | { type: 'SET_TASK'; op: string; status: 'idle' | 'running' | 'done' | 'error'; message: string }
  | { type: 'CLEAR_TASK'; op: string }
  | { type: 'SET_STATUS'; head: string; branch: string; embeddingsEnabled: boolean; ontologyRoot: string; indexState?: string; indexDone?: number; indexTotal?: number; indexPercent?: number; branchWritable?: boolean; experiment?: ExperimentInfo | null }
  | { type: 'SET_SINCE_FORK'; value: boolean }
  | { type: 'SET_HEAD'; head: string }
  // SET_INDEX carries ONLY the index fields, from the index event stream. It is
  // deliberately not SET_STATUS: that action rebuilds head, branch, ontology and
  // embeddings from a status READ, and an index event knows none of them —
  // reusing it would either blank those or force a fetch per event.
  | { type: 'SET_INDEX'; indexState: string; indexDone: number; indexTotal: number }
  | { type: 'SET_REPO'; repo: string }
  // push: a move the reader made (a rail pick, a chat's landing in another
  // context): the place left goes on the back stack instead of the stack being
  // dropped. fromChat stamps that entry for a chat's landing.
  | { type: 'SET_CONTEXT'; context: BrowseContext; push?: boolean; fromChat?: string }
  // The steps of Back to another context (navReturn): switch to it, drop it
  // because it is gone, land the entry once its branch is read, or give up on
  // the landing (the branch could not be read) and stay on the context's view.
  | { type: 'NAV_RETURN_ENTER' }
  | { type: 'NAV_RETURN_GONE' }
  | { type: 'NAV_RETURN_BRANCH_GONE' }
  | { type: 'NAV_RETURN_LAND' }
  | { type: 'NAV_RETURN_DROP' }
  | { type: 'CACHE_FACT_TITLE'; key: string; title: string }
  // writeId: the write member's root-commit id (see AppState.lensWriteId).
  | { type: 'SET_LENS'; lens: Lens; writeId?: string }
  | { type: 'SET_LENS_SOURCES'; repos: string[] | null }
  | { type: 'FOCUS_LENS_SOURCE'; repo: string }
  | { type: 'SET_FACT_SOURCE'; source: LensSource | null }
  | { type: 'SET_REMOTE_ERROR'; side: 'sync' | 'push'; error: string }
  | { type: 'CLEAR_REMOTE_ERRORS' }
  | { type: 'FOCUS_RIGHT_PANEL' }
  | { type: 'BLUR_RIGHT_PANEL' }
  | { type: 'SET_AS_OF'; asOf: AsOf }
  // `sort` rides APPLY_NAV for the same reason `filters` does: a reveal changes
  // the path scope, the sort axis and the open fact together, and they must land
  // in ONE entry or Back would undo them one at a time.
  | { type: 'BRANCH_RECHECK_SERVED' }
  | { type: 'BRANCH_GONE' }
  // departed: the context switch before this landing already pushed what the
  // reader left (SET_CONTEXT push), so this one does not push again.
  | { type: 'APPLY_NAV'; view: View; factPath: string | null; asOf: AsOf; filters?: FilterChip[]; freeText?: string; sort?: LibrarySort; sinceFork?: boolean; hop?: boolean; fromChat?: string; departed?: boolean }
  | { type: 'CHAT_RETURN_SERVED' }
  | { type: 'AMEND_NAV'; factPath: string | null; asOf?: AsOf }
  | { type: 'SET_LIBRARY_SORT'; sort: LibrarySort }
  // A reload puts back the list the URL carried (listUrl.ts), once its context
  // and branch are confirmed. ONE action so it lands as one state.
  | { type: 'RESTORE_LIST'; filters: FilterChip[]; freeText: string; sort: LibrarySort; lensSources: string[] | null; sinceFork: boolean }
  // "Show me every fact with this shape." ONE action, not ADD_FILTER plus a
  // path clear: two dispatches would be two chances to push and, as the
  // lens-sources bug showed, in practice none.
  | { type: 'PIVOT_MOTIF'; motif: string }
  // The two derived modes' exits. Each leaves its mode and puts you back where
  // you were — see the reducer arms, which are twins: search kept your folder
  // all along, the pivot restores the one it displaced.
  | { type: 'EXIT_SEARCH' }
  | { type: 'EXIT_MOTIF' }
  | { type: 'SET_NOTICE'; text: string }
  | { type: 'CLEAR_NOTICE' }
  | { type: 'SET_SEARCHING'; value: boolean }
  | { type: 'SET_SERVER_READONLY'; value: boolean }
  | { type: 'SET_REPO_READONLY'; value: boolean };

export const init: AppState = {
  // No repo is selected until the server's repo list loads — the UI must never
  // assume a repo name exists (any repo, including the default, can be renamed
  // or deleted server-side). App picks the repo from /api/v1/repos on mount.
  repo: '',
  context: { kind: 'repo', repo: '' },
  lens: null,
  lensWriteId: null,
  lensSources: null,
  chatReturn: null,
  branchRecheck: false,
  navReturn: null,
  factSource: null,
  view: 'library',
  factPath: null,
  asOf: { mode: 'live' },
  filters: [],
  freeText: '',
  tasks: { sync: { status: 'idle', message: '' }, synth: { status: 'idle', message: '' } },
  headCommit: '',
  branch: '',
  embeddingsEnabled: false,
  ontologyRoot: 'kb',
  indexState: 'ready',
  indexDone: 0,
  indexTotal: 0,
  indexPercent: 100,
  navStack: [],
  remoteSyncError: '',
  remotePushError: '',
  rightPanelFocused: false,
  // Ontology browsing, not chrono: the tree is how the corpus is organised, so
  // arriving at a folder listing beats arriving at a flat by-date feed.
  librarySort: 'path',
  notice: '',
  searching: false,
  serverReadOnly: false,
  branchWritable: true,
  experiment: null,
  sinceFork: false,
  repoReadOnly: false,
  factTitles: {},
};

function pushNav(s: AppState, fromChat?: string): NavEntry[] {
  const entry: NavEntry = {
    ...(fromChat ? { fromChat } : {}),
    context: s.context,
    repo: s.repo,
    branch: s.branch,
    head: s.headCommit,
    experiment: s.experiment,
    branchWritable: s.branchWritable,
    view: s.view,
    filters: [...s.filters],
    freeText: s.freeText,
    factPath: s.factPath,
    asOf: s.asOf,
    sort: s.librarySort,
    lensSources: s.lensSources === null ? null : [...s.lensSources],
    sinceFork: s.sinceFork,
  };
  const stack = [...s.navStack, entry];
  if (stack.length > 20) stack.shift();
  return stack;
}

// sameContext: two browse contexts are the same surface (the same repo, or the
// same lens).
export function sameContext(a: BrowseContext, b: BrowseContext): boolean {
  return a.kind === 'repo' ? b.kind === 'repo' && a.repo === b.repo : b.kind === 'lens' && a.name === b.name;
}

// canGoBack reports whether NAV_BACK has anywhere to go. There is no forward:
// NAV_BACK pops, so the entries ahead of the cursor do not exist to return to.
// A back-only history is the whole model here — see the Library header.
// While a Back to another context is under way (navReturn) there is nowhere to
// go back to yet: NAV_BACK ignores itself then, so the control says so.
export function canGoBack(state: AppState): boolean {
  return state.navStack.length > 0 && !state.navReturn;
}

export function currentPath(state: AppState): string {
  const pathChip = state.filters.find(f => f.category === 'path');
  return pathChip?.value || state.ontologyRoot || 'kb';
}

function replacePathChip(filters: FilterChip[], value: string): FilterChip[] {
  return [...filters.filter(f => f.category !== 'path'), { category: 'path', value }];
}

function applyAction(s: AppState, a: Action): AppState {
  switch (a.type) {
    case 'NAVIGATE':
      return {
        ...s,
        filters: replacePathChip(s.filters, a.path),
        factPath: null,
        navStack: pushNav(s),
        rightPanelFocused: false,
      };
    case 'GO_UP': {
      const path = currentPath(s);
      const parts = path.split('/');
      if (parts.length <= 1) return s;
      const parent = parts.slice(0, -1).join('/');
      return {
        ...s,
        filters: replacePathChip(s.filters, parent),
        factPath: null,
        navStack: pushNav(s),
        rightPanelFocused: false,
      };
    }
    case 'ADD_FILTER': {
      const isPath = a.chip.category === 'path';
      const filters = isPath
        ? replacePathChip(s.filters, a.chip.value)
        : [...s.filters, a.chip];
      // Path-changing filters are navigations; clear the open fact so the
      // right panel returns to the stats view for the new path. Non-path
      // filters are refinements that should preserve the current selection.
      return { ...s, filters, factPath: isPath ? null : s.factPath, navStack: pushNav(s) };
    }
    case 'REMOVE_FILTER': {
      const filters = s.filters.filter((_, i) => i !== a.index);
      return { ...s, filters, factPath: null, navStack: pushNav(s) };
    }
    case 'SET_FREE_TEXT': {
      // When clearing the search box leaves no active non-path filters, the
      // user has exited search mode — drop the (typically auto-selected)
      // factPath so the right panel returns to root stats instead of stranding
      // the previous result.
      const exitingSearch = a.text === '' && !s.filters.some(f => f.category !== 'path');
      return { ...s, freeText: a.text, factPath: exitingSearch ? null : s.factPath };
    }
    case 'CLEAR_FILTERS':
      return { ...s, filters: [], freeText: '', factPath: null, navStack: pushNav(s) };
    // EXIT_SEARCH leaves a search without moving you. Relevance is DERIVED —
    // effectiveSort = searchActive ? 'relevance' : librarySort — so the mode you
    // were in is still in librarySort, and stopping the search is the whole of
    // going back to it. Which is why this must NOT write librarySort: the
    // Relevance segment used to dispatch SET_LIBRARY_SORT{relevance}, storing
    // the derived value over the remembered one and erasing the only record of
    // where the reader came from.
    //
    // The path chip survives: it is location, not search. Leaving a query
    // should not also teleport you out of the folder you were searching in —
    // that is CLEAR_FILTERS' job (Escape), and it is a different intent.
    //
    // factPath goes for the reason SET_LIBRARY_SORT drops it: the list is about
    // to be replaced wholesale, and a selection carried over from results the
    // reader just discarded is a stranded one. Each mode then does its own
    // thing — Recent auto-selects its first row, Path waits to be asked.
    case 'EXIT_SEARCH': {
      const kept = s.filters.filter(f => f.category === 'path');
      const sort = s.librarySort === 'relevance' ? 'recent' : s.librarySort;
      return { ...s, filters: kept, freeText: '', factPath: null, librarySort: sort, navStack: pushNav(s) };
    }
    // EXIT_MOTIF is EXIT_SEARCH's twin for the other derived mode: leaving a
    // pivot without moving you. The motif chip IS the pivot, so dropping it is
    // the whole of leaving — and librarySort is untouched for the same reason,
    // because it still holds the mode the reader came from and PIVOT_MOTIF
    // deliberately no longer overwrites it.
    //
    // ONLY the motif chip goes. A domain the reader added to narrow the pivot
    // survives, because it narrows where they are rather than being what they
    // are looking at. (EXIT_SEARCH drops all non-path chips; a search's
    // refinements belong to the search. A pivot's do not.)
    //
    // ...and the folder the pivot displaced COMES BACK. EXIT_SEARCH's comment
    // states the rule this arm long violated: leaving a derived mode should
    // not also teleport you out of the folder you were in. Search never drops
    // the path chip so its exit keeps it for free; the pivot must drop it on
    // the way in (a shape cuts across the ontology), so its exit restores it
    // from the chip's own stash — otherwise "Leave this motif and go back"
    // lands an ontology browser at the root, one gesture away from a chevron
    // that goes back properly. A path chip already present wins over the
    // stash: it says where the reader is NOW, and a reveal can plant one.
    case 'EXIT_MOTIF': {
      const motifChip = s.filters.find(f => f.category === 'motif');
      if (!motifChip) return s;
      const rest = s.filters.filter(f => f.category !== 'motif');
      const kept = motifChip.returnPath && !rest.some(f => f.category === 'path')
        ? [...rest, { category: 'path' as const, value: motifChip.returnPath }]
        : rest;
      return { ...s, filters: kept, factPath: null, navStack: pushNav(s) };
    }
    case 'NAV_BACK': {
      // One Back at a time: while one to another context is under way, the stack
      // below it belongs to the place it is going to.
      if (s.navStack.length === 0 || s.navReturn) return s;
      const prev = s.navStack[s.navStack.length - 1];
      // Back to another repo or lens: what was left there means nothing here, so
      // App switches to it first (navReturn) and restores the entry once its branch
      // is read. Nothing of the entry is applied here.
      if (!sameContext(prev.context, s.context)) {
        return {
          ...s,
          chatReturn: prev.fromChat ?? null,
          navReturn: { entry: prev, entered: false },
          navStack: s.navStack.slice(0, -1),
          rightPanelFocused: false,
        };
      }
      // Back to a chat: what the reader left when the landing took them away. It is
      // only what the app knew then, so App re-reads it (branchRecheck). Back inside
      // Browse leaves the branch alone: a branch moved there by the picker or by an
      // experiment ending is where the reader chose to be, and may be all there is.
      const moved = !!prev.branch && prev.branch !== s.branch;
      const branchBack = prev.fromChat ? {
        ...(moved ? { branch: prev.branch, headCommit: prev.head, experiment: prev.experiment, branchWritable: prev.branchWritable } : {}),
        // Only an experiment holds it, as SET_SINCE_FORK guards.
        sinceFork: prev.sinceFork && !!(moved ? prev.experiment : s.experiment),
        branchRecheck: true,
      } : {};
      return {
        ...s,
        ...branchBack,
        chatReturn: prev.fromChat ?? null,
        view: prev.view,
        factPath: prev.factPath,
        asOf: prev.asOf,
        filters: prev.filters,
        freeText: prev.freeText,
        librarySort: prev.sort,
        lensSources: prev.lensSources,
        navStack: s.navStack.slice(0, -1),
        rightPanelFocused: false,
      };
    }
    case 'SET_TASK': {
      const cur = s.tasks[a.op];
      if (cur && cur.status === a.status && cur.message === a.message) return s;
      return { ...s, tasks: { ...s.tasks, [a.op]: { status: a.status, message: a.message } } };
    }
    case 'CLEAR_TASK': {
      // Retires a FINISHED task back to idle. Only the terminal states are
      // retired: a running task owns the footer for as long as it runs, and
      // dropping one mid-flight would hide live progress. Without this nothing
      // ever left the footer, so the last "done" of the session sat there
      // reading like something still happening.
      const cur = s.tasks[a.op];
      if (!cur || cur.status === 'idle' || cur.status === 'running') return s;
      return { ...s, tasks: { ...s.tasks, [a.op]: { status: 'idle', message: '' } } };
    }
    case 'SET_STATUS':
      return {
        ...s,
        // A blank head is the server declining to answer, not a repo with no
        // commits: the branch root discards WithRead's error, and WithRead
        // skips the closure when Acquire fails, so a store swap or open makes
        // it answer 200 with head "". Both refresh call sites (the 2s indexing
        // poll and the post-task refresh) run in exactly that window. Blanking
        // headCommit there drops the head pill, sends edge reads to the
        // un-anchored route and churns every refetch keyed on it. Keeping the
        // last known head cannot smuggle a stale one across a repo switch:
        // SET_CONTEXT and SET_LENS both clear headCommit before the status
        // bootstrap re-runs, so `s.headCommit` is already "" by then.
        headCommit: a.head || s.headCommit,
        branch: a.branch,
        embeddingsEnabled: a.embeddingsEnabled,
        ontologyRoot: a.ontologyRoot || s.ontologyRoot,
        indexState: a.indexState ?? s.indexState,
        indexDone: a.indexDone ?? s.indexDone,
        indexTotal: a.indexTotal ?? s.indexTotal,
        indexPercent: a.indexPercent ?? s.indexPercent,
        // ?? not ||: `false` is a real answer here and must not fall through
        // to the previous value. An action that omits the key entirely (an
        // older code path) keeps what we had.
        branchWritable: a.branchWritable ?? s.branchWritable,
        experiment: a.experiment !== undefined ? a.experiment : s.experiment,
        // Leaving an experiment must drop the filter with it: a "since the
        // fork" view of a branch that has no fork point is meaningless, and
        // silently keeping it on would filter the agent branch against a
        // commit from somewhere else.
        sinceFork: a.experiment ? s.sinceFork : false,
      };
    case 'SET_SINCE_FORK':
      // Guarded rather than trusted: nothing outside an experiment may turn
      // this on, so a stale control cannot filter the agent branch against a
      // fork point that does not apply to it.
      return { ...s, sinceFork: s.experiment ? a.value : false };
    case 'SET_HEAD':
      if (s.headCommit === a.head) return s;
      return { ...s, headCommit: a.head };
    case 'SET_INDEX': {
      // indexPercent is DERIVED here rather than carried, because the server
      // computes it in the status payload and an event that carried its own
      // could disagree with the counts beside it. 100 at ready, so the banner
      // does not sit at 97% after the terminal event.
      const pct = a.indexState === 'ready'
        ? 100
        : a.indexTotal > 0 ? Math.round((a.indexDone / a.indexTotal) * 100) : 0;
      if (s.indexState === a.indexState && s.indexDone === a.indexDone
        && s.indexTotal === a.indexTotal && s.indexPercent === pct) return s;
      return { ...s, indexState: a.indexState, indexDone: a.indexDone, indexTotal: a.indexTotal, indexPercent: pct };
    }
    case 'SET_REPO':
      // Thin wrapper: switching to a repo is just entering a {kind:'repo'}
      // context. Reducing through SET_CONTEXT keeps a single reset path so repo
      // and lens switches can never drift apart.
      return reducer(s, { type: 'SET_CONTEXT', context: { kind: 'repo', repo: a.repo } });
    case 'SET_CONTEXT': {
      // Entering any browse surface resets the navigation state exactly as a
      // repo switch always did: asOf → live, open fact closed, filters/freeText
      // cleared, nav trail dropped, remote error cleared, right panel blurred.
      // factSource is dropped too — the open fact (and its source mount) is gone.
      const base: AppState = {
        ...s,
        context: a.context,
        view: 'library',
        factPath: null,
        asOf: { mode: 'live' },
        filters: [],
        freeText: '',
        // A move the reader made keeps where they were, so Back returns there
        // (navReturn). Every other entry (a fallback off a deleted repo, a boot
        // restore, a create landing) is not a move of theirs and drops the stack.
        // A pick before the branch is known (the last switch still opening) has
        // nothing of its own to push: the stack is kept as it is, not dropped.
        navStack: a.push ? (s.repo && s.branch ? pushNav(s, a.fromChat) : s.navStack) : [],
        remoteSyncError: '',
        remotePushError: '',
        rightPanelFocused: false,
        factSource: null,
        navReturn: null,
      };
      if (a.context.kind === 'repo') {
        // Repo context: repo/branch are authoritative. Clearing branch/headCommit
        // re-triggers the status bootstrap. lens state is not applicable.
        //
        // EXCEPT when the repo does not change — leaving a lens for its own
        // write repo (the remote banner's Reconnect, a rail pick). App's status
        // bootstrap is keyed on the repo NAME, so it would not re-run, and a
        // cleared branch would strand Browse on its "Opening…" placeholder
        // with the head poll and the event stream stopped. The branch the lens
        // context holds is that repo's own, so it is kept.
        if (a.context.repo === s.repo) return { ...base, repo: s.repo, lens: null, lensSources: null };
        return { ...base, repo: a.context.repo, headCommit: '', branch: '', lens: null, lensSources: null };
      }
      // Lens context: the write mount isn't known until SET_LENS resolves the
      // lens doc, so repo/branch stay at their previous (still valid) values
      // until then. lens/lensSources reset; App re-fetches the lens.
      return { ...base, lens: null, lensSources: null };
    }
    case 'SET_LENS':
      // Guard against a stale, out-of-order resolution. resolveLens is
      // fire-and-forget: a slow getLens(A) can resolve after the user already
      // switched to lens B or back to a repo context. Applying it would repoint
      // repo at A's write mount and set lens=A in the wrong context (violating
      // "lens is null in a repo context"). Only accept a lens doc that still
      // matches the active lens context.
      if (s.context.kind !== 'lens' || s.context.name !== a.lens.name) return s;
      // The lens's write mount becomes the app's write/status target so
      // state.repo/state.branch stay valid in a lens context. When the write
      // repo actually changes, clear branch/headCommit to re-run the status
      // bootstrap (which resolves the write repo's agent branch).
      return {
        ...s,
        lens: a.lens,
        lensWriteId: a.writeId ?? null,
        repo: a.lens.write.name,
        branch: s.repo === a.lens.write.name ? s.branch : '',
        headCommit: s.repo === a.lens.write.name ? s.headCommit : '',
      };
    case 'SET_LENS_SOURCES':
      return { ...s, lensSources: a.repos };
    case 'FOCUS_LENS_SOURCE':
      // "Show me this mount", from the summary's Repos section. ONE action
      // rather than the SET_LENS_SOURCES + SET_LIBRARY_SORT pair it replaces:
      // two dispatches meant two chances to push (or, as shipped, none), and
      // back then restored the sort while leaving the union pinned to one
      // mount. Naming the intent gives the history one thing to hook — the
      // same argument NAVIGATE settled for entering a directory.
      //
      // The sort switch is load-bearing, not cosmetic: path mode lists the
      // mount's topic FOLDERS and deliberately starts un-selected, so a pick
      // made from a facts-and-confidence table would land on a folder tree.
      // Clearing factPath lets the refetched list select its own first row.
      return {
        ...s,
        lensSources: [a.repo],
        librarySort: 'recent',
        factPath: null,
        navStack: pushNav(s),
      };
    case 'PIVOT_MOTIF': {
      // Everything the pivot changes, set in one arm: the chip that IS the
      // query, and the path it displaces. The
      // motif chip REPLACES any previous one rather than accumulating — a
      // pivot is "show me this shape", and two motif chips widen, so keeping
      // the old one would silently return a union nobody asked for.
      //
      // Path is dropped because a shape cuts ACROSS the ontology: scoping it to
      // one folder is the opposite of what the reader asked for. This is the
      // ONLY chip operation that clears path — ADD_FILTER leaves an existing
      // path chip in place, deliberately, because a domain or type chip narrows
      // within where you are. Nothing else here behaves this way, so the reason
      // has to stand on its own rather than lean on a precedent.
      //
      // Dropped, not forgotten: the displaced value is stashed on the new chip
      // as returnPath so EXIT_MOTIF can restore it. A re-pivot finds no path
      // chip to displace — the reader is inside a pivot — so it carries the
      // previous chip's stash forward: the place they were LAST STANDING is
      // still the record.
      //
      // librarySort is NOT written. The pivot already arrives in Recent by
      // DERIVATION — a motif chip is a content filter and the tree cannot
      // honour one, so effectiveSort overrides Path for its duration — and
      // writing it here would store the derived value over the remembered one,
      // erasing the only record of where the reader came from. That is the
      // exact bug EXIT_SEARCH's comment describes, and it is why leaving a
      // pivot can now put an ontology browser back in the ontology.
      //
      // factPath is NOT cleared, and that is the whole of the fix for two
      // faults that shared one cause. Clearing it here was justified by "the
      // refetched list selects its own first row" — but every list branch
      // already does that, and each does it only when the open fact did NOT
      // survive the refetch (api.recent, api.search and the lens rows hold the
      // same guard). So the clear decided nothing the fetch would not decide
      // better, while owning the gap between the two: with no fact to draw,
      // the right panel fell back to the ontology dashboard, which FLASHED up
      // mid-pivot and then STAYED whenever the pivot changed no query — the
      // same motif pinned again from a carrier's own header refetches nothing,
      // so nothing ever re-selected. Leaving the fact alone is also the better
      // answer on the common path: the motif was read off the open fact, so
      // that fact is a carrier and the reader keeps their place.
      const pathChip = s.filters.find(f => f.category === 'path');
      const motifChips = s.filters.filter(f => f.category === 'motif');
      const prevMotif = motifChips[0];
      // Nothing to do: this shape is already the query and there is no path to
      // displace, so the only thing a rebuild would add is a Back press that
      // returns to the identical view. EXIT_MOTIF guards its stray dispatch
      // the same way.
      //
      // ONE motif chip, counted — not "the first motif chip happens to match".
      // Chips typed into the FilterBar accumulate, so `motif:a` + `motif:b` is
      // a reachable union, and a `.find()` here would read the union's FIRST
      // member and no-op a pivot whose whole job is to collapse it: the reader
      // asks for one shape, keeps two, and the answer depends on which order
      // they typed them in. Collapsing is exactly what the arm below does.
      if (motifChips.length === 1 && prevMotif.value === a.motif && !pathChip) return s;
      const filters = s.filters.filter(f => f.category !== 'motif' && f.category !== 'path');
      const returnPath = pathChip?.value ?? prevMotif?.returnPath;
      return {
        ...s,
        filters: [...filters, { category: 'motif' as const, value: a.motif, ...(returnPath ? { returnPath } : {}) }],
        navStack: pushNav(s),
      };
    }
    case 'SET_FACT_SOURCE':
      return { ...s, factSource: a.source };
    case 'SET_REMOTE_ERROR':
      // Scoped to ONE side. sync_ok says the fetch half is healthy and says
      // NOTHING about the push half, so clearing both here is how a sync_ok
      // used to wipe a banner an expired push token had raised — leaving the
      // next failing push tick to put it straight back, once per reconcile
      // interval, for as long as the token stayed broken.
      //
      // Identity-stable when nothing changed. This action is dispatched by
      // POLLS (the persisted-status re-read on repo switch, reconnect, and
      // repo-manager close), not just by remote events, and re-confirming the
      // same value must not mint a new AppState and re-render every panel.
      if (a.side === 'push') {
        if (s.remotePushError === a.error) return s;
        return { ...s, remotePushError: a.error };
      }
      if (s.remoteSyncError === a.error) return s;
      return { ...s, remoteSyncError: a.error };
    case 'CLEAR_REMOTE_ERRORS':
      // The user dismissing the banner acknowledges BOTH sides — it is one
      // banner, and there is no way to acknowledge half of it. Nothing is lost:
      // a side that is still broken re-raises on its next failing tick.
      if (!s.remoteSyncError && !s.remotePushError) return s;
      return { ...s, remoteSyncError: '', remotePushError: '' };
    case 'FOCUS_RIGHT_PANEL':
      return { ...s, rightPanelFocused: true };
    case 'BLUR_RIGHT_PANEL':
      return { ...s, rightPanelFocused: false };
    case 'SET_AS_OF': {
      // Opening a fact's timeline (live -> non-live with a fact open) is a
      // drill-down like any other, so it records the departure state and Back
      // returns to the fact (#486). Only the ENTRY pushes: scrubbing between
      // versions, leaving a diff and returning to live stay in place, so a
      // scrub does not grow the stack and the trail. selectTrail collapses the
      // fact's live entry and its history entry into one crumb.
      const entering = s.factPath != null && s.asOf.mode === 'live' && a.asOf.mode !== 'live';
      return { ...s, asOf: a.asOf, ...(entering ? { navStack: pushNav(s) } : {}) };
    }
    case 'SET_LIBRARY_SORT':
      // Switching sort clears the selected fact so the right panel doesn't
      // strand a previous selection in the new view. Recent/Relevance modes
      // auto-select their first row after the fetch settles; Path mode
      // starts un-selected so the user picks deliberately from the tree.
      return { ...s, librarySort: a.sort, factPath: null };
    case 'RESTORE_LIST':
      // A page load, not a move: nothing is pushed, there is no trail to go
      // back along yet. sinceFork is held to the SET_SINCE_FORK guard: only
      // inside an experiment. lensSources only in a lens context, where a
      // mount selection means something.
      return {
        ...s,
        filters: a.filters,
        freeText: a.freeText,
        librarySort: a.sort,
        lensSources: s.context.kind === 'lens' ? a.lensSources : s.lensSources,
        sinceFork: s.experiment ? a.sinceFork : false,
        factPath: null,
      };
    case 'SET_NOTICE':
      return { ...s, notice: a.text };
    case 'CLEAR_NOTICE':
      return s.notice === '' ? s : { ...s, notice: '' };
    case 'SET_SEARCHING':
      return s.searching === a.value ? s : { ...s, searching: a.value };
    case 'SET_SERVER_READONLY':
      return { ...s, serverReadOnly: a.value };
    case 'SET_REPO_READONLY':
      return { ...s, repoReadOnly: a.value };
    case 'APPLY_NAV': {
      // Cycle-collapse: a subject hop (hop:true) to a fact already in the trail
      // unwinds to the existing crumb instead of pushing a duplicate. This is
      // the single chokepoint for ALL link-following navigation (edge refs,
      // in-body refs, timeline files-affected), so cycles can't accumulate no
      // matter which surface the hop came from. Deliberate navigations
      // (library selection, return-to-live) omit hop and always push.
      if (a.hop && a.factPath != null) {
        const plan = planTrailHop(selectTrail(s), a.factPath);
        if (plan.kind === 'unwind') {
          let next = s;
          for (let k = 0; k < plan.steps; k++) next = reducer(next, { type: 'NAV_BACK' });
          return next;
        }
      }
      return {
        ...s,
        view: a.view,
        factPath: a.factPath,
        asOf: a.asOf,
        filters: a.filters !== undefined ? a.filters : s.filters,
        freeText: a.freeText !== undefined ? a.freeText : s.freeText,
        librarySort: a.sort ?? s.librarySort,
        sinceFork: a.sinceFork ?? s.sinceFork,
        navStack: a.departed ? s.navStack : pushNav(s, a.fromChat),
        rightPanelFocused: false,
      };
    }
    case 'CHAT_RETURN_SERVED':
      return s.chatReturn ? { ...s, chatReturn: null } : s;
    case 'BRANCH_RECHECK_SERVED':
      return s.branchRecheck ? { ...s, branchRecheck: false } : s;
    case 'BRANCH_GONE':
      // The branch restored for a return from a chat no longer exists. Nothing on it,
      // or on the trail behind it, can be read; the status of the branch moved to
      // comes with this.
      return { ...s, view: 'library', factPath: null, asOf: { mode: 'live' }, filters: [], freeText: '', navStack: [], experiment: null, sinceFork: false, navReturn: null };
    case 'NAV_RETURN_ENTER': {
      // The context Back returns to exists: switch to it the way any switch does,
      // keeping the stack, which is the history Back is walking.
      const r = s.navReturn;
      if (!r || r.entered) return s;
      const next = reducer(s, { type: 'SET_CONTEXT', context: r.entry.context });
      return { ...next, navStack: s.navStack, navReturn: { ...r, entered: true } };
    }
    case 'NAV_RETURN_GONE': {
      // The context Back returns to is gone: the reader stays where they are, and
      // every entry left in it goes too, or each Back would be another dead step.
      const r = s.navReturn;
      if (!r) return s;
      const stack = [...s.navStack];
      while (stack.length > 0 && sameContext(stack[stack.length - 1].context, r.entry.context)) stack.pop();
      return { ...s, navReturn: null, navStack: stack };
    }
    case 'NAV_RETURN_BRANCH_GONE': {
      // The branch the entry was left on is gone. The context is entered and its
      // status is the branch on screen: its normal view, with that branch's own
      // experiment and since-fork as its status set them. The entries left on the
      // dead branch of this context go; entries of other contexts stay.
      const r = s.navReturn;
      if (!r) return s;
      const stack = [...s.navStack];
      const onDead = (e: NavEntry) => sameContext(e.context, r.entry.context) && e.branch === r.entry.branch;
      while (stack.length > 0 && onDead(stack[stack.length - 1])) stack.pop();
      return { ...s, navReturn: null, navStack: stack, view: 'library', factPath: null, asOf: { mode: 'live' }, filters: [], freeText: '' };
    }
    case 'NAV_RETURN_LAND': {
      // The entry's branch is the one on screen now: put back what the reader left.
      // Not a move, so nothing is pushed. Since-fork holds only in an experiment.
      const e = s.navReturn?.entry;
      if (!e || !s.navReturn?.entered) return s;
      return {
        ...s,
        navReturn: null,
        view: e.view,
        factPath: e.factPath,
        asOf: e.asOf,
        filters: e.filters,
        freeText: e.freeText,
        librarySort: e.sort,
        lensSources: e.lensSources,
        sinceFork: e.sinceFork && !!s.experiment,
        rightPanelFocused: false,
      };
    }
    case 'NAV_RETURN_DROP':
      return s.navReturn ? { ...s, navReturn: null } : s;
    case 'AMEND_NAV': {
      // In-place update — no navStack push. Used by auto-select behaviors so that
      // a single user action (e.g. view-button click) creates exactly one navStack entry.
      if (s.factPath === a.factPath && (a.asOf === undefined || JSON.stringify(s.asOf) === JSON.stringify(a.asOf))) return s;
      return {
        ...s,
        factPath: a.factPath,
        ...(a.asOf !== undefined ? { asOf: a.asOf } : {}),
      };
    }
    case 'CACHE_FACT_TITLE': {
      // No-op when nothing changes so a re-fire never triggers a needless render.
      if (!a.title || s.factTitles[a.key] === a.title) return s;
      return { ...s, factTitles: { ...s.factTitles, [a.key]: a.title } };
    }
    default:
      return s;
  }
}

// reducer wraps applyAction with a cross-cutting invariant: factSource is
// meaningful only while a fact is open. Whenever a fact closes (factPath -> null)
// — via APPLY_NAV/AMEND_NAV to null, NAVIGATE, GO_UP, filter changes, sort
// switch, context switch, etc. — drop the source mount so no direct reader can
// observe stale data. Centralizing this here means new fact-close paths inherit
// the guarantee for free. Referential identity is preserved for the common case
// (already-null factSource), so no-op arms that `return s` stay ===.
// The reader's own moves. One made while a Back to another context is still on its
// way (navReturn) wins: the return is dropped, or its landing would overwrite
// what the reader just chose. Not AMEND_NAV or RESTORE_LIST: those are the app's
// own (a list selecting its first row, a reload), not the reader's.
const READER_MOVES: ReadonlySet<Action['type']> = new Set<Action['type']>([
  'NAVIGATE', 'GO_UP', 'ADD_FILTER', 'REMOVE_FILTER', 'SET_FREE_TEXT', 'CLEAR_FILTERS', 'APPLY_NAV',
  'FOCUS_LENS_SOURCE', 'PIVOT_MOTIF', 'EXIT_SEARCH', 'EXIT_MOTIF', 'SET_LIBRARY_SORT', 'SET_LENS_SOURCES',
  'SET_SINCE_FORK', 'SET_AS_OF',
]);

export function reducer(s: AppState, a: Action): AppState {
  return reduce(s.navReturn && READER_MOVES.has(a.type) ? { ...s, navReturn: null } : s, a);
}

function reduce(s: AppState, a: Action): AppState {
  const next = applyAction(s, a);
  if (next.factPath === null && next.factSource !== null) {
    return { ...next, factSource: null };
  }
  return next;
}

// FactReading is what the fact view (FactView) reads: which fact, from where, at
// which commit, and whether it can be written. Browse's whole AppState is one; the
// chat's side panel builds one for the fact it shows. Every selector below takes
// this rather than AppState, so both go through the same rules.
export type FactReading = Pick<AppState,
  'context' | 'lens' | 'lensWriteId' | 'repo' | 'branch' | 'factPath' | 'factSource' | 'asOf' | 'headCommit'
  | 'serverReadOnly' | 'repoReadOnly' | 'branchWritable'>;

// openFactSourceResolved says whether factSource is the mount of the fact
// state.factPath names — i.e. whether pairing the two addresses a real fact.
// Outside a lens there is no per-fact mount, so it is always true.
//
// factSource is re-set only once getLensFact answers for a NEW path, so for one
// stretch after the open fact changes it still names the PREVIOUS fact's mount.
// A reader that pairs it with the NEW path (useFactEdges, synchronously) asked
// alpha's route for a beta fact: two 404s on /incoming and /outgoing. Such a
// reader waits on this, exactly as it waits for a null source on a first open.
//
// It is deliberately NOT applied by clearing factSource in the reducer: the
// fact view keeps rendering the previous fact until its replacement loads, and
// pairs it — coherently — with the previous source (VersionWalker's commits).
// Clearing it sends that render to the write-repo fallback {write, ''}.
//
// The path is the canonical lens address (decision 10a3bcc0): a read mount's
// fact is kb://<id12>/…, owned by the source with that id; a bare path is the
// write repo's, owned by the source whose id is the write member's
// (lensWriteId) — by identity, so a read mount that happens to share the write
// repo's NAME is never taken for it. Only when the repo listing carried no id
// (an older server) does the name stand in. A hop within one mount is resolved
// at once.
export function openFactSourceResolved(s: FactReading): boolean {
  if (s.context.kind !== 'lens') return true;
  const src = s.factSource;
  if (!s.factPath || !src) return false;
  const m = /^kb:\/\/([^/]+)\//.exec(s.factPath);
  if (m) return src.id === m[1];
  if (!s.lens) return false;
  return s.lensWriteId ? src.id === s.lensWriteId : src.repo === s.lens.write.name;
}

export function selectAnchorCommit(s: FactReading): string | null {
  switch (s.asOf.mode) {
    case 'live':     return null;
    case 'history': return s.asOf.commit;
    case 'diff':     return s.asOf.to;
  }
}

export function isLive(s: FactReading): boolean {
  return s.asOf.mode === 'live';
}

// isLensContext is true when the app is browsing a lens (rather than a repo).
export function isLensContext(s: FactReading): boolean {
  return s.context.kind === 'lens';
}

// lensResolutionPending is true when the context names a lens that state.lens
// does not yet reflect — the signal for App's resolution effect. A lens context
// can be entered from several surfaces (TopBar switcher, manager Browse,
// bootstrap restore); they all just dispatch SET_CONTEXT, and App resolves
// whenever this returns true. Same-name re-resolution after an edit is
// deliberately NOT covered (state.lens.name already matches) — that path goes
// through refreshContextAfterChange.
export function lensResolutionPending(s: AppState): boolean {
  return s.context.kind === 'lens' && s.lens?.name !== s.context.name;
}

// openFactSource is THE temporal/write anchor for the current view:
//   - repo context      → {state.repo, state.branch} (authoritative)
//   - lens context, fact open → the open fact's source mount (repo + the branch
//     it was read at), so writes/history for that fact route to where it lives.
//   - lens context, no fact   → the lens's write mount, {lens.write, ''}.
// A branch of '' is the "resolve the agent branch" sentinel: callers translate
// it (via api.getAgentBranch) before issuing a write. A closed fact (factPath
// null) is treated as "no fact open" so a stale factSource can never leak past
// a fact close — the fact-open path re-sets factSource for the next lens fact.
export function openFactSource(s: FactReading): { repo: string; branch: string } {
  if (s.context.kind === 'lens') {
    if (s.factPath && s.factSource) {
      return { repo: s.factSource.repo, branch: s.factSource.branch };
    }
    if (s.lens) return { repo: s.lens.write.name, branch: '' };
  }
  return { repo: s.repo, branch: s.branch };
}

// factHistoryAnchor is the READ anchor for a fact's history/edges: the open
// fact's source mount (openFactSource) paired with the RELATIVE path (the
// kb://<id12>/ qualifier stripped via displayLensPath). Co-locating repo,
// branch, AND path in one helper keeps the three dimensions from drifting apart
// at the call sites (LeftPanel/App/RightPanel/useTimeTravel) — the drift that
// let a browse-surface commit leak against a read mount. `path` defaults to the
// open fact; pass the loaded fact's own canonical path where it differs. Repo
// context: {state.repo, state.branch, <bare path>} — byte-identical to the old
// inline pairing.
export function factHistoryAnchor(
  s: FactReading,
  path: string | null = s.factPath,
): { repo: string; branch: string; path: string } {
  const { repo, branch } = openFactSource(s);
  return { repo, branch, path: displayLensPath(path ?? '') };
}

// edgeAnchorCommit is the commit useFactEdges anchors on. Time-travelling: the
// history/diff anchor (selectAnchorCommit) — a commit drawn from the OPEN FACT's
// own mount timeline, so it resolves against that mount. Live: the mount's live
// HEAD — state.headCommit in a repo context (the repo's own head), but '' (no
// commit → the non-anchored live-HEAD explain URL) in a lens context, where the
// open fact's mount head isn't tracked in state and state.headCommit is the
// WRITE repo's head (which doesn't exist in a read mount → an empty edge set in
// the default live view). Repo context is byte-identical (always state.headCommit).
export function edgeAnchorCommit(s: FactReading): string {
  return selectAnchorCommit(s) ?? (isLensContext(s) ? '' : s.headCommit);
}

// remoteErrorText is the single line the banner shows for an unhealthy remote,
// and the condition everything else gates on: empty means healthy on BOTH sides.
// The two halves are tracked apart (see remoteSyncError / remotePushError) but
// only one message fits one banner, so the fetch side wins when both are set —
// a fetch that cannot reach origin is the more fundamental failure, and the push
// error is usually a consequence of it rather than separate news.
export function remoteErrorText(s: AppState): string {
  return s.remoteSyncError || s.remotePushError;
}

export function isReadOnly(s: FactReading): boolean {
  return s.serverReadOnly || s.repoReadOnly || !s.branchWritable || !isLive(s);
}

export const READ_ONLY_TITLE = 'Read-only — anchor is not live';
export const SUBSCRIPTION_TITLE = 'Read-only — this repo follows a remote branch';
export const BRANCH_TITLE = 'Read-only — facts are not authored on this branch';

// readOnlyTitle names WHY the surface is read-only, most specific reason first.
// A subscription is read-only while the anchor is perfectly live, so the
// temporal wording would be actively wrong there — and it is the one reason the
// reader cannot fix by scrubbing back to live.
export function readOnlyTitle(s: FactReading): string {
  if (s.repoReadOnly) return SUBSCRIPTION_TITLE;
  // Ordered most-specific-first, like the subscription case above and for the
  // same reason: a branch that cannot be authored is read-only while the
  // anchor is perfectly live, so the temporal wording would send the reader
  // to scrub back to a "live" they are already on.
  if (!s.branchWritable) return BRANCH_TITLE;
  return READ_ONLY_TITLE;
}

export interface TrailCrumb {
  factPath: string;
  asOf: AsOf;
  // NAV_BACK pops that land on this crumb from the current view. Not the
  // crumb's index distance: one crumb can stand for several stack entries (a
  // fact's live entry and its timeline entry). Absent = index distance.
  pops?: number;
}

// The current view is the last crumb. In a history excursion the trail also
// includes the prior subject hops back to the live root (the most recent
// fact-bearing entry that was live). Pure time-scrubs (SET_AS_OF within a
// timeline, no navStack push) don't add crumbs — only subject hops (APPLY_NAV
// with a factPath) do. Entering a fact's timeline pushes the fact's live entry
// (#486); adjacent entries for the SAME fact collapse into one crumb, so the
// trail never lists a fact twice. A run that includes the current view keeps
// the current view; any other run keeps its oldest entry (jumping there pops
// the whole run).
export function selectTrail(s: AppState): TrailCrumb[] {
  // No fact open (Browse as of a commit, say) is not a crumb: a crumb names a
  // fact, and an empty path is neither a title to fetch nor a place to jump to.
  const current: TrailCrumb = { factPath: s.factPath ?? '', asOf: s.asOf, pops: 0 };
  const noFact = current.factPath === '';
  if (isLive(s)) return noFact ? [] : [current];
  const prefix: TrailCrumb[] = [];
  for (let i = s.navStack.length - 1; i >= 0; i--) {
    const e = s.navStack[i];
    // Back from here is the chat, so what Browse held before it is not a crumb.
    if (e.fromChat) break;
    // Nor is a fact in another repo or lens: a crumb is a hop inside this one.
    if (!sameContext(e.context, s.context)) break;
    if (e.factPath == null) continue;
    prefix.unshift({ factPath: e.factPath, asOf: e.asOf, pops: s.navStack.length - i });
    if (e.asOf.mode === 'live') break;
  }
  const out: TrailCrumb[] = [];
  for (const c of noFact ? prefix : [...prefix, current]) {
    const last = out[out.length - 1];
    if (last && last.factPath === c.factPath) {
      if (c === current) out[out.length - 1] = c;
      continue;
    }
    out.push(c);
  }
  return out;
}

/**
 * Decide how a subject hop to `targetPath` should affect the trail.
 *
 * If the target fact already appears in the trail, the user is revisiting a
 * fact they came from (A → B → A …). Rather than push a duplicate crumb — which
 * grows a repeating cycle in the breadcrumb — unwind back to the existing crumb.
 * `steps` is how many NAV_BACK pops land on it (0 = already current, a no-op).
 * Otherwise push a fresh crumb. Matched on `factPath` (subject identity), so a
 * revisit at a different version still collapses to the crumb already there.
 */
export function planTrailHop(
  trail: TrailCrumb[],
  targetPath: string,
): { kind: 'unwind'; steps: number } | { kind: 'push' } {
  const depth = trail.length - 1;
  const i = trail.findIndex(c => c.factPath === targetPath);
  if (i >= 0) return { kind: 'unwind', steps: trail[i].pops ?? depth - i };
  return { kind: 'push' };
}
