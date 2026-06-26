import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { LeftPanel } from './LeftPanel';
import { init } from './state';
import type { AppState, AsOf } from './state';

vi.mock('./api', () => ({
  api: {
    browse: vi.fn().mockResolvedValue({ children: [] }),
    search: vi.fn().mockResolvedValue({ results: [] }),
    recent: vi.fn().mockResolvedValue({ facts: [], total: 0 }),
  },
}));

function setup(asOf: AsOf, overrides: Partial<AppState> = {}) {
  const state: AppState = {
    ...init,
    repo: 'knomit',
    branch: 'machine/test',
    asOf,
    ...overrides,
  };
  return render(<LeftPanel state={state} dispatch={vi.fn()} navigate={vi.fn()} />);
}

describe('Library — read-only banner', () => {
  it('does not render banner in live mode', () => {
    setup({ mode: 'live' });
    expect(screen.queryByTestId('library-readonly-banner')).toBeNull();
  });

  it('renders banner when history', () => {
    setup({ mode: 'history', commit: 'b812d40' });
    const banner = screen.getByTestId('library-readonly-banner');
    expect(banner.textContent).toContain('history views not yet supported');
  });

  it('renders banner when diff', () => {
    setup({ mode: 'diff', from: 'aaa1111', to: 'bbb2222' });
    expect(screen.getByTestId('library-readonly-banner')).toBeInTheDocument();
  });
});
