import type { CSSProperties, ReactNode } from 'react';

interface Props {
  children: ReactNode;
  /** Defaults to `facet-toolbar`; a facet names its own (`browse-toolbar`). */
  testId?: string;
}

// FacetToolbar is the shell's second level: the optional 40px row a facet
// gets when it has controls of its own. Only a facet that acts on what it
// shows has one — Browse's branch picker and filter, later Activity's counts
// and Logs' level control. It never carries anything about the target itself;
// that is the top bar's label.
export function FacetToolbar({ children, testId = 'facet-toolbar' }: Props) {
  return <div data-testid={testId} style={toolbar}>{children}</div>;
}

const toolbar: CSSProperties = {
  height: 40, flexShrink: 0,
  display: 'flex', alignItems: 'center', gap: 10,
  padding: '0 12px',
  background: '#0f0f0f', borderBottom: '1px solid #1c1c1c',
  fontSize: 12, minWidth: 0,
};
