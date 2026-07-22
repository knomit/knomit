import { memo, type Dispatch } from 'react';
import type { AppState, Action } from './state';
import { isLive, selectAnchorCommit, factHistoryAnchor } from './state';
import type { NavRequest } from './useNavigationManager';
import { Library } from './Library';
import { TimelineNav } from './TimelineNav';

interface Props {
  state: AppState;
  dispatch: Dispatch<Action>;
  navigate: (req: NavRequest) => void;
  onScrub?: (commit: string) => void;
  onOpenFileAt?: (path: string, commit: string) => void;
  onReturnToLive?: () => void;
}

// Check once whether the user prefers reduced motion so we can skip the
// transition animation entirely if requested. Guard against jsdom/SSR where
// matchMedia may be absent.
const prefersReducedMotion =
  typeof window !== 'undefined' &&
  typeof window.matchMedia === 'function' &&
  window.matchMedia('(prefers-reduced-motion: reduce)').matches;

const SLIDE_PX = 24;
const TRANSITION = prefersReducedMotion ? 'none' : 'transform 320ms ease';

export const LeftPanel = memo(function LeftPanel({ state, dispatch, navigate, onScrub, onOpenFileAt, onReturnToLive }: Props) {
  const live = isLive(state);
  const anchorCommit = selectAnchorCommit(state);
  // The open fact's history is anchored on its SOURCE MOUNT + RELATIVE path
  // (factHistoryAnchor): {state.repo, state.branch, bare-path} in a repo context,
  // the fact's read mount + kb://<id12>/-stripped path in a lens context.
  const hist = factHistoryAnchor(state);

  // No-op defaults so callers (e.g. App before Task 17) don't need to supply
  // these props while the real callbacks aren't wired yet.
  const handleScrub = onScrub ?? (() => {});
  const handleOpenFileAt = onOpenFileAt ?? (() => {});
  const handleReturnToLive = onReturnToLive ?? (() => {});

  // Cross-slide: the entering layer slides in from +24px (history) or -24px
  // (live); the leaving layer slides out in the opposite direction. Both layers
  // occupy the same slot (position: absolute, fill parent). Only the active
  // layer is visible (opacity 1, z-index above).
  return (
    <div style={{ position: 'relative', width: '100%', height: '100%', overflow: 'hidden' }}>
      {/* Library layer — rendered when live */}
      <div
        style={{
          position: 'absolute', inset: 0,
          opacity: live ? 1 : 0,
          pointerEvents: live ? 'auto' : 'none',
          zIndex: live ? 1 : 0,
          transform: live ? 'translateX(0)' : `translateX(-${SLIDE_PX}px)`,
          transition: TRANSITION,
        }}
      >
        <Library state={state} dispatch={dispatch} navigate={navigate} />
      </div>

      {/* TimelineNav layer — rendered when history */}
      <div
        style={{
          position: 'absolute', inset: 0,
          opacity: live ? 0 : 1,
          pointerEvents: live ? 'none' : 'auto',
          zIndex: live ? 0 : 1,
          transform: live ? `translateX(${SLIDE_PX}px)` : 'translateX(0)',
          transition: TRANSITION,
        }}
      >
        {!live && state.factPath && anchorCommit ? (
          <TimelineNav
            repo={hist.repo}
            branch={hist.branch}
            factPath={hist.path}
            activeCommit={anchorCommit}
            onScrub={handleScrub}
            onOpenFileAt={handleOpenFileAt}
            onReturnToLive={handleReturnToLive}
          />
        ) : null}
      </div>
    </div>
  );
});
