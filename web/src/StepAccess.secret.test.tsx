import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { CreateRepoWizard } from './CreateRepoWizard';
import { api } from './api';

// The create wizard's access step offers a stored secret: chosen, the token
// fields go, and the probe, the initialization check and the create carry
// secret_id with no pasted credential (the server refuses both together).

vi.mock('./api', async importOriginal => ({
  ...(await importOriginal<typeof import('./api')>()),
  api: {
    probeOrigin: vi.fn(),
    probeInitialized: vi.fn(),
    createRepo: vi.fn(),
    repos: vi.fn(async () => [{ name: 'kb' }]),
    listSecrets: vi.fn(),
    createSecret: vi.fn(),
    ontologyPresets: vi.fn(async () => [{ name: 'default', id: 'general', title: 'General', description: 'd', topics: ['people'] }]),
    ontologyPresetYAML: vi.fn(async () => 'id: general\nname: General\ntopics:\n  people:\n'),
    validateOntology: vi.fn(async () => ({ ok: true, id: 'general', name: 'General', topics: ['people'], rule_count: 0 })),
    ontologySchema: vi.fn(async () => []),
  },
}));

const fn = (f: unknown) => f as ReturnType<typeof vi.fn>;
const probed = (over: Record<string, unknown> = {}) => ({
  reachable: true, empty: false, auth_required: false, upstream_branch: 'main', branches: ['main'], ...over,
});
const secret = (id: string, name: string) => ({
  id, name, type: 'token', hosts: ['github.com'], expires_at: null, status: 'none',
  created_at: '', updated_at: '', linked_repos: 0,
});
const done = async (_b: unknown, onStatus: (s: unknown) => void) => {
  const final = { create_id: 'j', name: 'kb', mode: 'clone', state: 'done', step: 'done', pct: 100, repo: { name: 'kb' } };
  onStatus(final);
  return final;
};

async function toAccess() {
  render(<CreateRepoWizard onDone={() => {}} onCancel={() => {}} />);
  fireEvent.change(screen.getByTestId('create-url'), { target: { value: 'https://github.com/a/private.git' } });
  fireEvent.click(screen.getByTestId('probe-button'));
  await waitFor(() => expect(screen.getByTestId('step-access')).toBeInTheDocument());
  fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'kb' } });
}

describe('StepAccess: stored secret', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    fn(api.createRepo).mockImplementation(done);
    fn(api.listSecrets).mockResolvedValue([secret('s1', 'gh-pat')]);
    fn(api.probeInitialized).mockResolvedValue({ initialized: 'yes', branch: 'main' });
  });

  it('hides the token fields, and sends secret_id with no token on probe, check and create', async () => {
    fn(api.probeOrigin).mockResolvedValueOnce(probed({ auth_required: true, branches: [] })).mockResolvedValue(probed());
    await toAccess();
    fireEvent.change(screen.getByTestId('create-auth-method'), { target: { value: 'secret' } });
    expect(screen.queryByTestId('create-token')).toBeNull();
    // Next waits for a secret to be chosen.
    expect(screen.getByRole('button', { name: /^next$/i })).toBeDisabled();
    fireEvent.change(await screen.findByTestId('create-secret-picker-select'), { target: { value: 's1' } });
    fireEvent.click(screen.getByRole('button', { name: /^next$/i }));

    await waitFor(() => expect(screen.getByTestId('step-branch')).toBeInTheDocument());
    const reprobe = fn(api.probeOrigin).mock.calls[1][0];
    expect(reprobe).toMatchObject({ url: 'https://github.com/a/private.git', secret_id: 's1' });
    expect(reprobe.auth_token).toBeFalsy();
    expect(reprobe.auth_method).toBeFalsy();

    fireEvent.click(screen.getByTestId('branch-check-button'));
    await waitFor(() => expect(screen.getByTestId('step-review')).toBeInTheDocument());
    expect(fn(api.probeInitialized).mock.calls[0][0]).toMatchObject({ secret_id: 's1' });

    fireEvent.click(screen.getByRole('button', { name: /create repository/i }));
    await waitFor(() => expect(api.createRepo).toHaveBeenCalled());
    const origin = fn(api.createRepo).mock.calls[0][0].origin;
    expect(origin).toMatchObject({ url: 'https://github.com/a/private.git', secret_id: 's1' });
    expect(origin.auth_token).toBeFalsy();
  });

  it('creating a secret inline selects it', async () => {
    fn(api.probeOrigin).mockResolvedValue(probed());
    fn(api.listSecrets).mockResolvedValue([]);
    const created = secret('new', 'new-pat');
    fn(api.createSecret).mockResolvedValue(created);
    await toAccess();
    fireEvent.change(screen.getByTestId('create-auth-method'), { target: { value: 'secret' } });
    fireEvent.change(await screen.findByTestId('create-secret-picker-select'), { target: { value: '\u0000new' } });
    expect(screen.getByTestId('secret-hosts')).toHaveValue('github.com');
    fireEvent.change(screen.getByTestId('secret-name'), { target: { value: 'new-pat' } });
    fireEvent.change(screen.getByTestId('secret-value'), { target: { value: 'v' } });
    fn(api.listSecrets).mockResolvedValue([created]);
    fireEvent.click(screen.getByTestId('secret-form-submit'));
    await waitFor(() => expect(screen.getByTestId('create-secret-picker-select')).toHaveValue('new'));
    expect(screen.getByRole('button', { name: /^next$/i })).not.toBeDisabled();
  });
});
