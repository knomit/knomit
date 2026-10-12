import { createContext, useContext } from 'react';
import type { MouseEvent as ReactMouseEvent } from 'react';

// Handed down through context rather than props because the pages that use it
// (RepoSettings, LensSettings, and the server pages later) are reached through
// layers that have no business knowing about a splitter.

export interface FacetLayout {
  /** The Library's width, in px. */
  width: number;
  /** Starts a splitter drag. Absent where no splitter can be dragged. */
  onSplitterDown?: (e: ReactMouseEvent) => void;
}

// Outside the facet shell (a page mounted alone, as in a unit test) there is
// no Library to line up with: a fixed index and no splitter.
const FacetLayoutContext = createContext<FacetLayout>({ width: 200 });

export const FacetLayoutProvider = FacetLayoutContext.Provider;

export function useFacetLayout(): FacetLayout {
  return useContext(FacetLayoutContext);
}

