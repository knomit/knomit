import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { RemoteConnectWizard } from './RemoteConnectWizard';
import { KNOMIT_AUTH_WRONG } from './originAuth';

// The wizard offers "Instance certificate" only for a knomit+https origin, and
// blocks Test on the same fleet mismatch validateURLAuth refuses server-side.

vi.mock('./api', () => ({
  api: {
    getOrigin: vi.fn().mockResolvedValue(null),
    listClientSessions: vi.fn().mockResolvedValue({
      sessions: [],
      policy: { dead_after_s: 3600, hidden_after_s: 10800, retention_s: 604800, live_window_s: 360, limit: 500, max_limit: 2000 },
    }),
  },
  createSession: vi.fn(),
  streamTest: vi.fn(),
  streamPreview: vi.fn(),
  streamApply: vi.fn(),
  streamCommit: vi.fn(),
  deleteSession: vi.fn().mockResolvedValue(undefined),
}));

const FLEET = 'knomit+https://bravo.tail:8443/git/kb';

async function renderWizard() {
  render(<RemoteConnectWizard repo="knomit" onCancel={() => {}} onDone={() => {}} />);
  return (await screen.findByTestId('wizard-url')) as HTMLInputElement;
}

function certOption() {
  return screen.queryByRole('option', { name: 'Instance certificate' });
}

describe('RemoteConnectWizard and knomit+https', () => {
  beforeEach(() => vi.clearAllMocks());

  it('offers the certificate only for a knomit+https URL', async () => {
    const url = await renderWizard();
    fireEvent.change(url, { target: { value: 'https://github.com/o/r.git' } });
    expect(certOption()).toBeNull();
    fireEvent.change(url, { target: { value: FLEET } });
    await waitFor(() => expect(certOption()).not.toBeNull());
  });

  it('blocks a token on a knomit+https URL with the server wording', async () => {
    const url = await renderWizard();
    fireEvent.change(url, { target: { value: FLEET } });
    fireEvent.change(screen.getByLabelText('Auth method'), { target: { value: 'token' } });
    await waitFor(() => expect(screen.getByText(KNOMIT_AUTH_WRONG)).toBeInTheDocument());
    expect(screen.getByTestId('wizard-test')).toBeDisabled();
  });

  it('allows cert on a knomit+https URL', async () => {
    const url = await renderWizard();
    fireEvent.change(url, { target: { value: FLEET } });
    await waitFor(() => expect(certOption()).not.toBeNull());
    fireEvent.change(screen.getByLabelText('Auth method'), { target: { value: 'cert' } });
    expect(screen.getByTestId('wizard-test')).toBeEnabled();
  });
});
