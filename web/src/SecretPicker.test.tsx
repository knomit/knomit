import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor, cleanup } from '@testing-library/react';
import { useState } from 'react';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';
import { SecretPicker } from './SecretPicker';
import { AccessSecretsLink, type AccessSecretsLinkValue } from './accessLink';
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
  .filter(o => !o.disabled).map(o => o.getAttribute('value'));

/** A controlled picker, as every caller holds it; the URL can be changed. */
function Held({ url: start, initial = '', preselect = false, onChange = () => {} }: {
  url: string; initial?: string; preselect?: boolean; onChange?: (id: string) => void;
}) {
  const [v, setV] = useState(initial);
  const [url, setUrl] = useState(start);
  return (
    <>
      <input data-testid="url" value={url} onChange={e => setUrl(e.target.value)} />
      <output data-testid="value">{v}</output>
      <SecretPicker url={url} value={v} preselect={preselect} onChange={id => { setV(id); onChange(id); }} />
    </>
  );
}
const valueNow = () => screen.getByTestId('value').textContent;
const link = (over: Partial<AccessSecretsLinkValue>): AccessSecretsLinkValue => ({ exists: true, open: null, serverOpen: false, returns: 0, ...over });

describe('SecretPicker: what it lists', () => {
  it('lists only the token and basic secrets for the https host, and has no create option', async () => {
    render(<Held url="https://github.com/a/b.git" />);
    await screen.findByTestId('secret-picker-select');
    expect(options()).toEqual(['gh-token', 'gh-basic']);
    expect(screen.queryByText(/create a new secret/i)).toBeNull();
    expect(screen.queryByText(/no stored secret fits/i)).toBeNull();
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
});

describe('SecretPicker: what it chooses', () => {
  it('without preselect starts with nothing chosen, and the placeholder cannot be chosen', async () => {
    const onChange = vi.fn();
    render(<Held url="https://github.com/a/b.git" onChange={onChange} />);
    const select = await screen.findByTestId('secret-picker-select');
    expect(select).toHaveValue('');
    expect(select.querySelector('option[value=""]')).toBeDisabled();
    expect(onChange).not.toHaveBeenCalled();
  });

  it('with preselect chooses the first fitting secret once the list arrives, and a pick changes it', async () => {
    render(<Held url="https://github.com/a/b.git" preselect />);
    const select = await screen.findByTestId('secret-picker-select');
    await waitFor(() => expect(valueNow()).toBe('gh-token'));
    fireEvent.change(select, { target: { value: 'gh-basic' } });
    expect(valueNow()).toBe('gh-basic');
    expect(select).toHaveValue('gh-basic');
  });

  it('preselects a secret that has not expired over an expired one listed first', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([
      secret({ id: 'old', name: 'old', status: 'expired' }),
      secret({ id: 'ok', name: 'ok', status: 'expiring' }),
    ]);
    render(<Held url="https://github.com/a/b.git" preselect />);
    await waitFor(() => expect(valueNow()).toBe('ok'));
  });

  it('preselects an expired secret only when nothing else fits', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([secret({ id: 'old', name: 'old', status: 'expired' })]);
    render(<Held url="https://github.com/a/b.git" preselect />);
    await waitFor(() => expect(valueNow()).toBe('old'));
  });

  it('clears a starting value that does not fit and says why, rather than swapping in another', async () => {
    render(<Held url="https://github.com/a/b.git" initial="gl-token" preselect />);
    expect(await screen.findByTestId('secret-picker-lost')).toHaveTextContent("gl-token isn't allowed for github.com; pick another.");
    await waitFor(() => expect(valueNow()).toBe(''));
    expect(screen.getByTestId('secret-picker-select')).toHaveValue('');
  });

  it("keeps the reader's pick across a URL edit that passes through hosts that fit nothing", async () => {
    render(<Held url="https://github.com/a/b.git" preselect />);
    const select = await screen.findByTestId('secret-picker-select');
    await waitFor(() => expect(valueNow()).toBe('gh-token'));
    fireEvent.change(select, { target: { value: 'gh-basic' } });
    // Retyping the host: midway it fits nothing, and the pick is cleared...
    fireEvent.change(screen.getByTestId('url'), { target: { value: 'https://github.c' } });
    await waitFor(() => expect(valueNow()).toBe(''));
    expect(screen.getByTestId('secret-picker-lost')).toHaveTextContent("gh-basic isn't allowed for github.c");
    // ...and comes back, not the preselected first, once it fits again.
    fireEvent.change(screen.getByTestId('url'), { target: { value: 'https://github.com/a/b.git' } });
    await waitFor(() => expect(valueNow()).toBe('gh-basic'));
    expect(screen.queryByTestId('secret-picker-lost')).toBeNull();
  });
});

