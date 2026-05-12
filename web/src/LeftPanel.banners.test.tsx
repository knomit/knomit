import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { LeftPanel } from './LeftPanel';
import { init } from './state';
import type { AppState, AsOf, View } from './state';

vi.mock('./api', () => ({
  api: {
    browse: vi.fn().mockResolvedValue({ children: [] }),
    search: vi.fn().mockResolvedValue({ results: [] }),
    recent: vi.fn().mockResolvedValue({ facts: [], total: 0 }),
    history: vi.fn().mockResolvedValue({ entries: [] }),
  },
}));

function setup(view: View, asOf: AsOf, overrides: Partial<AppState> = {}) {
  const state: AppState = {
    ...init,
    repo: 'knomit',
    branch: 'machine/test',
    view,
    asOf,
    ...overrides,
  };
  return render(<LeftPanel state={state} dispatch={vi.fn()} navigate={vi.fn()} />);
}

describe('LeftPanel — read-only banners', () => {
  it('does not render tree banner in live mode', () => {
    setup('tree', { mode: 'live' });
    expect(screen.queryByTestId('tree-readonly-banner')).toBeNull();
  });

  it('renders tree banner with correct copy when scrubbed', () => {
    setup('tree', { mode: 'scrubbed', commit: 'b812d40' });
    const banner = screen.getByTestId('tree-readonly-banner');
    expect(banner).toBeInTheDocument();
    expect(banner.textContent).toContain('Showing live tree');
    expect(banner.textContent).toContain('scrubbed views not yet supported by backend');
  });

  it('renders tree banner when in diff mode', () => {
    setup('tree', { mode: 'diff', from: 'aaa1111', to: 'bbb2222' });
    expect(screen.getByTestId('tree-readonly-banner')).toBeInTheDocument();
  });

  it('does not render chrono banner in live mode', () => {
    setup('chrono', { mode: 'live' });
    expect(screen.queryByTestId('chrono-readonly-banner')).toBeNull();
  });

  it('renders chrono banner with correct copy when scrubbed', () => {
    setup('chrono', { mode: 'scrubbed', commit: 'b812d40' });
    const banner = screen.getByTestId('chrono-readonly-banner');
    expect(banner).toBeInTheDocument();
    expect(banner.textContent).toContain('Showing live activity');
    expect(banner.textContent).toContain('listFacts is HEAD-only');
  });

  it('renders chrono banner when in diff mode', () => {
    setup('chrono', { mode: 'diff', from: 'aaa1111', to: 'bbb2222' });
    expect(screen.getByTestId('chrono-readonly-banner')).toBeInTheDocument();
  });
});
