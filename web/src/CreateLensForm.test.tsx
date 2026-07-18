import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { CreateLensForm } from './CreateLensForm';
import { api } from './api';

vi.mock('./api', () => ({
  api: {
    createLens: vi.fn().mockResolvedValue({ name: 'dev', write: 'work', reads: [] }),
    // Branch picker fetches these when a read repo is toggled on.
    listBranchNames: vi.fn().mockResolvedValue(['agent/dev', 'main']),
    getAgentBranch: vi.fn().mockResolvedValue('agent/dev'),
  },
}));

const repos = [{ name: 'core' }, { name: 'work' }, { name: 'ops' }];

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
    expect(body).toEqual({
      name: 'dev',
      write: 'work',
      reads: [{ repo: 'core', branch: 'main' }, { repo: 'ops' }],
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
});
