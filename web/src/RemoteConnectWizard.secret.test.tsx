import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { RemoteConnectWizard } from './RemoteConnectWizard';

vi.mock('./api', async importOriginal => ({
  RepoIndexingError: (await importOriginal<typeof import('./api')>()).RepoIndexingError,
  api: { getOrigin: vi.fn(), listSecrets: vi.fn(), createSecret: vi.fn() },
  createSession: vi.fn(),
  streamTest: vi.fn(),
  streamPreview: vi.fn(),
  streamApply: vi.fn(),
  streamCommit: vi.fn(),
  deleteSession: vi.fn().mockResolvedValue(undefined),
}));

import { api, createSession } from './api';
type Fn = ReturnType<typeof vi.fn>;

describe('RemoteConnectWizard: stored secret', () => {
  beforeEach(() => vi.clearAllMocks());

  it('creates the session with secret_id and no pasted credential', async () => {
    (api.getOrigin as unknown as Fn).mockResolvedValueOnce(null);
    (api.listSecrets as unknown as Fn).mockResolvedValue([
      { id: 's1', name: 'gh-pat', type: 'token', hosts: ['github.com'], expires_at: null, status: 'none', created_at: '', updated_at: '', linked_repos: 0 },
    ]);
    (createSession as unknown as Fn).mockRejectedValueOnce(new Error('stop here'));
    render(<RemoteConnectWizard repo="kb" onCancel={() => {}} onDone={() => {}} />);
    fireEvent.change(await screen.findByTestId('wizard-url'), { target: { value: 'https://github.com/a/kb.git' } });
    fireEvent.change(screen.getByLabelText('Auth method'), { target: { value: 'secret' } });
    expect(screen.queryByPlaceholderText('ghp_…')).toBeNull();
    const test = screen.getByRole('button', { name: /^test/i });
    expect(test).toBeDisabled();
    fireEvent.change(await screen.findByTestId('wizard-secret-picker-select'), { target: { value: 's1' } });
    fireEvent.click(screen.getByRole('button', { name: /^test/i }));
    await waitFor(() => expect(createSession).toHaveBeenCalled());
    expect((createSession as unknown as Fn).mock.calls[0][1]).toEqual({ url: 'https://github.com/a/kb.git', secret_id: 's1' });
  });

  it('a linked origin opens on Stored secret', async () => {
    (api.getOrigin as unknown as Fn).mockResolvedValueOnce({
      name: 'origin', url: 'https://github.com/a/kb.git', branch: 'main', auth_method: 'token', secret_name: 'gh-pat', secret_status: 'ok',
    });
    (api.listSecrets as unknown as Fn).mockResolvedValue([]);
    render(<RemoteConnectWizard repo="kb" onCancel={() => {}} onDone={() => {}} />);
    await waitFor(() => expect((screen.getByLabelText('Auth method') as HTMLSelectElement).value).toBe('secret'));
  });
});
