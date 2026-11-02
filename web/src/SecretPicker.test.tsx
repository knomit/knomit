import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor, cleanup } from '@testing-library/react';
import { SecretPicker } from './SecretPicker';
import type { SecretView } from './api';

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>();
  return { ...actual, api: { listSecrets: vi.fn(), createSecret: vi.fn() } };
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
  .map(o => o.getAttribute('value')).filter(v => v && !v.startsWith('\u0000'));

describe('SecretPicker', () => {
  it('offers token and basic secrets for the https host only', async () => {
    render(<SecretPicker url="https://github.com/a/b.git" value="" onChange={vi.fn()} />);
    await screen.findByTestId('secret-picker-select');
    expect(options()).toEqual(['gh-token', 'gh-basic']);
  });

  it('offers SSH keys for an SSH remote', async () => {
    render(<SecretPicker url="git@github.com:a/b.git" value="" onChange={vi.fn()} />);
    await screen.findByTestId('secret-picker-select');
    expect(options()).toEqual(['gh-ssh']);
  });

  it('matches a host entry with a port only to that port', async () => {
    render(<SecretPicker url="https://git.example:8443/a.git" value="" onChange={vi.fn()} />);
    await screen.findByTestId('secret-picker-select');
    expect(options()).toEqual(['port']);
  });

  it('says why nothing is listed', async () => {
    render(<SecretPicker url="https://bitbucket.org/a/b.git" value="" onChange={vi.fn()} />);
    expect(await screen.findByTestId('secret-picker-none')).toHaveTextContent('bitbucket.org');
  });

  it('creates a new secret inline with the remote host allowed, and selects it', async () => {
    const api = await mocked();
    const created = secret({ id: 'new', name: 'new-pat' });
    api.createSecret.mockResolvedValue(created);
    const onChange = vi.fn();
    render(<SecretPicker url="https://github.com/a/b.git" value="" onChange={onChange} allowCreate />);
    fireEvent.change(await screen.findByTestId('secret-picker-select'), { target: { value: '\u0000new' } });
    expect(screen.getByTestId('secret-hosts')).toHaveValue('github.com');
    fireEvent.change(screen.getByTestId('secret-name'), { target: { value: 'new-pat' } });
    fireEvent.change(screen.getByTestId('secret-value'), { target: { value: 'v' } });
    api.listSecrets.mockResolvedValue([created]);
    fireEvent.click(screen.getByTestId('secret-form-submit'));
    await waitFor(() => expect(onChange).toHaveBeenCalledWith('new', created));
    expect(api.createSecret).toHaveBeenCalledWith(expect.objectContaining({ hosts: ['github.com'], type: 'token' }));
  });
});
