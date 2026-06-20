import { it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { TrailBreadcrumb } from './TrailBreadcrumb';
import { api } from './api';

vi.mock('./api', async (orig) => {
  const mod = await orig<typeof import('./api')>();
  return { ...mod, api: { ...mod.api, fact: vi.fn() } };
});

beforeEach(() => {
  (api.fact as any).mockImplementation(async (_r: string, _b: string, path: string) => ({
    path, title: path === 'kb/a.md' ? 'Alpha fact title' : 'Beta fact title',
    body: '', domain: [], confidence: 0, sources: 0, entities: [], refs: [],
  }));
});

const trail = [
  { factPath: 'kb/a.md', asOf: { mode: 'live' as const } },
  { factPath: 'kb/b.md', asOf: { mode: 'scrubbed' as const, commit: 'bbb1111' } },
];

it('fires jump (by index) and return-to-now', async () => {
  const onJump = vi.fn(); const onReturnToNow = vi.fn();
  render(<TrailBreadcrumb repo="r" branch="b" trail={trail} onJump={onJump} onReturnToNow={onReturnToNow} />);
  await waitFor(() => screen.getByText('Alpha fact title'));
  fireEvent.click(screen.getByText('Alpha fact title'));
  expect(onJump).toHaveBeenCalledWith(0);
  fireEvent.click(screen.getByText(/return to now/i));
  expect(onReturnToNow).toHaveBeenCalled();
});

it('shows the fetched fact title, not the hash filename', async () => {
  render(<TrailBreadcrumb repo="r" branch="b" trail={trail} onJump={vi.fn()} onReturnToNow={vi.fn()} />);
  await waitFor(() => screen.getByText('Beta fact title'));
  expect(screen.queryByText('b')).toBeNull(); // the basename of kb/b.md must not be shown
});
