import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup, waitFor, within, act } from '@testing-library/react';
import { AccessPage } from './AccessPage';
import { accessTabsFor } from './facets';
import type { SecretView } from './api';

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>();
  return { ...actual, api: { listSecrets: vi.fn(), getSecret: vi.fn(), listOAuthPending: vi.fn(), listOAuthGrants: vi.fn() } };
});

const secret = (over: Partial<SecretView> = {}): SecretView => ({
  id: 's1', name: 'gh-pat', type: 'token', hosts: ['github.com'], expires_at: null, status: 'none',
  created_at: '2026-11-01T00:00:00Z', updated_at: '2026-11-01T00:00:00Z', linked_repos: 0, ...over,
});

async function mocked() {
  const { api } = await import('./api');
  return vi.mocked(api);
}

beforeEach(async () => {
  const api = await mocked();
  api.listSecrets.mockResolvedValue([]);
  api.getSecret.mockImplementation(async (id: string) => ({ ...secret({ id }), repos: [] }));
  api.listOAuthPending.mockResolvedValue([]);
  api.listOAuthGrants.mockResolvedValue([]);
});
afterEach(() => { cleanup(); vi.clearAllMocks(); });

// The page's own width decides its mode; jsdom has no layout, so a test
// stands in for one by giving every element a clientWidth.
function atWidth(px: number) {
  const d = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'clientWidth');
  Object.defineProperty(HTMLElement.prototype, 'clientWidth', { configurable: true, get: () => px });
  return () => { if (d) Object.defineProperty(HTMLElement.prototype, 'clientWidth', d); };
}

const page = (over: Partial<Parameters<typeof AccessPage>[0]> = {}) => (
  <AccessPage tab="secrets" onTab={vi.fn()} oauthAvailable={false} authCount={null} onAuthCount={vi.fn()} {...over} />
);

describe('accessTabsFor', () => {
  it('is Secrets alone without an OAuth issuer, Secrets then Authorizations with one', () => {
    expect(accessTabsFor(false)).toEqual(['secrets']);
    expect(accessTabsFor(true)).toEqual(['secrets', 'auth']);
  });
});

describe('AccessPage — the section rail', () => {
  it('omits Authorizations without an issuer and shows Secrets even when asked for it', async () => {
    render(page({ tab: 'auth' }));
    expect(screen.queryByTestId('access-section-auth')).toBeNull();
    expect(screen.getByTestId('access-section-secrets')).toHaveAttribute('aria-current', 'true');
    expect(await screen.findByTestId('secrets-overview')).toBeInTheDocument();
  });

  it('picks a section through onTab, and shows the waiting count on Authorizations', async () => {
    const onTab = vi.fn();
    render(page({ onTab, oauthAvailable: true, authCount: 3 }));
    const auth = screen.getByTestId('access-section-auth');
    expect(within(auth).getByLabelText('3 waiting')).toHaveTextContent(/^3$/);
    fireEvent.click(auth);
    expect(onTab).toHaveBeenCalledWith('auth');
  });

  it('shows no waiting count at zero', async () => {
    render(page({ oauthAvailable: true, authCount: 0 }));
    expect(within(screen.getByTestId('access-section-auth')).queryByLabelText(/waiting/)).toBeNull();
  });

  it('counts the secrets and flags one that needs attention, the worst first', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([
      secret({ id: 'a', name: 'a', status: 'expiring', expires_at: '2099-01-01T00:00:00Z' }),
      secret({ id: 'b', name: 'b', status: 'expired', expires_at: '2020-01-01T00:00:00Z' }),
    ]);
    render(page());
    const s = screen.getByTestId('access-section-secrets');
    await waitFor(() => expect(s).toHaveTextContent(/Secrets.*2/));
    expect(within(s).getByLabelText('A secret has expired')).toBeInTheDocument();
  });

  it('walks the sections with ↑/↓ and moves on to the list with →', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([secret()]);
    const onTab = vi.fn();
    render(page({ onTab, oauthAvailable: true, authCount: 0 }));
    await screen.findByTestId('secret-row-gh-pat');
    const rail = screen.getByTestId('access-sections');
    fireEvent.keyDown(rail, { key: 'ArrowDown' });
    expect(onTab).toHaveBeenCalledWith('auth');
    fireEvent.keyDown(rail, { key: 'ArrowRight' });
    expect(document.activeElement).toBe(screen.getByTestId('secret-row-gh-pat'));
  });
});

