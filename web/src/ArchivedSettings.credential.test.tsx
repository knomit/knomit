import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent, cleanup } from '@testing-library/react';
import { ArchivedSettings } from './ArchivedSettings';
import { api, SecretRefusal, type ArchivedRepo } from './api';

// Restoring an archived repo that has an origin asks which credential it
// uses (archiving unlinked any stored secret): a stored secret, a pasted
// credential, or none, with nothing preselected.

vi.mock('./api', async importOriginal => ({
  ...(await importOriginal<typeof import('./api')>()),
  api: { restoreRepo: vi.fn(), purgeRepo: vi.fn(), listSecrets: vi.fn(), createSecret: vi.fn() },
}));

const KB: ArchivedRepo = { id: 'u1', name: 'kb', origin: 'https://github.com/a/kb.git', archivedAt: '2026-10-01T00:00:00Z' };

function renderPage(info: ArchivedRepo = KB) {
  const props = { info, serverReadOnly: false, activeNames: new Set<string>(), onRestored: vi.fn(), onPurged: vi.fn(), onError: vi.fn() };
  render(<ArchivedSettings {...props} />);
  return props;
}

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(api.restoreRepo).mockResolvedValue({ name: 'kb' });
  vi.mocked(api.listSecrets).mockResolvedValue([
    { id: 's1', name: 'gh-pat', type: 'token', hosts: ['github.com'], expires_at: null, status: 'none', created_at: '', updated_at: '', linked_repos: 0 },
  ]);
});
afterEach(() => { cleanup(); });

const open = () => fireEvent.click(screen.getByTestId('archived-restore-u1'));

describe('Restore asks for the credential', () => {
  it('cannot be confirmed before a choice is made, and nothing is preselected', () => {
    renderPage();
    open();
    for (const c of ['secret', 'pasted', 'none']) expect(screen.getByTestId(`restore-choice-${c}`)).toHaveAttribute('aria-checked', 'false');
    expect(screen.getByTestId('restore-confirm-u1')).toBeDisabled();
    expect(api.restoreRepo).not.toHaveBeenCalled();
  });

  it('a stored secret sends secret_id', async () => {
    const p = renderPage();
    open();
    fireEvent.click(screen.getByTestId('restore-choice-secret'));
    expect(screen.getByTestId('restore-confirm-u1')).toBeDisabled();
    fireEvent.change(await screen.findByTestId('restore-secret-picker-select'), { target: { value: 's1' } });
    fireEvent.click(screen.getByTestId('restore-confirm-u1'));
    await waitFor(() => expect(p.onRestored).toHaveBeenCalledWith('kb'));
    expect(api.restoreRepo).toHaveBeenCalledWith('u1', '', { secret_id: 's1' });
  });

  it('a pasted credential sends its fields', async () => {
    renderPage();
    open();
    fireEvent.click(screen.getByTestId('restore-choice-pasted'));
    fireEvent.change(screen.getByTestId('restore-pasted-token'), { target: { value: 'tok' } });
    fireEvent.click(screen.getByTestId('restore-confirm-u1'));
    await waitFor(() => expect(api.restoreRepo).toHaveBeenCalledWith('u1', '', { auth_method: 'token', token: 'tok' }));
  });

  it('no credential sends no_credential', async () => {
    renderPage();
    open();
    fireEvent.click(screen.getByTestId('restore-choice-none'));
    fireEvent.click(screen.getByTestId('restore-confirm-u1'));
    await waitFor(() => expect(api.restoreRepo).toHaveBeenCalledWith('u1', '', { no_credential: true }));
  });

  it('a repo with no origin restores without the prompt', async () => {
    const p = renderPage({ ...KB, origin: '' });
    open();
    await waitFor(() => expect(p.onRestored).toHaveBeenCalled());
    expect(screen.queryByTestId('restore-prompt-u1')).toBeNull();
  });

  it('a 422 from the server shows its message and keeps the prompt open', async () => {
    vi.mocked(api.restoreRepo).mockRejectedValue(new SecretRefusal('stored secret "gh-pat" is not allowed for host github.com', 422));
    const p = renderPage();
    open();
    fireEvent.click(screen.getByTestId('restore-choice-secret'));
    fireEvent.change(await screen.findByTestId('restore-secret-picker-select'), { target: { value: 's1' } });
    fireEvent.click(screen.getByTestId('restore-confirm-u1'));
    expect(await screen.findByTestId('restore-error-u1')).toHaveTextContent('not allowed for host');
    expect(screen.getByTestId('restore-prompt-u1')).toBeInTheDocument();
    expect(p.onRestored).not.toHaveBeenCalled();
  });
});

describe('Restore prompt Cancel', () => {
  it('forgets the typed credential and the choice', () => {
    renderPage();
    open();
    fireEvent.click(screen.getByTestId('restore-choice-pasted'));
    fireEvent.change(screen.getByTestId('restore-pasted-token'), { target: { value: 'TYPED-SECRET' } });
    fireEvent.click(screen.getByTestId('restore-cancel-u1'));
    open();
    expect(screen.getByTestId('restore-choice-pasted')).toHaveAttribute('aria-checked', 'false');
    fireEvent.click(screen.getByTestId('restore-choice-pasted'));
    expect(screen.getByTestId('restore-pasted-token')).toHaveValue('');
    expect(document.body.innerHTML).not.toContain('TYPED-SECRET');
  });
});
