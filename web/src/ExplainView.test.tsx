import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor, act } from '@testing-library/react';
import { ExplainView } from './ExplainView';
import type { RefGroup } from './api';

vi.mock('./api', () => ({
  api: {
    fact: vi.fn(),
    explain: vi.fn(),
  },
}));

import { api } from './api';

const baseFact = {
  path: 'kb/x.md', title: 'X', body: 'body', domain: [], confidence: 1, sources: 1, entities: [], refs: [],
};

function makeGroup(overrides: Partial<RefGroup> = {}): RefGroup {
  return {
    path: 'kb/a.md',
    title: 'Fact A',
    versions: [{ commit: 'aaaaaaa1234', committed_at: 1000 }],
    deleted: false,
    ...overrides,
  };
}

describe('ExplainView Chip', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    (api.fact as unknown as ReturnType<typeof vi.fn>).mockResolvedValue(baseFact);
  });

  it('renders single-version chip with commit_at_<hash7> pill and no ×N badge', async () => {
    (api.explain as unknown as ReturnType<typeof vi.fn>).mockResolvedValue({
      incoming: [makeGroup({ path: 'kb/single.md', title: 'Single', versions: [{ commit: 'abcdef0123', committed_at: 1000 }] })],
      outgoing: [],
    });
    render(<ExplainView repo="r" branch="b" initialEntry={{ path: 'kb/x.md', commit: null }} onClose={() => {}} />);

    expect(await screen.findByText('Single')).toBeInTheDocument();
    expect(screen.getByText(/commit_at_abcdef0/)).toBeInTheDocument();
    // No multi-version badge.
    expect(screen.queryByText(/×\d+ ⌄/)).toBeNull();
  });

  it('renders ×N ⌄ badge for multi-version chip and opens dropdown on click', async () => {
    const versions = [
      { commit: 'newcommit1234567', committed_at: 2000 },
      { commit: 'oldcommit7654321', committed_at: 1000 },
    ];
    (api.explain as unknown as ReturnType<typeof vi.fn>).mockResolvedValue({
      incoming: [makeGroup({ path: 'kb/multi.md', title: 'Multi', versions })],
      outgoing: [],
    });
    render(<ExplainView repo="r" branch="b" initialEntry={{ path: 'kb/x.md', commit: null }} onClose={() => {}} />);

    expect(await screen.findByText('Multi')).toBeInTheDocument();
    const badge = screen.getByText('×2 ⌄');
    expect(badge).toBeInTheDocument();
    // Dropdown rows not yet rendered.
    expect(screen.queryByText('newcomm')).toBeNull();

    // Clicking the chip opens the dropdown listing both 7-char hashes.
    fireEvent.click(screen.getByText('Multi'));
    expect(screen.getByText('newcomm')).toBeInTheDocument();
    expect(screen.getByText('oldcomm')).toBeInTheDocument();
  });

  it('clicking a dropdown row navigates to that specific commit (incoming)', async () => {
    const versions = [
      { commit: 'newcommit1234567', committed_at: 2000 },
      { commit: 'oldcommit7654321', committed_at: 1000 },
    ];
    (api.explain as unknown as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      incoming: [makeGroup({ path: 'kb/multi.md', title: 'Multi', versions })],
      outgoing: [],
    });
    // After navigation, the new "current" entry has commit set, so explain
    // is not called again — only fact() is. Mock fact() for the navigated path.
    (api.fact as unknown as ReturnType<typeof vi.fn>).mockResolvedValue({ ...baseFact, path: 'kb/multi.md' });

    render(<ExplainView repo="r" branch="b" initialEntry={{ path: 'kb/x.md', commit: null }} onClose={() => {}} />);

    await screen.findByText('Multi');
    fireEvent.click(screen.getByText('Multi'));
    fireEvent.click(screen.getByText('oldcomm'));

    // After clicking a dropdown row for incoming: navigateTo({path, commit}).
    // ExplainView triggers a fact fetch for the new (path, commit) pair.
    await waitFor(() => {
      const calls = (api.fact as unknown as ReturnType<typeof vi.fn>).mock.calls;
      expect(calls.some(c => c[2] === 'kb/multi.md' && c[3] === 'oldcommit7654321')).toBe(true);
    });
  });

  it('outside-click closes the dropdown', async () => {
    const versions = [
      { commit: 'newcommit1234567', committed_at: 2000 },
      { commit: 'oldcommit7654321', committed_at: 1000 },
    ];
    (api.explain as unknown as ReturnType<typeof vi.fn>).mockResolvedValue({
      incoming: [makeGroup({ path: 'kb/multi.md', title: 'Multi', versions })],
      outgoing: [],
    });
    render(<ExplainView repo="r" branch="b" initialEntry={{ path: 'kb/x.md', commit: null }} onClose={() => {}} />);

    await screen.findByText('Multi');
    fireEvent.click(screen.getByText('Multi'));
    expect(screen.getByText('newcomm')).toBeInTheDocument();

    // Outside click: fire mousedown on document.body.
    act(() => {
      document.body.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }));
    });

    await waitFor(() => {
      expect(screen.queryByText('newcomm')).toBeNull();
    });
  });
});

describe('ExplainView body parity', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('renders type badge, stat boxes, tag clouds, external refs in body', async () => {
    (api.fact as unknown as ReturnType<typeof vi.fn>).mockResolvedValue({
      ...baseFact,
      type: 'concept',
      title: 'X',
      body: 'Hello **world**',
      domain: ['ai'],
      entities: ['Anthropic'],
      confidence: 0.5,
      sources: 2,
      refs: ['https://example.com/paper', 'kb/other.md'],
    });
    (api.explain as unknown as ReturnType<typeof vi.fn>).mockResolvedValue({ incoming: [], outgoing: [] });

    render(<ExplainView repo="r" branch="b" initialEntry={{ path: 'kb/x.md', commit: null }} onClose={() => {}} />);

    expect(await screen.findByTestId('fact-type-badge')).toBeInTheDocument();
    expect(screen.getByText('0.50')).toBeInTheDocument();
    expect(screen.getByText('2')).toBeInTheDocument();
    expect(screen.getByText('ai')).toBeInTheDocument();
    expect(screen.getByText('Anthropic')).toBeInTheDocument();
    expect(screen.getByText(/example\.com\/paper/)).toBeInTheDocument();
    // Local ref must NOT appear in the body — it surfaces in the outgoing rail.
    expect(screen.queryByText(/kb\/other\.md/)).toBeNull();
  });
});
