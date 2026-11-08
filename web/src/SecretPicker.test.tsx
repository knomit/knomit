import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor, cleanup } from '@testing-library/react';
import { useState } from 'react';
import { SecretPicker } from './SecretPicker';
import { AccessSecretsLink } from './accessLink';
import type { SecretView } from './api';

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>();
  return { ...actual, api: { listSecrets: vi.fn() } };
});

const secret = (over: Partial<SecretView>): SecretView => ({
  id: 'x', name: 'x', type: 'token', hosts: ['github.com'], expires_at: null, status: 'none',
  created_at: '', updated_at: '', linked_repos: 0, ...over,
});

async function mocked() {
  const { api } = await import('./api');
  return vi.mocked(api);
}

beforeEach(async () => {
  const api = await mocked();
  api.listSecrets.mockResolvedValue([
    secret({ id: 'gh-token', name: 'gh-token' }),
    secret({ id: 'gh-basic', name: 'gh-basic', type: 'basic' }),
    secret({ id: 'gh-ssh', name: 'gh-ssh', type: 'ssh_key' }),
    secret({ id: 'gl-token', name: 'gl-token', hosts: ['gitlab.com'] }),
    secret({ id: 'port', name: 'port', hosts: ['git.example:8443'] }),
  ]);
});
afterEach(() => { cleanup(); vi.clearAllMocks(); });

const options = () => Array.from(screen.getByTestId('secret-picker-select').querySelectorAll('option'))
  .map(o => o.getAttribute('value'));

/** A controlled picker, as every caller holds it. */
function Held({ url, onChange = () => {} }: { url: string; onChange?: (id: string) => void }) {
  const [v, setV] = useState('');
  return <SecretPicker url={url} value={v} onChange={id => { setV(id); onChange(id); }} />;
}

describe('SecretPicker', () => {
  it('lists only the token and basic secrets for the https host, with no placeholder and no create option', async () => {
    render(<Held url="https://github.com/a/b.git" />);
    await screen.findByTestId('secret-picker-select');
    expect(options()).toEqual(['gh-token', 'gh-basic']);
    expect(screen.queryByText(/create a new secret/i)).toBeNull();
    expect(screen.queryByText(/no stored secret fits/i)).toBeNull();
  });

  it('chooses the first fitting secret once the list arrives, and a pick changes it', async () => {
    const onChange = vi.fn();
    render(<Held url="https://github.com/a/b.git" onChange={onChange} />);
    const select = await screen.findByTestId('secret-picker-select');
    await waitFor(() => expect(onChange).toHaveBeenCalledWith('gh-token'));
    expect(select).toHaveValue('gh-token');
    fireEvent.change(select, { target: { value: 'gh-basic' } });
    expect(onChange).toHaveBeenLastCalledWith('gh-basic');
    expect(select).toHaveValue('gh-basic');
  });

  it('replaces a value that does not fit with the first that does', async () => {
    const onChange = vi.fn();
    render(<SecretPicker url="https://github.com/a/b.git" value="gl-token" onChange={onChange} />);
    await waitFor(() => expect(onChange).toHaveBeenCalledWith('gh-token', expect.objectContaining({ id: 'gh-token' })));
  });

  it('offers SSH keys for an SSH remote', async () => {
    render(<Held url="git@github.com:a/b.git" />);
    await screen.findByTestId('secret-picker-select');
    expect(options()).toEqual(['gh-ssh']);
  });

  it('matches a host entry with a port only to that port', async () => {
    render(<Held url="https://git.example:8443/a.git" />);
    await screen.findByTestId('secret-picker-select');
    expect(options()).toEqual(['port']);
  });

  it('with none fitting: no select, a line naming the host, and the value cleared', async () => {
    const onChange = vi.fn();
    render(<SecretPicker url="https://bitbucket.org/a/b.git" value="gh-token" onChange={onChange} />);
    expect(await screen.findByTestId('secret-picker-none'))
      .toHaveTextContent('No stored secret is allowed for bitbucket.org. Add one in Access → Secrets.');
    expect(screen.queryByTestId('secret-picker-select')).toBeNull();
    await waitFor(() => expect(onChange).toHaveBeenCalledWith('', null));
  });

  it('"Access → Secrets" opens the Access page when App provides the way there', async () => {
    const open = vi.fn();
    render(
      <AccessSecretsLink.Provider value={{ open, serverOpen: false }}>
        <Held url="https://bitbucket.org/a/b.git" />
      </AccessSecretsLink.Provider>,
    );
    fireEvent.click(await screen.findByRole('button', { name: 'Access → Secrets' }));
    expect(open).toHaveBeenCalledTimes(1);
  });

  it('names Access → Secrets as plain text where there is no way there', async () => {
    render(<Held url="https://bitbucket.org/a/b.git" />);
    await screen.findByTestId('secret-picker-none');
    expect(screen.queryByTestId('secret-picker-access-link')).toBeNull();
  });

  it('re-reads the secrets when the server pages close, and picks up one added there', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([]);
    const tree = (serverOpen: boolean) => (
      <AccessSecretsLink.Provider value={{ open: () => {}, serverOpen }}>
        <Held url="https://github.com/a/b.git" />
      </AccessSecretsLink.Provider>
    );
    const { rerender } = render(tree(false));
    await screen.findByTestId('secret-picker-none');
    rerender(tree(true));
    api.listSecrets.mockResolvedValue([secret({ id: 'added', name: 'added' })]);
    rerender(tree(false));
    await waitFor(() => expect(screen.getByTestId('secret-picker-select')).toHaveValue('added'));
  });
});

describe('secrets are created only in Access → Secrets', () => {
  it('SecretForm is used by SecretsTab alone', () => {
    const files = import.meta.glob(['./*.tsx', '!./*.test.tsx'], { query: '?raw', import: 'default', eager: true }) as Record<string, string>;
    const users = Object.entries(files)
      .filter(([, src]) => /from '\.\/SecretForm'/.test(src))
      .map(([f]) => f);
    expect(users).toEqual(['./SecretsTab.tsx']);
  });
});
