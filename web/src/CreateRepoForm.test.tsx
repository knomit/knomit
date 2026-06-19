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
});
