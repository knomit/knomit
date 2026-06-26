import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { VersionBadge } from './VersionBadge';

describe('VersionBadge', () => {
  afterEach(() => { vi.restoreAllMocks(); });

  it('fetches the version on mount and renders the full string', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ version: '0.5.0', commit: '2a7ae9d', full: '0.5.0.2a7ae9d' }),
    }) as unknown as typeof fetch;

    render(<VersionBadge />);

    await waitFor(() => {
      expect(screen.getByTestId('version-badge')).toHaveTextContent('0.5.0.2a7ae9d');
    });
  });

  it('renders nothing when the version fetch fails', async () => {
    globalThis.fetch = vi.fn().mockRejectedValue(new Error('network')) as unknown as typeof fetch;

    render(<VersionBadge />);

    // Give the rejected promise a tick to settle, then assert no badge.
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalled());
    expect(screen.queryByTestId('version-badge')).toBeNull();
  });
});
