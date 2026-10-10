import type { Dispatch, MouseEvent as ReactMouseEvent } from 'react';
import type { AppState, Action } from './state';
import { isLive, isLensContext } from './state';
import type { RepoCreateStatus } from './api';
import type { NavRequest } from './useNavigationManager';
import type { useTimeTravel } from './useTimeTravel';
import type { FactEdges } from './useFactEdges';
import { ErrorBoundary } from './ErrorBoundary';
import { ExperimentBand } from './ExperimentBand';
import { FacetToolbar } from './FacetToolbar';
import { BranchPicker } from './BranchPicker';
import { MountsPicker } from './MountsPicker';
import { FilterBar } from './FilterBar';
import { LeftPanel } from './LeftPanel';
import { RightPanel } from './RightPanel';

interface Props {
  state: AppState;
  dispatch: Dispatch<Action>;
  navigate: (req: NavRequest) => void;
  tt: ReturnType<typeof useTimeTravel>;
  edges: FactEdges;
  repoNames: Record<string, string>;
  /** The create still working on the browsed repo, if any. */
  repoCreate?: RepoCreateStatus;
  leftPanelWidth: number;
  libraryNarrow: boolean;
  onSplitterDown: (e: ReactMouseEvent) => void;
  onJumpTrail: (i: number) => void;
  onEnterBranch: (branch: string) => void;
}

