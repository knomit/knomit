import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent, cleanup } from '@testing-library/react';
import { ArchivedSettings } from './ArchivedSettings';
import { api, type ArchivedRepo } from './api';
import { formatBytes } from './utils';

// An archived repo is a target of its own now: pinned from the rail's
// Archived group, one repo per page, with Settings as its only facet. The page
// carries what ArchivedRepo carries — the date, the origin, the size on disk
// and why it was archived — plus Restore, and Purge in its danger zone.

vi.mock('./api', async importOriginal => ({
  ...(await importOriginal<typeof import('./api')>()),
  api: {
    restoreRepo: vi.fn().mockResolvedValue({ name: 'q3-planning' }),
    purgeRepo: vi.fn().mockResolvedValue(undefined),
  },
}));

const Q3: ArchivedRepo = {
  id: 'id-q3', name: 'q3-planning', origin: 'git@example.com:me/q3.git',
  archivedAt: '2026-08-12T10:00:00Z', sizeBytes: 3_145_728,
  reason: { source: 'user', reason: '', note: 'quarter closed', condition: '', summary: 'Archived by the user: quarter closed' },
};

function renderPage(over: Partial<React.ComponentProps<typeof ArchivedSettings>> = {}) {
  const props = {
    info: Q3,
    serverReadOnly: false,
    activeNames: new Set(['core-kb']),
    onRestored: vi.fn(),
    onPurged: vi.fn(),
    onError: vi.fn(),
    ...over,
  };
  render(<ArchivedSettings {...props} />);
  return props;
}

beforeEach(() => { vi.clearAllMocks(); });
afterEach(() => { cleanup(); });

describe('ArchivedSettings — one archived repo', () => {
  it('has exactly the Archived and Danger zone sections, indexed under "Repository"', () => {
    renderPage();
    expect(screen.getByTestId('settings-index-heading')).toHaveTextContent(/^Repository$/);
    const ids = Array.from(document.querySelectorAll('section[id]')).map(s => s.id);
    expect(ids).toEqual(['archived', 'danger']);
    expect(screen.getByTestId('toc-danger')).toHaveTextContent('Danger zone');
  });

  it('the Archived section shows the size on disk and why it was archived', () => {
    renderPage();
    const block = screen.getByTestId('block-archived');
    expect(block).toHaveTextContent(formatBytes(3_145_728));
    expect(screen.getByTestId('archived-size-id-q3')).toHaveTextContent('3.0 MB');
    expect(screen.getByTestId('archived-reason-id-q3')).toHaveTextContent('Archived by the user: quarter closed');
    expect(block).toHaveTextContent('git@example.com:me/q3.git');
    // Restore lives here; Purge does not.
    expect(block).toContainElement(screen.getByTestId('archived-restore-id-q3'));
    expect(block).not.toContainElement(screen.getByTestId('archived-purge-id-q3'));
    expect(screen.getByTestId('block-danger')).toContainElement(screen.getByTestId('archived-purge-id-q3'));
  });

  it('says nothing about a size or a reason an older server did not send', () => {
    renderPage({ info: { id: 'o.4', name: 'olderserver', origin: '', archivedAt: '2026-01-01T00:00:00Z' } });
    // Control: the section itself renders.
    expect(screen.getByTestId('block-archived')).toBeInTheDocument();
    expect(screen.queryByTestId('archived-size-o.4')).toBeNull();
    expect(screen.queryByTestId('archived-reason-o.4')).toBeNull();
  });

  it('explains a system archive, and that restoring re-runs the check', () => {
    renderPage({ info: { id: 's.2', name: 'linked', origin: '', archivedAt: '2026-10-02T00:00:00Z',
      reason: { source: 'system', reason: '.knomit/ontology.yaml is a symlink', note: '', condition: 'unopenable',
        summary: 'Archived automatically: .knomit/ontology.yaml is a symlink' } } });
    const why = screen.getByTestId('archived-reason-s.2');
    expect(why).toHaveTextContent('Archived automatically: .knomit/ontology.yaml is a symlink');
    expect(why).toHaveTextContent('Restoring re-runs the check');
    expect(screen.getByTestId('archived-condition-s.2')).toHaveTextContent('unopenable');
  });

  it('Restore calls restoreRepo with the id and then onRestored with the restored name', async () => {
    const p = renderPage();
    fireEvent.click(screen.getByTestId('archived-restore-id-q3'));
    await waitFor(() => expect(p.onRestored).toHaveBeenCalledWith('q3-planning'));
    expect(api.restoreRepo).toHaveBeenCalledWith('id-q3', '');
  });

  it('asks for a new name when the name is taken, and restores under it', async () => {
    vi.mocked(api.restoreRepo).mockResolvedValue({ name: 'q3-again' });
    const p = renderPage({ activeNames: new Set(['q3-planning']) });
    fireEvent.click(screen.getByTestId('archived-restore-id-q3'));
    expect(api.restoreRepo).not.toHaveBeenCalled();
    fireEvent.change(screen.getByTestId('restore-name-input-id-q3'), { target: { value: 'q3-again' } });
    fireEvent.click(screen.getByTestId('restore-confirm-id-q3'));
    await waitFor(() => expect(p.onRestored).toHaveBeenCalledWith('q3-again'));
    expect(api.restoreRepo).toHaveBeenCalledWith('id-q3', 'q3-again');
  });

  it('Purge requires the typed name, then calls purgeRepo with the id and onPurged', async () => {
    const p = renderPage();
    fireEvent.click(screen.getByTestId('archived-purge-id-q3'));
    const confirm = screen.getByTestId('purge-confirm-id-q3');
    expect(confirm).toBeDisabled();
    fireEvent.change(screen.getByTestId('purge-confirm-input-id-q3'), { target: { value: 'q3-plan' } });
    expect(confirm).toBeDisabled();
    fireEvent.change(screen.getByTestId('purge-confirm-input-id-q3'), { target: { value: 'q3-planning' } });
    expect(confirm).toBeEnabled();
    fireEvent.click(confirm);
    await waitFor(() => expect(p.onPurged).toHaveBeenCalledTimes(1));
    expect(api.purgeRepo).toHaveBeenCalledWith('id-q3');
  });

  it('disables Restore and Purge on a read-only server, and enables both otherwise', () => {
    renderPage();
    expect(screen.getByTestId('archived-restore-id-q3')).toBeEnabled();
    expect(screen.getByTestId('archived-purge-id-q3')).toBeEnabled();
    cleanup();
    renderPage({ serverReadOnly: true });
    expect(screen.getByTestId('archived-restore-id-q3')).toBeDisabled();
    expect(screen.getByTestId('archived-purge-id-q3')).toBeDisabled();
  });
});
