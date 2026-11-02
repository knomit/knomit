import { describe, it, expect, vi, afterEach } from 'vitest';
import { api, createSession, SecretRefusal, SECRETS_UNAVAILABLE } from './api';

// Stored secrets and the calls that link a repo to one. The server admits
// them only through the local-or-same-origin admin gate, so each carries the
// same-origin proof header; a call with no secret_id stays a simple request.

function mockFetch(status: number, body: unknown) {
  const f = vi.fn().mockResolvedValue({
    ok: status >= 200 && status < 300,
    status,
    statusText: String(status),
    json: () => Promise.resolve(body),
  });
  globalThis.fetch = f as unknown as typeof fetch;
  return f;
}

function call(f: ReturnType<typeof vi.fn>, i = 0) {
  const [url, init] = f.mock.calls[i] as [string, RequestInit];
  return {
    url,
    method: init?.method ?? 'GET',
    header: new Headers(init?.headers).get('X-Knomit-Client'),
    body: init?.body ? JSON.parse(String(init.body)) : undefined,
  };
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe('secrets api', () => {
  it('lists secrets with the proof header and unwraps them', async () => {
    const f = mockFetch(200, { secrets: [{ id: 's1', name: 'gh' }] });
    expect(await api.listSecrets()).toEqual([{ id: 's1', name: 'gh' }]);
    expect(call(f)).toMatchObject({ url: '/api/v1/secrets', method: 'GET', header: 'web' });
  });

  it('gets one secret by id', async () => {
    const f = mockFetch(200, { id: 's/1' });
    await api.getSecret('s/1');
    expect(call(f)).toMatchObject({ url: '/api/v1/secrets/s%2F1', method: 'GET', header: 'web' });
  });

  it('creates a secret with its value in the body', async () => {
    const f = mockFetch(201, { id: 's1' });
    await api.createSecret({ name: 'gh', type: 'token', hosts: ['github.com'], value: 'v' });
    expect(call(f)).toMatchObject({
      url: '/api/v1/secrets', method: 'POST', header: 'web',
      body: { name: 'gh', type: 'token', hosts: ['github.com'], value: 'v' },
    });
  });

  it('patches name, hosts, expiry and value', async () => {
    const f = mockFetch(200, { id: 's1' });
    await api.patchSecret('s1', { hosts: ['a.com', 'b.com'], expires_at: null });
    expect(call(f)).toMatchObject({
      url: '/api/v1/secrets/s1', method: 'PATCH', header: 'web',
      body: { hosts: ['a.com', 'b.com'], expires_at: null },
    });
  });

  it('deletes a secret', async () => {
    const f = mockFetch(204, null);
    await api.deleteSecret('s1');
    expect(call(f)).toMatchObject({ url: '/api/v1/secrets/s1', method: 'DELETE', header: 'web' });
  });

  it('turns a 409 into a SecretRefusal naming the repos and hosts', async () => {
    mockFetch(409, { detail: 'Host(s) a.com are used by: x', repos: [{ name: 'x', uid: 'u', state: 'active' }], hosts: ['a.com'] });
    const err = await api.patchSecret('s1', { hosts: ['b.com'] }).catch(e => e);
    expect(err).toBeInstanceOf(SecretRefusal);
    expect(err).toMatchObject({ status: 409, repos: ['x'], hosts: ['a.com'] });
    expect(err.message).toMatch(/used by: x/);
  });

  it('reads 503 as the instance key being unreadable', async () => {
    mockFetch(503, { detail: 'whatever' });
    await expect(api.listSecrets()).rejects.toThrow(SECRETS_UNAVAILABLE);
  });
});

describe('origin credential api', () => {
  it('replaces a pasted token without the proof header and without secret_id', async () => {
    const f = mockFetch(200, { status: 'ok' });
    await api.setOriginCredential('kb', { auth_method: 'token', token: 't' });
    const c = call(f);
    expect(c).toMatchObject({ url: '/api/v1/repos/kb/origin/credential', method: 'PUT', header: null });
    expect(c.body).toEqual({ auth_method: 'token', token: 't' });
  });

  it('links with secret_id and the proof header', async () => {
    const f = mockFetch(200, { status: 'ok' });
    await api.setOriginCredential('kb', { secret_id: 's1' });
    expect(call(f)).toMatchObject({ method: 'PUT', header: 'web', body: { secret_id: 's1' } });
  });

  it('unlinks with secret_id null, which also needs the gate', async () => {
    const f = mockFetch(200, { status: 'ok' });
    await api.setOriginCredential('kb', { secret_id: null });
    expect(call(f)).toMatchObject({ header: 'web', body: { secret_id: null } });
  });
});

describe('secret_id on the other edges', () => {
  it('restore sends the credential choice', async () => {
    let f = mockFetch(200, { name: 'kb' });
    await api.restoreRepo('u1', undefined, { secret_id: 's1' });
    expect(call(f)).toMatchObject({ url: '/api/v1/archived/u1/restore', header: 'web', body: { secret_id: 's1' } });
    f = mockFetch(200, { name: 'kb' });
    await api.restoreRepo('u1', 'kb2', { no_credential: true });
    expect(call(f)).toMatchObject({ header: null, body: { new_name: 'kb2', no_credential: true } });
    f = mockFetch(200, { name: 'kb' });
    await api.restoreRepo('u1', undefined, { auth_method: 'token', token: 't' });
    expect(call(f).body).toEqual({ auth_method: 'token', token: 't' });
  });

  it('restore surfaces the server detail on a 422', async () => {
    mockFetch(422, { detail: 'stored secret "gh" is not allowed for host example.com' });
    await expect(api.restoreRepo('u1', undefined, { secret_id: 's1' })).rejects.toThrow(/not allowed for host/);
  });

  it('probe and probe-initialized carry secret_id with the header', async () => {
    let f = mockFetch(200, { reachable: true });
    await api.probeOrigin({ url: 'https://github.com/a/b', secret_id: 's1' });
    expect(call(f)).toMatchObject({ header: 'web', body: { url: 'https://github.com/a/b', secret_id: 's1' } });
    f = mockFetch(200, {});
    await api.probeInitialized({ url: 'https://github.com/a/b', branch: 'main', secret_id: 's1' });
    expect(call(f)).toMatchObject({ header: 'web', body: { secret_id: 's1' } });
    f = mockFetch(200, { reachable: true });
    await api.probeOrigin({ url: 'https://github.com/a/b' });
    expect(call(f).header).toBeNull();
  });

  it('create carries secret_id on the origin with the header', async () => {
    const f = mockFetch(200, { id: 'c1', state: 'done' });
    await api.createRepo({ name: 'kb', mode: 'clone', origin: { url: 'https://github.com/a/b', secret_id: 's1' } }, () => {});
    expect(call(f)).toMatchObject({ url: '/api/v1/repos', header: 'web', body: { origin: { secret_id: 's1' } } });
  });

  it('origin session create carries secret_id with the header', async () => {
    const f = mockFetch(200, { id: 'sess' });
    await createSession('kb', { url: 'https://github.com/a/b', secret_id: 's1' });
    expect(call(f)).toMatchObject({ header: 'web', body: { secret_id: 's1' } });
  });

  it('fleet join, check and follow carry secret_id and no token', async () => {
    let f = mockFetch(200, {});
    await api.joinFleet('B', { secret_id: 's1' });
    expect(call(f).body).toEqual({ bundle: 'B', secret_id: 's1' });
    f = mockFetch(200, {});
    await api.checkFleetJoin('B', { auth_method: 'token', auth_token: 't' });
    expect(call(f).body).toEqual({ bundle: 'B', auth_method: 'token', auth_token: 't' });
    f = mockFetch(200, {});
    await api.followFleetHost('', { secret_id: 's1' });
    expect(call(f)).toMatchObject({ url: '/api/v1/fleet/follow', body: { secret_id: 's1' } });
  });

  it('fleet create carries secret_id on the origin', async () => {
    const f = mockFetch(201, {});
    await api.createFleet({ url: 'https://github.com/a/f', secret_id: 's1' });
    expect(call(f).body).toEqual({ origin: { url: 'https://github.com/a/f', secret_id: 's1' } });
  });
});
