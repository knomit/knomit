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
    fireEvent.change(screen.getByPlaceholderText(/name/i), { target: { value: 'work' } });
    fireEvent.click(screen.getByRole('button', { name: /^create$/i }));
    await waitFor(() => expect(api.createRepo).toHaveBeenCalled());
    await waitFor(() => expect(onDone).toHaveBeenCalledWith('work'));
  });
});
