import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor, act } from '@testing-library/react';
import { ExplainView } from './ExplainView';
import type { RefGroup } from './api';

vi.mock('./api', () => ({
  api: {
    fact: vi.fn(),
    explain: vi.fn(),
    factCommits: vi.fn().mockResolvedValue({ entries: [] }),
    commitDetail: vi.fn().mockResolvedValue({
      commit: '', date: '', message: '', operation: '', files: [],
    }),
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

  it('renders single-version chip with 7-char commit hash and no ×N badge', async () => {
    (api.explain as unknown as ReturnType<typeof vi.fn>).mockResolvedValue({
      incoming: [makeGroup({ path: 'kb/single.md', title: 'Single', versions: [{ commit: 'abcdef0123', committed_at: 1000 }] })],
      outgoing: [],
    });
    render(<ExplainView repo="r" branch="b" initialEntry={{ path: 'kb/x.md', commit: null }} onClose={() => {}} />);

    expect(await screen.findByText('Single')).toBeInTheDocument();
    expect(screen.getByText('abcdef0')).toBeInTheDocument();
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

  it('clicking a non-deleted outgoing chip navigates to the historical target commit, not live', async () => {
    // Regression: when an outgoing ref's target path no longer exists at HEAD
    // but t.deleted is false on the graph node (target was moved/renamed/
    // GC'd without a formal retraction), clicking the chip used to navigate
    // to the live path (commit=null) and 404. Out-edges must always follow
    // lineage to the version the source originally pointed to.
    (api.explain as unknown as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      incoming: [],
      outgoing: [makeGroup({
        path: 'kb/target.md',
        title: 'Target',
        deleted: false,
        versions: [{ commit: 'target0commit789', committed_at: 1000 }],
      })],
    });
    (api.fact as unknown as ReturnType<typeof vi.fn>).mockResolvedValue({ ...baseFact, path: 'kb/target.md' });

    render(<ExplainView repo="r" branch="b" initialEntry={{ path: 'kb/x.md', commit: null }} onClose={() => {}} />);

    await screen.findByText('Target');
    fireEvent.click(screen.getByText('Target'));

    await waitFor(() => {
      const calls = (api.fact as unknown as ReturnType<typeof vi.fn>).mock.calls;
      expect(calls.some(c => c[2] === 'kb/target.md' && c[3] === 'target0commit789')).toBe(true);
    });
  });

  it('multi-version dropdown is rendered outside the chip row so overflow clipping does not hide it', async () => {
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

    const row = screen.getByText('newcomm');
    const chip = screen.getByTestId('ref-chip');
    const chipRow = chip.parentElement;
    // The dropdown must NOT be inside the chip-row container, because that
    // container has overflow:auto/hidden which would visually clip the dropdown.
    expect(chipRow).not.toBeNull();
    expect(chipRow!.contains(row)).toBe(false);
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

describe('ExplainView header commit chip', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('shows the 7-char commit hash of the loaded fact in the history panel header', async () => {
    // Without a commit hash visible, a user landing on a historical version
    // via an out-edge has no way to tell which version they are looking at.
    (api.fact as unknown as ReturnType<typeof vi.fn>).mockResolvedValue({
      ...baseFact,
      commit_hash: 'cafe1234567890',
    });
    (api.explain as unknown as ReturnType<typeof vi.fn>).mockResolvedValue({ incoming: [], outgoing: [] });

    render(<ExplainView repo="r" branch="b" initialEntry={{ path: 'kb/x.md', commit: null }} onClose={() => {}} />);

    expect(await screen.findByTestId('history-panel-commit')).toHaveTextContent('cafe123');
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
    // Local refs are listed in the body as non-clickable text (the outgoing
    // rail visualises edges with type/commit metadata; the body shows the
    // fact's literal refs list — the right panel agrees on this in readOnly).
    expect(screen.getByText(/kb\/other\.md/)).toBeInTheDocument();
  });
});

describe('ExplainView Chip — type-aware styling', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    (api.fact as unknown as ReturnType<typeof vi.fn>).mockResolvedValue(baseFact);
  });

  it('renders the type icon and uses the type color on the chip border', async () => {
    (api.explain as unknown as ReturnType<typeof vi.fn>).mockResolvedValue({
      incoming: [],
      outgoing: [makeGroup({
        path: 'kb/principle.md',
        title: 'A principle',
        type: 'principle',
        versions: [{ commit: 'aaaaaaa1', committed_at: 1000, type: 'principle' }],
      })],
    });
    render(<ExplainView repo="r" branch="b" initialEntry={{ path: 'kb/x.md', commit: null }} onClose={() => {}} />);

    const chip = await screen.findByTestId('ref-chip');
    // Type icon present (the SVG produced by TypeIcon).
    expect(chip.querySelector('svg')).not.toBeNull();
    // Title still rendered.
    expect(chip).toHaveTextContent('A principle');
    // Border uses the principle accent. typeStyles.principle.color = '#da8' → rgb(221, 170, 136).
    // jsdom returns the resolved style as a string; assert it contains the principle hex or rgb.
    expect(chip.style.borderColor.toLowerCase()).toMatch(/(#da8|rgb\(221,\s*170,\s*136\))/);
  });

  it('shows just the 7-char commit hash (no commit_at_ prefix)', async () => {
    (api.explain as unknown as ReturnType<typeof vi.fn>).mockResolvedValue({
      incoming: [makeGroup({
        path: 'kb/single.md',
        title: 'Single',
        type: 'concept',
        versions: [{ commit: 'abcdef0123', committed_at: 1000, type: 'concept' }],
      })],
      outgoing: [],
    });
    render(<ExplainView repo="r" branch="b" initialEntry={{ path: 'kb/x.md', commit: null }} onClose={() => {}} />);
    expect(await screen.findByText('Single')).toBeInTheDocument();
    expect(screen.getByText('abcdef0')).toBeInTheDocument();
    expect(screen.queryByText(/commit_at_/)).toBeNull();
  });
});

describe('ExplainView header strips', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    (api.fact as unknown as ReturnType<typeof vi.fn>).mockResolvedValue(baseFact);
  });

  it('outgoing header shows total, per-type breakdown, and retracted count', async () => {
    (api.explain as unknown as ReturnType<typeof vi.fn>).mockResolvedValue({
      incoming: [],
      outgoing: [
        makeGroup({ path: 'kb/p1.md', title: 'P1', type: 'principle', versions: [{ commit: 'a1234567', committed_at: 1, type: 'principle' }] }),
        makeGroup({ path: 'kb/p2.md', title: 'P2', type: 'principle', versions: [{ commit: 'b2345678', committed_at: 1, type: 'principle' }] }),
        makeGroup({ path: 'kb/c1.md', title: 'C1', type: 'concept',   versions: [{ commit: 'c3456789', committed_at: 1, type: 'concept' }] }),
        makeGroup({ path: 'kb/d1.md', title: 'D1', type: 'concept',   deleted: true, versions: [{ commit: 'd4567890', committed_at: 1, type: 'concept', deleted: true }] }),
      ],
    });
    render(<ExplainView repo="r" branch="b" initialEntry={{ path: 'kb/x.md', commit: null }} onClose={() => {}} />);

    const header = await screen.findByTestId('outgoing-header');
    expect(header).toHaveTextContent('OUT-EDGES');
    expect(header).toHaveTextContent('REFERENCES 4');
    expect(header).toHaveTextContent(/principle\s*2/i);
    expect(header).toHaveTextContent(/concept\s*2/i);
    expect(header).toHaveTextContent('1 retracted');
  });

  it('incoming header shows IN-EDGES · REFERENCED BY <total>', async () => {
    (api.explain as unknown as ReturnType<typeof vi.fn>).mockResolvedValue({
      incoming: [
        makeGroup({ path: 'kb/h.md', title: 'H', type: 'hypothesis', versions: [{ commit: 'a1', committed_at: 1, type: 'hypothesis' }] }),
      ],
      outgoing: [],
    });
    render(<ExplainView repo="r" branch="b" initialEntry={{ path: 'kb/x.md', commit: null }} onClose={() => {}} />);

    const header = await screen.findByTestId('incoming-header');
    expect(header).toHaveTextContent('IN-EDGES');
    expect(header).toHaveTextContent('REFERENCED BY 1');
    expect(header).toHaveTextContent(/hypothesis\s*1/i);
  });

  it('renders incoming strip in commit-anchored view (not gated to live mode)', async () => {
    // Regression: ExplainView previously gated the incoming strip on
    // current.commit === null, hiding incoming edges entirely when the user
    // navigated to a historical version. The backend supports commit-anchored
    // /incoming and the data is fetched — the UI must surface it.
    (api.explain as unknown as ReturnType<typeof vi.fn>).mockResolvedValue({
      incoming: [
        makeGroup({ path: 'kb/source.md', title: 'Source', type: 'synthesis', versions: [{ commit: 'src01234567', committed_at: 1, type: 'synthesis' }] }),
      ],
      outgoing: [],
    });
    render(<ExplainView repo="r" branch="b" initialEntry={{ path: 'kb/x.md', commit: 'tc01234567' }} onClose={() => {}} />);

    const header = await screen.findByTestId('incoming-header');
    expect(header).toHaveTextContent('IN-EDGES');
    expect(header).toHaveTextContent('REFERENCED BY 1');
    expect(await screen.findByText('Source')).toBeInTheDocument();
  });
});

describe('ExplainView fact fetch', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('passes fallback: "before" so retracted refs show their last known version', async () => {
    (api.fact as unknown as ReturnType<typeof vi.fn>).mockResolvedValue({
      path: 'kb/retracted.md', title: 'Retracted Target', body: 'pre-retraction body',
      domain: [], confidence: 0.5, sources: 1, entities: [], refs: [],
      commit_hash: 'older1234',
    });
    (api.explain as unknown as ReturnType<typeof vi.fn>).mockResolvedValue({ incoming: [], outgoing: [] });

    render(
      <ExplainView
        repo="knomit"
        branch="machine/test"
        initialEntry={{ path: 'kb/retracted.md', commit: 'parentcommit' }}
        onClose={() => {}}
      />
    );

    await waitFor(() => {
      expect(api.fact).toHaveBeenCalledWith(
        'knomit',
        'machine/test',
        'kb/retracted.md',
        'parentcommit',
        { fallback: 'before' }
      );
    });
  });

  it('clicking a local ref inside the rendered fact body navigates within Explain at parent\'s commit', async () => {
    (api.fact as unknown as ReturnType<typeof vi.fn>)
      .mockResolvedValueOnce({
        path: 'kb/parent.md', title: 'Parent',
        body: 'parent body', domain: [], confidence: 0.9, sources: 1, entities: [],
        refs: ['kb/inner-ref.md'],
        commit_hash: 'parent7',
      })
      .mockResolvedValueOnce({
        path: 'kb/inner-ref.md', title: 'Inner Ref',
        body: 'inner body', domain: [], confidence: 0.7, sources: 1, entities: [],
        refs: [],
        commit_hash: 'inner7',
      });
    (api.explain as unknown as ReturnType<typeof vi.fn>).mockResolvedValue({ incoming: [], outgoing: [] });

    render(
      <ExplainView
        repo="knomit"
        branch="machine/test"
        initialEntry={{ path: 'kb/parent.md', commit: 'parentcommit' }}
        onClose={() => {}}
      />
    );

    // Wait for parent fact to render (its ref link appears in the body).
    const refLink = await screen.findByText(/kb\/inner-ref\.md/);
    fireEvent.click(refLink);

    // Second fact fetch: anchored at PARENT's commit (the moment of reference),
    // with fallback=before so retracted refs walk back.
    await waitFor(() => {
      expect(api.fact).toHaveBeenLastCalledWith(
        'knomit',
        'machine/test',
        'kb/inner-ref.md',
        'parentcommit',
        { fallback: 'before' },
      );
    });

    // Back-stack advanced: Back button is now visible.
    expect(screen.getByRole('button', { name: /back/i })).toBeInTheDocument();
  });
});