describe('SecretPicker: none fit', () => {
  it('shows no select, names the kind of secret and the host, and clears the value', async () => {
    render(<Held url="https://bitbucket.org/a/b.git" initial="gh-token" />);
    expect(await screen.findByTestId('secret-picker-none'))
      .toHaveTextContent('No token or username + password secret is allowed for bitbucket.org. Add one in Access → Secrets.');
    expect(screen.queryByTestId('secret-picker-select')).toBeNull();
    await waitFor(() => expect(valueNow()).toBe(''));
  });

  it('names the SSH-key kind when the host has secrets, but none of the type an SSH URL needs', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([secret({ id: 'gh-token', name: 'gh-token' })]);
    render(<Held url="git@github.com:a/b.git" />);
    expect(await screen.findByTestId('secret-picker-none')).toHaveTextContent('No SSH-key secret is allowed for github.com.');
  });

  it('"Access → Secrets" opens the Access page when App provides the way there', async () => {
    const open = vi.fn();
    render(<AccessSecretsLink.Provider value={link({ open })}><Held url="https://bitbucket.org/a/b.git" /></AccessSecretsLink.Provider>);
    fireEvent.click(await screen.findByRole('button', { name: 'Access → Secrets' }));
    expect(open).toHaveBeenCalledTimes(1);
  });

  it('names Access → Secrets as plain text while it cannot be opened', async () => {
    render(<Held url="https://bitbucket.org/a/b.git" />);
    expect(await screen.findByTestId('secret-picker-none')).toHaveTextContent('Add one in Access → Secrets.');
    expect(screen.queryByTestId('secret-picker-access-link')).toBeNull();
  });

  it('does not point at Access → Secrets on a server that has no Access page', async () => {
    render(<AccessSecretsLink.Provider value={link({ exists: false })}><Held url="https://bitbucket.org/a/b.git" /></AccessSecretsLink.Provider>);
    const none = await screen.findByTestId('secret-picker-none');
    expect(none).toHaveTextContent('No token or username + password secret is allowed for bitbucket.org.');
    expect(none).not.toHaveTextContent('Access');
  });

  it('re-reads the secrets when the server pages close, and picks up one added there', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([]);
    const tree = (serverOpen: boolean, returns: number) => (
      <AccessSecretsLink.Provider value={link({ open: () => {}, serverOpen, returns })}>
        <Held url="https://github.com/a/b.git" preselect />
      </AccessSecretsLink.Provider>
    );
    const { rerender } = render(tree(false, 0));
    await screen.findByTestId('secret-picker-none');
    rerender(tree(true, 0));
    api.listSecrets.mockResolvedValue([secret({ id: 'added', name: 'added' })]);
    rerender(tree(false, 1));
    await waitFor(() => expect(valueNow()).toBe('added'));
  });

  it('hands the focus from the link to the select when the re-read replaces one with the other', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([]);
    const tree = (returns: number) => (
      <AccessSecretsLink.Provider value={link({ open: () => {}, returns })}>
        <Held url="https://github.com/a/b.git" />
      </AccessSecretsLink.Provider>
    );
    const { rerender } = render(tree(0));
    const a = await screen.findByTestId('secret-picker-access-link');
    a.focus();
    expect(a).toHaveFocus();
    api.listSecrets.mockResolvedValue([secret({ id: 'added', name: 'added' })]);
    rerender(tree(1));
    await waitFor(() => expect(screen.getByTestId('secret-picker-select')).toHaveFocus());
  });

  it('passes the link itself to open, so focus can come back to it', async () => {
    const open = vi.fn();
    render(<AccessSecretsLink.Provider value={link({ open })}><Held url="https://bitbucket.org/a/b.git" /></AccessSecretsLink.Provider>);
    const a = await screen.findByTestId('secret-picker-access-link');
    fireEvent.click(a);
    expect(open).toHaveBeenCalledWith(a);
  });
});

describe('secrets are created only in Access → Secrets', () => {
  // import … from './SecretForm', import('./SecretForm'), export … from
  // '../x/SecretForm', with or without an extension, in any quotes.
  const ref = /(?:\bfrom\s*|\bimport\s*\(\s*|\bimport\s+)['"`][^'"`]*\/SecretForm(?:\.[jt]sx?)?['"`]/;

  /** Every source file under web/src, at any depth, but not the tests. */
  function sources(dir: string): string[] {
    return readdirSync(dir).flatMap(n => {
      const p = join(dir, n);
      if (statSync(p).isDirectory()) return n === 'node_modules' ? [] : sources(p);
      return /\.(tsx?|jsx?|mjs)$/.test(n) && !/\.test\./.test(n) ? [p] : [];
    });
  }

  it('nothing but SecretsTab imports or re-exports SecretForm, anywhere under web/src', () => {
    const users = sources(__dirname).filter(f => ref.test(readFileSync(f, 'utf8'))).map(f => relative(__dirname, f));
    expect(users).toEqual(['SecretsTab.tsx']);
  });

  it('the guard matches a re-export and a dynamic import, and not a lookalike name', () => {
    expect(ref.test("export { SecretForm } from '../forms/SecretForm';")).toBe(true);
    expect(ref.test('const m = await import("./SecretForm.tsx");')).toBe(true);
    expect(ref.test("import { SecretFormX } from './SecretFormX';")).toBe(false);
  });
});
