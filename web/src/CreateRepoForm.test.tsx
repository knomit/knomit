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

  it('clones a local path with no auth: defaults to None, hides token, sends auth_method "none"', async () => {
    render(<CreateRepoForm onDone={() => {}} onCancel={() => {}} />);
    fireEvent.change(screen.getByTestId('create-name'), { target: { value: 'work' } });
    fireEvent.click(screen.getByRole('button', { name: /clone remote/i }));

    // None is the default auth method, so the token field is hidden.
    expect(screen.queryByPlaceholderText('••••••••')).toBeNull();

    fireEvent.change(screen.getByPlaceholderText(/path\/to\/repo/i), { target: { value: '/srv/kb' } });
    fireEvent.click(screen.getByRole('button', { name: /^create$/i }));

    await waitFor(() => expect(api.createRepo).toHaveBeenCalled());
    const body = (api.createRepo as unknown as ReturnType<typeof vi.fn>).mock.calls[0][0];
    expect(body.origin).toMatchObject({ url: '/srv/kb', auth_method: 'none', auth_token: '' });
  });
});