describe('AccessPage — list and detail', () => {
  it('shows the section overview with nothing picked, and an item\'s detail once picked', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([secret()]);
    render(page());
    expect(await screen.findByTestId('secrets-overview')).toBeInTheDocument();
    fireEvent.click(await screen.findByTestId('secret-row-gh-pat'));
    expect(await screen.findByTestId('secret-editor')).toBeInTheDocument();
    expect(screen.getByTestId('secret-row-gh-pat')).toHaveAttribute('aria-current', 'true');
  });

  it('walks the list with ↑/↓ (and j/k), back to the rail with ←, on to the detail with →', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([secret({ id: 'a', name: 'alpha' }), secret({ id: 'b', name: 'beta' })]);
    render(page());
    const list = await screen.findByTestId('secrets-list');
    await screen.findByTestId('secret-row-beta');
    fireEvent.keyDown(list, { key: 'ArrowDown' });
    expect(await screen.findByTestId('secret-editor')).toHaveTextContent('alpha');
    fireEvent.keyDown(list, { key: 'j' });
    await waitFor(() => expect(screen.getByTestId('secret-editor')).toHaveTextContent('beta'));
    fireEvent.keyDown(list, { key: 'k' });
    await waitFor(() => expect(screen.getByTestId('secret-editor')).toHaveTextContent('alpha'));
    fireEvent.keyDown(list, { key: 'ArrowRight' });
    expect(document.activeElement).toBe(screen.getByTestId('access-detail'));
    fireEvent.keyDown(list, { key: 'ArrowLeft' });
    expect(document.activeElement).toBe(screen.getByTestId('access-section-secrets'));
  });

  it('keeps each section\'s pick while switching between them', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([secret()]);
    const { rerender } = render(page({ oauthAvailable: true, authCount: 0 }));
    fireEvent.click(await screen.findByTestId('secret-row-gh-pat'));
    expect(await screen.findByTestId('secret-editor')).toBeInTheDocument();
    rerender(page({ tab: 'auth', oauthAvailable: true, authCount: 0 }));
    expect(await screen.findByTestId('auth-overview')).toBeInTheDocument();
    rerender(page({ oauthAvailable: true, authCount: 0 }));
    expect(await screen.findByTestId('secret-editor')).toBeInTheDocument();
  });

  it('reads the Authorizations data only while that section shows', async () => {
    const api = await mocked();
    const { rerender } = render(page({ oauthAvailable: true, authCount: 0 }));
    await screen.findByTestId('secrets-overview');
    expect(api.listOAuthPending).not.toHaveBeenCalled();
    rerender(page({ tab: 'auth', oauthAvailable: true, authCount: 0 }));
    await waitFor(() => expect(api.listOAuthPending).toHaveBeenCalled());
    expect(api.listOAuthGrants).toHaveBeenCalled();
  });
});

describe('AccessPage — narrow widths', () => {
  let restore: () => void = () => {};
  afterEach(() => restore());

  it('below the medium width draws the rail as glyphs, named by their labels', async () => {
    restore = atWidth(800);
    render(page({ oauthAvailable: true, authCount: 2 }));
    const s = await screen.findByTestId('access-section-secrets');
    expect(s).toHaveAttribute('aria-label', 'Secrets');
    expect(s).not.toHaveTextContent('Secrets');
    expect(screen.getByTestId('access-section-auth')).toHaveAttribute('aria-label', 'Authorizations');
  });

  it('at a phone width shows the sections as tabs and one column at a time, with a way back', async () => {
    restore = atWidth(500);
    const api = await mocked();
    api.listSecrets.mockResolvedValue([secret()]);
    render(page());
    expect(screen.getByTestId('access-sections')).toHaveAttribute('role', 'tablist');
    expect(screen.getByTestId('access-section-secrets')).toHaveAttribute('aria-selected', 'true');
    fireEvent.click(await screen.findByTestId('secret-row-gh-pat'));
    expect(await screen.findByTestId('secret-editor')).toBeInTheDocument();
    expect(screen.queryByTestId('secrets-list')).toBeNull();
    await act(async () => { fireEvent.click(screen.getByTestId('access-detail-back')); });
    expect(await screen.findByTestId('secrets-list')).toBeInTheDocument();
    expect(screen.queryByTestId('secret-editor')).toBeNull();
  });
});
