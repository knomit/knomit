import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { CreateLensForm } from './CreateLensForm';
import { api } from './api';

// Only `api` is stubbed. The module's other exports — repoAvailable, which the
// form gates its repo list on — pass through from the real module, so a test
// about which repos are offered is testing the rule the app actually ships.
vi.mock('./api', async importOriginal => ({
  ...(await importOriginal<typeof import('./api')>()),
  api: {
    createLens: vi.fn().mockResolvedValue({ name: 'dev', write: { uid: 'uid-work', name: 'work' }, reads: [] }),
    // Branch picker fetches these when a read repo is toggled on.
    listBranchNames: vi.fn().mockResolvedValue(['agent/dev', 'main']),
    getAgentBranch: vi.fn().mockResolvedValue('agent/dev'),
  },
}));

// Repo rows carry the registry uid the lens API keys membership by; the form
// renders names and submits uids.
const repos = [
  { name: 'core', uid: 'uid-core' },
  { name: 'work', uid: 'uid-work' },
  { name: 'ops', uid: 'uid-ops' },
];

describe('CreateLensForm', () => {
  beforeEach(() => vi.clearAllMocks());

  it('assembles the write repo and toggled reads with an optional branch, then reports done', async () => {
    const onDone = vi.fn();
    render(<CreateLensForm repos={repos} onDone={onDone} onError={() => {}} />);

    fireEvent.change(screen.getByTestId('lens-name'), { target: { value: 'dev' } });
    // Pick a write repo other than the default first entry.
    fireEvent.change(screen.getByTestId('lens-write'), { target: { value: 'work' } });
    // Toggle a read repo, wait for its branch list to load, then pin it to a
    // non-agent branch via the dropdown. (Query while only core's select
    // exists so the 'main' option is unambiguous.)
    fireEvent.click(screen.getByTestId('lens-read-core'));
    await screen.findByRole('option', { name: 'main' });
    fireEvent.change(screen.getByTestId('lens-branch-core'), { target: { value: 'main' } });
    // Second read repo left at its default (agent branch, sent unpinned).
    fireEvent.click(screen.getByTestId('lens-read-ops'));

    fireEvent.click(screen.getByTestId('lens-create'));

    await waitFor(() => expect(api.createLens).toHaveBeenCalled());
    const body = (api.createLens as ReturnType<typeof vi.fn>).mock.calls[0][0];
    // The wire carries uids only — the reader picked names, the form translated.
    expect(body).toEqual({
      name: 'dev',
      write: { uid: 'uid-work' },
      reads: [{ uid: 'uid-core', branch: 'main' }, { uid: 'uid-ops' }],
    });
    await waitFor(() => expect(onDone).toHaveBeenCalledWith('dev'));
  });

  it('surfaces the server error and does not call onDone', async () => {
    (api.createLens as ReturnType<typeof vi.fn>).mockRejectedValueOnce(new Error('lens "dev" already exists'));
    const onDone = vi.fn();
    const onError = vi.fn();
    render(<CreateLensForm repos={repos} onDone={onDone} onError={onError} />);

    fireEvent.change(screen.getByTestId('lens-name'), { target: { value: 'dev' } });
    fireEvent.click(screen.getByTestId('lens-create'));

    await waitFor(() => expect(api.createLens).toHaveBeenCalled());
    await waitFor(() => expect(screen.getByText(/already exists/)).toBeInTheDocument());
    expect(onDone).not.toHaveBeenCalled();
    expect(onError).toHaveBeenCalled();
  });

  it('blocks submit until a name is entered', () => {
    render(<CreateLensForm repos={repos} onDone={() => {}} onError={() => {}} />);
    const createBtn = screen.getByTestId('lens-create') as HTMLButtonElement;
    expect(createBtn.disabled).toBe(true);
    fireEvent.click(createBtn);
    expect(api.createLens).not.toHaveBeenCalled();
  });

  // ── redesign: live name validation ──

  const lenses = [{ name: 'dev', write: { uid: 'uid-work', name: 'work' }, reads: [] }];

  it('disables Create and explains when the name collides with a repo', () => {
    render(<CreateLensForm repos={repos} lenses={[]} onDone={() => {}} onError={() => {}} onCancel={() => {}} />);
    fireEvent.change(screen.getByTestId('lens-name'), { target: { value: 'core' } });
    expect((screen.getByTestId('lens-create') as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByTestId('lens-name-status')).toHaveTextContent(/already exists/i);
  });

  it('disables Create and explains when the name collides with a lens', () => {
    render(<CreateLensForm repos={repos} lenses={lenses} onDone={() => {}} onError={() => {}} onCancel={() => {}} />);
    fireEvent.change(screen.getByTestId('lens-name'), { target: { value: 'dev' } });
    expect((screen.getByTestId('lens-create') as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByTestId('lens-name-status')).toHaveTextContent(/already exists/i);
  });

  it('rejects names outside the a–z 0–9 - _ pattern', () => {
    render(<CreateLensForm repos={repos} lenses={[]} onDone={() => {}} onError={() => {}} onCancel={() => {}} />);
    fireEvent.change(screen.getByTestId('lens-name'), { target: { value: 'Bad Name!' } });
    expect((screen.getByTestId('lens-create') as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByTestId('lens-name-status')).toBeInTheDocument();
  });

  it('shows "available" for a valid, non-colliding name and enables Create', () => {
    render(<CreateLensForm repos={repos} lenses={lenses} onDone={() => {}} onError={() => {}} onCancel={() => {}} />);
    fireEvent.change(screen.getByTestId('lens-name'), { target: { value: 'fresh' } });
    expect(screen.getByTestId('lens-name-status')).toHaveTextContent(/available/i);
    expect((screen.getByTestId('lens-create') as HTMLButtonElement).disabled).toBe(false);
  });

  // ── redesign: pinned write row + checkbox reads ──

  it('renders the write repo as a pinned first row that is not toggleable', () => {
    render(<CreateLensForm repos={repos} lenses={[]} onDone={() => {}} onError={() => {}} onCancel={() => {}} />);
    // Default write is the first repo, 'core'.
    const pinned = screen.getByTestId('lens-read-core') as HTMLButtonElement;
    expect(pinned.disabled).toBe(true);
    expect(screen.getByText(/always read/i)).toBeInTheDocument();
  });

  it('select all toggles every read repo on and off', () => {
    render(<CreateLensForm repos={repos} lenses={[]} onDone={() => {}} onError={() => {}} onCancel={() => {}} />);
    // write=core; the toggleable reads are work + ops (off → no branch select).
    expect(screen.queryByTestId('lens-branch-work')).toBeNull();
    expect(screen.queryByTestId('lens-branch-ops')).toBeNull();
    fireEvent.click(screen.getByTestId('lens-select-all'));
    expect(screen.getByTestId('lens-branch-work')).toBeInTheDocument();
    expect(screen.getByTestId('lens-branch-ops')).toBeInTheDocument();
    fireEvent.click(screen.getByTestId('lens-select-all'));
    expect(screen.queryByTestId('lens-branch-work')).toBeNull();
    expect(screen.queryByTestId('lens-branch-ops')).toBeNull();
  });

  it('select all is scoped to the visible rows when a filter is active', () => {
    // >8 repos so the search filter renders; write defaults to the first (r0).
    const many = Array.from({ length: 10 }, (_, i) => ({ name: `r${i}`, uid: `uid-r${i}` }));
    render(<CreateLensForm repos={many} lenses={[]} onDone={() => {}} onError={() => {}} onCancel={() => {}} />);
    // Filter to just r1 (r1 matches; r2..r9 and the r1x-style names are hidden).
    fireEvent.change(screen.getByTestId('lens-read-search'), { target: { value: 'r1' } });
    fireEvent.click(screen.getByTestId('lens-select-all'));
    // The visible match r1 mounted; a filtered-out repo (r5) stayed off.
    expect(screen.getByTestId('lens-branch-r1')).toBeInTheDocument();
    // Clear the filter to reveal r5's row and confirm it never got a branch select.
    fireEvent.change(screen.getByTestId('lens-read-search'), { target: { value: '' } });
    expect(screen.queryByTestId('lens-branch-r5')).toBeNull();
  });

  // ── redesign: preview ──

  it('preview reflects pinned read branches', async () => {
    render(<CreateLensForm repos={repos} lenses={[]} onDone={() => {}} onError={() => {}} onCancel={() => {}} />);
    // Write repo appears in the preview from the start.
    expect(screen.getByTestId('lens-preview')).toHaveTextContent('core');
    fireEvent.click(screen.getByTestId('lens-read-work'));
    await screen.findByRole('option', { name: 'main' });
    fireEvent.change(screen.getByTestId('lens-branch-work'), { target: { value: 'main' } });
    expect(screen.getByTestId('lens-preview')).toHaveTextContent('@main');
  });

  // ── redesign: description ──

  it('includes description in the payload only when non-empty', async () => {
    render(<CreateLensForm repos={repos} lenses={[]} onDone={() => {}} onError={() => {}} onCancel={() => {}} />);
    fireEvent.change(screen.getByTestId('lens-name'), { target: { value: 'dev' } });
    fireEvent.change(screen.getByTestId('lens-description'), { target: { value: 'eng lens' } });
    fireEvent.click(screen.getByTestId('lens-create'));
    await waitFor(() => expect(api.createLens).toHaveBeenCalled());
    const body = (api.createLens as ReturnType<typeof vi.fn>).mock.calls[0][0];
    expect(body.description).toBe('eng lens');
  });

  // A lens whose member has no live store cannot be resolved at all — the
  // binding refuses it and the whole lens fails, not just that mount. So a repo
  // in a non-active state is not offered as a target: the form must not let a
  // user assemble something the server will reject on first read.
  describe('repos with no live store', () => {
    const withBroken = [
      { name: 'aaa', uid: 'uid-aaa', state: 'missing', detail: 'database file not found' },
      ...repos,
    ];

    it('is not offered as the write target, and is not the default', () => {
      render(<CreateLensForm repos={withBroken} lenses={[]} onDone={() => {}} onError={() => {}} />);
      const select = screen.getByTestId('lens-write') as HTMLSelectElement;
      const options = Array.from(select.options).map(o => o.value);
      expect(options).toEqual(['core', 'work', 'ops']);
      // It sorts first, so an ungated default would have landed on it.
      expect(select.value).toBe('core');
    });

    it('is not offered as a read mount', () => {
      render(<CreateLensForm repos={withBroken} lenses={[]} onDone={() => {}} onError={() => {}} />);
      expect(screen.queryByTestId('lens-read-aaa')).toBeNull();
      expect(screen.getByTestId('lens-read-work')).toBeInTheDocument();
    });

    it('is not swept in by select-all', async () => {
      render(<CreateLensForm repos={withBroken} lenses={[]} onDone={() => {}} onError={() => {}} />);
      fireEvent.change(screen.getByTestId('lens-name'), { target: { value: 'dev' } });
      fireEvent.click(screen.getByTestId('lens-select-all'));
      fireEvent.click(screen.getByTestId('lens-create'));

      await waitFor(() => expect(api.createLens).toHaveBeenCalled());
      const body = (api.createLens as ReturnType<typeof vi.fn>).mock.calls[0][0];
      expect(body.reads).toEqual([{ uid: 'uid-work' }, { uid: 'uid-ops' }]);
    });
  });

  it('calls onCancel from the Cancel button', () => {
    const onCancel = vi.fn();
    render(<CreateLensForm repos={repos} lenses={[]} onDone={() => {}} onError={() => {}} onCancel={onCancel} />);
    fireEvent.click(screen.getByTestId('lens-cancel'));
    expect(onCancel).toHaveBeenCalled();
  });
});
