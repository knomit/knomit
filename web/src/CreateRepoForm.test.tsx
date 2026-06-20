import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { CreateRepoForm } from './CreateRepoForm';
import { api } from './api';

vi.mock('./api', () => ({
  api: {
    createRepo: vi.fn(async (_body: unknown, onEvent: (e: unknown) => void) => {
      onEvent({ type: 'progress', step: 'init-git', pct: 50, message: 'x' });
      onEvent({ type: 'done', repo: { name: 'work' } });
    }),
  },
}));

describe('CreateRepoForm', () => {
  beforeEach(() => vi.clearAllMocks());

  it('submits preset mode and reports done', async () => {
    const onDone = vi.fn();
    render(<CreateRepoForm onDone={onDone} onCancel={() => {}} />);
    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'work' } });
    fireEvent.click(screen.getByRole('button', { name: /^create$/i }));
    await waitFor(() => expect(api.createRepo).toHaveBeenCalled());
    await waitFor(() => expect(onDone).toHaveBeenCalledWith('work'));
  });

  it('clones a local path with the default auto-detect auth and no token: sends auth_method ""', async () => {
    render(<CreateRepoForm onDone={() => {}} onCancel={() => {}} />);
    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'work' } });
    fireEvent.click(screen.getByRole('button', { name: /clone remote/i }));

    fireEvent.change(screen.getByPlaceholderText(/path\/to\/repo/i), { target: { value: '/srv/kb' } });
    fireEvent.click(screen.getByRole('button', { name: /^create$/i }));

    await waitFor(() => expect(api.createRepo).toHaveBeenCalled());
    const body = (api.createRepo as unknown as ReturnType<typeof vi.fn>).mock.calls[0][0];
    // No token entered → stays auto-detect (anonymous/SSH inference on the backend).
    expect(body.origin).toMatchObject({ url: '/srv/kb', auth_method: '', auth_token: '' });
  });

  // Regression: under the default auto-detect, a private-HTTPS clone must still be
  // possible. The token field is shown (optional) and entering a token promotes
  // the request to explicit token auth so the credential is actually used —
  // previously the field was hidden and the token silently dropped.
  it('promotes auto-detect + token to token auth for a private HTTPS clone', async () => {
    render(<CreateRepoForm onDone={() => {}} onCancel={() => {}} />);
    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'work' } });
    fireEvent.click(screen.getByRole('button', { name: /clone remote/i }));

    // The token field is available under the default auto-detect (no need to
    // first discover the token dropdown option).
    const tokenField = screen.getByPlaceholderText('••••••••');
    fireEvent.change(screen.getByPlaceholderText(/path\/to\/repo/i), { target: { value: 'https://github.com/me/private.git' } });
    fireEvent.change(tokenField, { target: { value: 'ghp_secret' } });
    fireEvent.click(screen.getByRole('button', { name: /^create$/i }));

    await waitFor(() => expect(api.createRepo).toHaveBeenCalled());
    const body = (api.createRepo as unknown as ReturnType<typeof vi.fn>).mock.calls[0][0];
    expect(body.origin).toMatchObject({ url: 'https://github.com/me/private.git', auth_method: 'token', auth_token: 'ghp_secret' });
  });

  it('sends auth_method "none" when None is explicitly selected for a clone', async () => {
    render(<CreateRepoForm onDone={() => {}} onCancel={() => {}} />);
    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'work' } });
    fireEvent.click(screen.getByRole('button', { name: /clone remote/i }));

    fireEvent.change(screen.getByPlaceholderText(/path\/to\/repo/i), { target: { value: '/srv/kb' } });
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'none' } });
    fireEvent.click(screen.getByRole('button', { name: /^create$/i }));

    await waitFor(() => expect(api.createRepo).toHaveBeenCalled());
    const body = (api.createRepo as unknown as ReturnType<typeof vi.fn>).mock.calls[0][0];
    expect(body.origin).toMatchObject({ url: '/srv/kb', auth_method: 'none', auth_token: '' });
  });

  // Regression: a token typed under auto-detect and then abandoned (method
  // switched to None) must NOT be shipped — only methods that consume a token
  // send one, so no stale credential is persisted server-side.
  it('drops a stale token when the method is switched away from token/basic', async () => {
    render(<CreateRepoForm onDone={() => {}} onCancel={() => {}} />);
    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'work' } });
    fireEvent.click(screen.getByRole('button', { name: /clone remote/i }));

    fireEvent.change(screen.getByPlaceholderText(/path\/to\/repo/i), { target: { value: 'git@github.com:me/repo.git' } });
    // Type a token under the default auto-detect (field is visible there)…
    fireEvent.change(screen.getByPlaceholderText('••••••••'), { target: { value: 'ghp_secret' } });
    // …then switch to None, hiding the field but leaving the state behind.
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'none' } });
    fireEvent.click(screen.getByRole('button', { name: /^create$/i }));

    await waitFor(() => expect(api.createRepo).toHaveBeenCalled());
    const body = (api.createRepo as unknown as ReturnType<typeof vi.fn>).mock.calls[0][0];
    expect(body.origin).toMatchObject({ auth_method: 'none', auth_token: '' });
  });

  // Regression: basic auth needs a username. The form exposes a dedicated
  // username field for "basic" and assembles "user:password" into auth_token —
  // the convention the backend (assembleAuthToken / remoteAuthFromRecord /
  // authConfigFromSpec) splits on. Previously the single field sent only the
  // password, producing BasicAuth{Username:"", ...} which fails on real hosts.
  it('assembles user:password into auth_token for basic auth', async () => {
    render(<CreateRepoForm onDone={() => {}} onCancel={() => {}} />);
    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'work' } });
    fireEvent.click(screen.getByRole('button', { name: /clone remote/i }));

    fireEvent.change(screen.getByPlaceholderText(/path\/to\/repo/i), { target: { value: 'https://git.example.com/repo.git' } });
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'basic' } });
    fireEvent.change(screen.getByPlaceholderText('username'), { target: { value: 'alice' } });
    fireEvent.change(screen.getByPlaceholderText('••••••••'), { target: { value: 's3cret' } });
    fireEvent.click(screen.getByRole('button', { name: /^create$/i }));

    await waitFor(() => expect(api.createRepo).toHaveBeenCalled());
    const body = (api.createRepo as unknown as ReturnType<typeof vi.fn>).mock.calls[0][0];
    expect(body.origin).toMatchObject({ auth_method: 'basic', auth_token: 'alice:s3cret' });
  });

  // Regression: basic auth with a blank username must NOT submit — otherwise the
  // colon-less token is read by the backend as Password with an empty Username,
  // reproducing the exact broken-credential case basic support exists to avoid.
  it('blocks submit for basic auth when username is blank', async () => {
    render(<CreateRepoForm onDone={() => {}} onCancel={() => {}} />);
    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'work' } });
    fireEvent.click(screen.getByRole('button', { name: /clone remote/i }));

    fireEvent.change(screen.getByPlaceholderText(/path\/to\/repo/i), { target: { value: 'https://git.example.com/repo.git' } });
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'basic' } });
    // Password only, no username.
    fireEvent.change(screen.getByPlaceholderText('••••••••'), { target: { value: 's3cret' } });

    const createBtn = screen.getByRole('button', { name: /^create$/i }) as HTMLButtonElement;
    expect(createBtn.disabled).toBe(true);
    fireEvent.click(createBtn);
    expect(api.createRepo).not.toHaveBeenCalled();

    // Supplying a username unblocks submit.
    fireEvent.change(screen.getByPlaceholderText('username'), { target: { value: 'alice' } });
    expect((screen.getByRole('button', { name: /^create$/i }) as HTMLButtonElement).disabled).toBe(false);
  });
});
