import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { RemoteConnectWizard } from './RemoteConnectWizard';

// RemoteConnectWizard has the same "classified one way, sent another" asymmetry
// the create wizard had: it decides whether the chosen auth method fits the
// address by reading the RAW field. With a leading space neither isSSHURL nor
// isHTTPURL matched, so authMismatch computed as empty, the inline guard was
// silently skipped and Test stayed enabled.
//
// The server trims, so the request itself still worked — this is not a
// correctness bug. What was lost is the guidance: the user got the same
// refusal from the server a round trip later, in a place with nothing attached
// telling them which of the two to change.

vi.mock('./api', () => ({
  api: {
    getOrigin: vi.fn().mockResolvedValue(null),
    listClientSessions: vi.fn().mockResolvedValue({
      sessions: [],
      policy: { dead_after_s: 3600, hidden_after_s: 10800, retention_s: 604800, live_window_s: 360 },
    }),
  },
  createSession: vi.fn(),
  streamTest: vi.fn(),
  streamPreview: vi.fn(),
  streamApply: vi.fn(),
  streamCommit: vi.fn(),
  deleteSession: vi.fn().mockResolvedValue(undefined),
}));

import { createSession } from './api';
type Fn = ReturnType<typeof vi.fn>;

const HTTP = 'https://github.com/knomit/kb.git';
const SSH = 'git@github.com:knomit/kb.git';

async function renderWizard() {
  render(<RemoteConnectWizard repo="knomit" onCancel={() => {}} onDone={() => {}} />);
  return (await screen.findByTestId('wizard-url')) as HTMLInputElement;
}

function setAuth(method: string) {
  fireEvent.change(screen.getByLabelText('Auth method'), { target: { value: method } });
}

describe('RemoteConnectWizard classifies the URL it will send', () => {
  beforeEach(() => vi.clearAllMocks());

  // The guard must fire on a padded URL exactly as it does on a clean one.
  it.each([
    ['clean', HTTP],
    ['leading space', ` ${HTTP}`],
    ['trailing space', `${HTTP} `],
    ['tab', `\t${HTTP}\t`],
    ['newline', `${HTTP}\n`],
  ])('warns that SSH auth cannot be used with an HTTP URL — %s', async (_name, typed) => {
    const url = await renderWizard();
    fireEvent.change(url, { target: { value: typed } });
    setAuth('ssh');

    await waitFor(() =>
      expect(screen.getByText('SSH auth cannot be used with HTTP/HTTPS URLs')).toBeInTheDocument());
    expect(screen.getByTestId('wizard-test')).toBeDisabled();
  });

  it.each([
    ['clean', SSH],
    ['leading space', ` ${SSH}`],
    ['tab', `\t${SSH}\t`],
  ])('warns that token auth cannot be used with an SSH URL — %s', async (_name, typed) => {
    const url = await renderWizard();
    fireEvent.change(url, { target: { value: typed } });
    setAuth('token');

    await waitFor(() =>
      expect(screen.getByText('Token/basic auth cannot be used with SSH URLs')).toBeInTheDocument());
    expect(screen.getByTestId('wizard-test')).toBeDisabled();
  });

  // And the URL that goes out is the trimmed one, so the session stores what
  // the wizard classified.
  it('creates the session with the trimmed URL', async () => {
    (createSession as unknown as Fn).mockResolvedValueOnce({ session_id: 'sess-1' });
    (vi.mocked(await import('./api')).streamTest as unknown as Fn).mockImplementation(() => () => {});

    const url = await renderWizard();
    fireEvent.change(url, { target: { value: ` \t${HTTP}\t ` } });
    fireEvent.click(screen.getByTestId('wizard-test'));

    await waitFor(() => expect(createSession).toHaveBeenCalled());
    expect((createSession as unknown as Fn).mock.calls[0][1]).toMatchObject({ url: HTTP });
  });
});
