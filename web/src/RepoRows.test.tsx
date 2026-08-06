// Tests for RepoRows — the lens summary's per-mount list. The section is a
// ranked table, not a stack of cards: order, share width, the activity meter
// and the empty-mount state are the parts a style tweak could silently break.

import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { RepoRows } from './RepoRows';
import type { LensRepoStats } from './api';

function repo(over: Partial<LensRepoStats> = {}): LensRepoStats {
  return {
    id: 'id0123456789', name: 'core', source: '', branch: 'main', is_write: false,
    total: 100, avg_confidence: 0.8, domains: { ai: 9, go: 4 }, entities: {},
    last_commit: '2026-07-20T10:00:00Z', changes_7d: 1, changes_30d: 2, changes_90d: 3,
    ...over,
  };
}

const rows = () => screen.getAllByTestId('lens-repo-row');
const names = () => rows().map(r => r.getAttribute('data-repo'));

describe('RepoRows', () => {
  it('ranks mounts by fact count descending, whatever order the server sent', () => {
    render(<RepoRows repos={[
      repo({ name: 'small', id: 'a', total: 9 }),
      repo({ name: 'big', id: 'b', total: 1377 }),
      repo({ name: 'mid', id: 'c', total: 376 }),
    ]} dispatch={vi.fn()} />);
    expect(names()).toEqual(['big', 'mid', 'small']);
  });

  it('breaks a count tie on name, so two renders of one corpus cannot disagree', () => {
    render(<RepoRows repos={[
      repo({ name: 'zeta', id: 'a', total: 50 }),
      repo({ name: 'alpha', id: 'b', total: 50 }),
    ]} dispatch={vi.fn()} />);
    expect(names()).toEqual(['alpha', 'zeta']);
  });

  it('sizes each share bar against the largest mount, not against the total', () => {
    render(<RepoRows repos={[
      repo({ name: 'big', id: 'a', total: 200 }),
      repo({ name: 'quarter', id: 'b', total: 50 }),
    ]} dispatch={vi.fn()} />);
    const bars = screen.getAllByTestId('repo-share');
    expect(bars[0].style.width).toBe('100%');
    expect(bars[1].style.width).toBe('25%');
  });

  it('renders the three change buckets the payload carries, and no fourth', () => {
    render(<RepoRows repos={[repo({ changes_7d: 4, changes_30d: 12, changes_90d: 30 })]}
      dispatch={vi.fn()} />);
    const meter = screen.getByTestId('repo-activity');
    expect(meter.getAttribute('data-activity')).toBe('4/12/30');
    expect(meter.querySelectorAll('i')).toHaveLength(3);
    expect(meter.getAttribute('title')).toContain('4 in 7d');
    expect(meter.getAttribute('title')).toContain('30 in 90d');
  });

  it('scales the meter across mounts, so a busy repo dwarfs a quiet one', () => {
    render(<RepoRows repos={[
      repo({ name: 'busy', id: 'a', changes_7d: 90, changes_30d: 300, changes_90d: 800 }),
      repo({ name: 'quiet', id: 'b', changes_7d: 0, changes_30d: 1, changes_90d: 2 }),
    ]} dispatch={vi.fn()} />);
    const [busy, quiet] = screen.getAllByTestId('repo-activity');
    const h = (m: HTMLElement, i: number) =>
      parseFloat((m.querySelectorAll('i')[i] as HTMLElement).style.height);
    expect(h(busy, 2)).toBeGreaterThan(h(quiet, 2));
  });

  it('marks the write repo and no other', () => {
    render(<RepoRows repos={[
      repo({ name: 'core', id: 'a', is_write: true }),
      repo({ name: 'docs', id: 'b', total: 20 }),
    ]} dispatch={vi.fn()} />);
    const markers = screen.getAllByTestId('write-marker');
    expect(markers).toHaveLength(1);
    expect(rows()[0].contains(markers[0])).toBe(true);
  });

  it('shows the mount top domain and its recency', () => {
    render(<RepoRows repos={[repo({ domains: { store: 12, web: 3 } })]} dispatch={vi.fn()} />);
    expect(rows()[0].textContent).toContain('store');
    expect(rows()[0].textContent).toContain('100 facts');
    expect(rows()[0].textContent).toContain('0.80');
  });

  it('picking a mount filters by it — the repo chip category lens context already has', () => {
    const dispatch = vi.fn();
    render(<RepoRows repos={[repo({ name: 'docs' })]} dispatch={dispatch} />);
    fireEvent.click(rows()[0]);
    expect(dispatch).toHaveBeenCalledWith({
      type: 'ADD_FILTER', chip: { category: 'repo', value: 'docs' },
    });
  });

  it('renders a fact-less mount dimmed, with no confidence to report', () => {
    // A mount can hold nothing — freshly added, or emptied. The row must still
    // appear (it is a real mount) but must not claim a 0.00 confidence.
    render(<RepoRows repos={[
      repo({ name: 'core', id: 'a', total: 300 }),
      repo({ name: 'fresh', id: 'b', total: 0, avg_confidence: 0, domains: {},
        last_commit: '', changes_7d: 0, changes_30d: 0, changes_90d: 0 }),
    ]} dispatch={vi.fn()} />);
    const empty = rows()[1];
    expect(empty.getAttribute('data-empty')).toBe('true');
    expect(empty.textContent).toContain('0 facts');
    expect(empty.textContent).not.toContain('0.00');
  });

  it('survives a corpus where every mount is empty rather than dividing by zero', () => {
    render(<RepoRows repos={[repo({ total: 0, changes_7d: 0, changes_30d: 0, changes_90d: 0 })]}
      dispatch={vi.fn()} />);
    expect(screen.getByTestId('repo-share').style.width).toBe('0%');
  });

  it('renders nothing at all when the lens has no mounts', () => {
    const { container } = render(<RepoRows repos={[]} dispatch={vi.fn()} />);
    expect(container.textContent).toBe('');
  });

  it('heads the section with the mount count', () => {
    render(<RepoRows repos={[repo({ name: 'a', id: 'a' }), repo({ name: 'b', id: 'b' })]}
      dispatch={vi.fn()} />);
    expect(screen.getByTestId('repo-rows').textContent).toContain('Repos · 2');
  });
});
