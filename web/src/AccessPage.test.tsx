import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup } from '@testing-library/react';
import { AccessPage } from './AccessPage';
import { accessTabsFor } from './facets';

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>();
  return { ...actual, api: { listSecrets: vi.fn(), listOAuthPending: vi.fn() } };
});

beforeEach(async () => {
  const { api } = await import('./api');
  vi.mocked(api.listSecrets).mockResolvedValue([]);
  vi.mocked(api.listOAuthPending).mockResolvedValue([]);
});
afterEach(() => { cleanup(); vi.clearAllMocks(); });

describe('accessTabsFor', () => {
  it('is Secrets alone without an OAuth issuer, Secrets then Authorizations with one', () => {
    expect(accessTabsFor(false)).toEqual(['secrets']);
    expect(accessTabsFor(true)).toEqual(['secrets', 'auth']);
  });
});

describe('AccessPage', () => {
  it('omits Authorizations without an issuer and shows Secrets even when asked for it', async () => {
    render(<AccessPage tab="auth" onTab={vi.fn()} oauthAvailable={false} authCount={null} onAuthCount={vi.fn()} />);
    expect(screen.queryByTestId('access-tab-auth')).toBeNull();
    expect(await screen.findByTestId('secrets-tab')).toBeInTheDocument();
  });

  it('switches tabs through onTab, and badges Authorizations with the waiting count', async () => {
    const onTab = vi.fn();
    render(<AccessPage tab="secrets" onTab={onTab} oauthAvailable authCount={3} onAuthCount={vi.fn()} />);
    expect(screen.getByTestId('access-tab-secrets')).toHaveAttribute('aria-selected', 'true');
    expect(screen.getByTestId('access-tab-auth-badge')).toHaveTextContent(/^3$/);
    fireEvent.click(screen.getByTestId('access-tab-auth'));
    expect(onTab).toHaveBeenCalledWith('auth');
  });

  it('renders Authorizations when that tab is chosen', async () => {
    render(<AccessPage tab="auth" onTab={vi.fn()} oauthAvailable authCount={0} onAuthCount={vi.fn()} />);
    expect(await screen.findByTestId('auth-page')).toBeInTheDocument();
    expect(screen.queryByTestId('access-tab-auth-badge')).toBeNull();
  });
});