// BrowseFacet is the reading page: what the app was before the shell had
// levels. Above the panes sit the bands that describe what is being read (the
// experiment band, the create or index banner) and the Browse toolbar with the
// branch or mounts picker and the filter. Those two moved here from the top
// bar: they act on the data being read, so they belong to the page that reads
// it and not to the chrome every page shares.
export function BrowseFacet({
  state, dispatch, navigate, tt, edges, repoNames, repoCreate,
  leftPanelWidth, libraryNarrow, onSplitterDown, onJumpTrail, onEnterBranch,
}: Props) {
  const live = isLive(state);
  return (
    <div data-testid="browse-facet" style={{ flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column', overflow: 'hidden' }}>
      {/* The experiment band. Its presence is keyed on state.experiment — the
          branch row's own object — never on an `exp/` prefix, for the reason
          the server gives: the record is what makes a branch an experiment,
          the name is only a convention. */}
      {state.experiment && (
        <ErrorBoundary variant="inline" label="The experiment band hit an error">
          <ExperimentBand
            repo={state.repo}
            branch={state.branch}
            experiment={state.experiment}
            onEnterBranch={onEnterBranch}
          />
        </ErrorBoundary>
      )}
      {/* THE CREATE BANNER REPLACES THE INDEX BANNER, it does not stack with
          it. While a job is still working on this repo, "Indexing…" is a true
          statement about a detail and a misleading one about the whole: the
          repository is not finished being made, and if the create is being
          cancelled it is on its way out. Reporting the index instead is how a
          reader ended up watching a progress bar for a repository that was
          being deleted underneath them. */}
      {repoCreate && (
        <div data-testid="repo-create-banner"
          data-create-state={repoCreate.state}
          style={{
            background: repoCreate.state === 'cancelling' ? '#2a200e' : '#1c2b1c',
            color: repoCreate.state === 'cancelling' ? '#f5c47a' : '#9c9',
            fontSize: 12, padding: '4px 14px',
            borderBottom: '1px solid ' + (repoCreate.state === 'cancelling' ? '#a36a18' : '#2a3a2a'),
            flexShrink: 0, display: 'flex', alignItems: 'center', gap: 8,
          }}>
          {repoCreate.state === 'cancelling' ? (
            <>
              <span>⟳ Cancelling…</span>
              <span style={{ color: '#a08a54' }}>this repository is being removed</span>
            </>
          ) : (
            <>
              <span>⟳ Creating…</span>
              <span style={{ color: '#6a8a6a' }}>search and lists may be incomplete until this finishes</span>
            </>
          )}
        </div>
      )}
      {!repoCreate && state.indexState === 'indexing' && (
        <div data-testid="indexing-banner" style={{ background: '#1c2b1c', color: '#9c9', fontSize: 12, padding: '4px 14px', borderBottom: '1px solid #2a3a2a', flexShrink: 0, display: 'flex', alignItems: 'center', gap: 8 }}>
          <span>⟳ Indexing{state.indexTotal > 0 ? ` ${state.indexPercent}% (${state.indexDone}/${state.indexTotal})` : '…'}</span>
          <span style={{ color: '#6a8a6a' }}>search and lists may be incomplete until this finishes</span>
        </div>
      )}
      {state.indexState === 'error' && (
        <div data-testid="index-error-banner" style={{ background: '#2b1c1c', color: '#e0a0a0', fontSize: 12, padding: '4px 14px', borderBottom: '1px solid #3a2a2a', flexShrink: 0, display: 'flex', alignItems: 'center', gap: 8 }}>
          <span>⚠ Indexing did not complete — search and lists may be incomplete. It will retry on the next restart.</span>
        </div>
      )}

      {/* The Browse toolbar: the scope (which branch of the repo, or which
          mounts of the lens), then the filter, which takes the remaining width
          so it is the element that absorbs a narrowing window. History mode
          keeps the picker and drops the filter: there is no filtering while
          anchored, and the trail breadcrumb over the content pane takes its
          place. */}
      <FacetToolbar testId="browse-toolbar">
        {isLensContext(state)
          ? state.lens && <MountsPicker lens={state.lens} selection={state.lensSources} dispatch={dispatch} />
          : <BranchPicker state={state} onEnterBranch={onEnterBranch} />}
        {live && (
          <div data-testid="browse-search" style={{ flex: 1, minWidth: 0 }}>
            <ErrorBoundary variant="inline" label="Search hit an error">
              <FilterBar state={state} dispatch={dispatch} embedded />
            </ErrorBoundary>
          </div>
        )}
      </FacetToolbar>

      {/* Unified now/history surface: a rotating LeftPanel (Library ⇄ timeline
          nav), a trail-aware FilterBar, and the fact RightPanel — which carries
          the connections panel in its header. Time-travel (scrub/hop/
          return-to-now) routes through `tt` so the same layout serves live and
          history reads. */}
      <div style={{ display: 'flex', flex: 1, overflow: 'hidden', minHeight: 0 }}>
        <div style={{ width: leftPanelWidth, flexShrink: 0, overflow: 'hidden', display: 'flex', flexDirection: 'column' }}>
          <ErrorBoundary variant="inline" label="The library hit an error">
            <LeftPanel state={state} dispatch={dispatch} navigate={navigate} onScrub={tt.scrub} onOpenFileAt={tt.openFileAt} onReturnToLive={tt.returnToNow} narrow={libraryNarrow} />
          </ErrorBoundary>
        </div>
        {/* Drag handle. 4px visible separator + 8px hit zone via negative
            margins on either side so the cursor target is easier to grab
            than the visible line. */}
        <div
          data-testid="library-splitter"
          onMouseDown={onSplitterDown}
          title="Drag to resize"
          style={{
            width: 4, marginLeft: -2, marginRight: -2,
            cursor: 'ew-resize', flexShrink: 0, zIndex: 1,
            background: 'transparent',
            borderLeft: '1px solid #222',
          }}
          onMouseEnter={e => { (e.currentTarget as HTMLElement).style.background = 'rgba(136,170,255,0.15)'; }}
          onMouseLeave={e => { (e.currentTarget as HTMLElement).style.background = 'transparent'; }}
        />
        <div style={{ flex: 1, overflow: 'hidden', minWidth: 0, display: 'flex', flexDirection: 'column' }}>
          {/* Anchored reads swap the filter input for the trail breadcrumb.
              It stays over the content pane because it describes the hop
              path that got you to the open FACT, not the list. */}
          {!live && (
            <ErrorBoundary variant="inline" label="The trail bar hit an error">
              <FilterBar state={state} dispatch={dispatch} onJumpTrail={onJumpTrail} />
            </ErrorBoundary>
          )}
          <div style={{ flex: 1, minHeight: 0, display: 'flex', overflow: 'hidden' }}>
            <div style={{ flex: 1, minWidth: 0, overflow: 'hidden' }}>
              <ErrorBoundary variant="inline" label="This fact could not be displayed">
                <RightPanel
                  state={state}
                  dispatch={dispatch}
                  navigate={navigate}
                  onScrub={tt.scrub}
                  onHopRef={tt.hopEdge}
                  repoNames={repoNames}
                  refCommits={edges.refCommits}
                  incoming={edges.incoming}
                  outgoing={edges.outgoing}
                  edgesError={edges.error}
                  onHopEdge={tt.hopEdge}
                />
              </ErrorBoundary>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}
