import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { CreateRepoWizard } from './CreateRepoWizard';
import { api } from './api';

vi.mock('./api', () => ({
  api: {
    probeOrigin: vi.fn(),
    createRepo: vi.fn(async (_b: unknown, onEvent: (e: unknown) => void) => {
      onEvent({ type: 'progress', step: 'init-git', pct: 50, message: 'x' });
      onEvent({ type: 'done', repo: { name: 'kb' } });
    }),
    ontologyPresets: vi.fn(async () => [
      { name: 'default', id: 'general', title: 'General', description: 'd', topics: ['people'] },
      { name: 'code', id: 'source-code', title: 'Code', description: 'd', topics: ['invariants'] },
    ]),
  },
}));

const probed = (over: Partial<Record<string, unknown>> = {}) => ({
  reachable: true, empty: false, auth_required: false,
  upstream_branch: 'main', branches: ['main'], ...over,
});

describe('CreateRepoWizard', () => {
  beforeEach(() => vi.clearAllMocks());

  it('a populated remote goes straight to review with no ontology step', async () => {
    (api.probeOrigin as ReturnType<typeof vi.fn>).mockResolvedValue(probed());
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);

    fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://h/r.git' } });
    fireEvent.click(screen.getByTestId('probe-button'));

    await waitFor(() => expect(screen.getByTestId('step-access')).toBeInTheDocument());
    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'kb' } });
    fireEvent.click(screen.getByRole('button', { name: /next|review/i }));

    await waitFor(() => expect(screen.getByTestId('step-review')).toBeInTheDocument());
    expect(screen.queryByTestId('step-ontology')).not.toBeInTheDocument();
  });

  it('an empty remote reaches the ontology step and submits mode seed', async () => {
    (api.probeOrigin as ReturnType<typeof vi.fn>).mockResolvedValue(probed({ empty: true, branches: [] }));
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);

    fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://h/new.git' } });
    fireEvent.click(screen.getByTestId('probe-button'));

    await waitFor(() => expect(screen.getByTestId('step-access')).toBeInTheDocument());
    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'kb' } });
    fireEvent.click(screen.getByRole('button', { name: /next|ontology/i }));
    await waitFor(() => expect(screen.getByTestId('step-ontology')).toBeInTheDocument());

    fireEvent.click(screen.getByRole('button', { name: /next|review/i }));
    await waitFor(() => expect(screen.getByTestId('step-review')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: /create repository/i }));

    await waitFor(() => expect(api.createRepo).toHaveBeenCalled());
    const body = (api.createRepo as ReturnType<typeof vi.fn>).mock.calls[0][0];
    expect(body).toMatchObject({ mode: 'seed', name: 'kb', ontology_preset: 'default' });
  });

  it('shows the agreed local-only trade-off copy, verbatim and unstyled as a warning', async () => {
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);
    fireEvent.click(screen.getByRole('button', { name: /keep it on this machine/i }));
    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'scratch' } });
    fireEvent.click(screen.getByRole('button', { name: /next|ontology/i }));
    fireEvent.click(screen.getByRole('button', { name: /next|review/i }));

    await waitFor(() => expect(screen.getByTestId('step-review')).toBeInTheDocument());
    expect(screen.getByText(/all your facts come across/i)).toBeInTheDocument();
    expect(screen.getByText(/each fact's earlier revisions/i)).toBeInTheDocument();
  });

  // Neither peer carries a badge: a badge on one makes the other read as wrong.
  it('does not label either source choice as recommended', () => {
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);
    expect(screen.queryByText(/recommended/i)).not.toBeInTheDocument();
  });

  it('omits Cancel when no onCancel is supplied', () => {
    render(<CreateRepoWizard onDone={() => {}} />);
    expect(screen.queryByRole('button', { name: /^cancel$/i })).not.toBeInTheDocument();
  });

  it('surfaces an unreachable remote without advancing', async () => {
    (api.probeOrigin as ReturnType<typeof vi.fn>).mockResolvedValue({
      reachable: false, empty: false, auth_required: false,
      upstream_branch: '', branches: [], detail: 'no such host',
    });
    render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);
    fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://nope/r.git' } });
    fireEvent.click(screen.getByTestId('probe-button'));

    await waitFor(() => expect(screen.getByText(/no such host/i)).toBeInTheDocument());
    expect(screen.getByTestId('step-source')).toBeInTheDocument();
    expect(screen.queryByTestId('step-access')).not.toBeInTheDocument();
  });
});
