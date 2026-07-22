import { it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { LeftPanel } from './LeftPanel';
// Mock Library + TimelineNav to identifiable stubs
vi.mock('./Library', () => ({ Library: () => <div>LIBRARY</div> }));
vi.mock('./TimelineNav', () => ({ TimelineNav: () => <div>TIMELINE</div> }));

it('shows Library when live, TimelineNav when history', () => {
  // Minimal AppState: context is required now that LeftPanel derives the
  // TimelineNav anchor via openFactSource (Task 17).
  const base = { context: { kind: 'repo', repo: 'r' } } as any;
  const { rerender } = render(<LeftPanel state={{ ...base, asOf: { mode: 'live' }, factPath: 'kb/a.md' }} dispatch={vi.fn()} navigate={vi.fn()} onScrub={vi.fn()} onOpenFileAt={vi.fn()} />);
  expect(screen.getByText('LIBRARY')).toBeInTheDocument();
  rerender(<LeftPanel state={{ ...base, asOf: { mode: 'history', commit: 'c1' }, factPath: 'kb/a.md' }} dispatch={vi.fn()} navigate={vi.fn()} onScrub={vi.fn()} onOpenFileAt={vi.fn()} />);
  expect(screen.getByText('TIMELINE')).toBeInTheDocument();
});
