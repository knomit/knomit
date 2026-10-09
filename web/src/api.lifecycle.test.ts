import { describe, it, expect, vi, afterEach } from 'vitest';
import { api, streamCommit, readSSEStream, RepoIndexingError, statusFromBranchBody } from './api';

// The HTTP surface of the per-repo lifecycle machine (knomit#411): rebuilds
// that are absorbed rather than refused, the index cancel, and the 409 "Repo
// is indexing" refusal that origin attach/detach/commit answer while the
// repo's index job runs.

const realFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = realFetch; });

const respond = (status: number, body: unknown): Response => ({
  ok: status >= 200 && status < 300,
  status,
  statusText: '',
  json: async () => body,
} as unknown as Response);

const INDEXING_PROBLEM = {
  type: 'about:blank', title: 'Repo is indexing', status: 409,
  detail: 'wait for it to finish or cancel indexing',
  _links: { 'cancel-index': { href: '/api/v1/repos/kb/index:cancel', method: 'POST' } },
};

describe('api.rebuild', () => {
  // 201 is a new job; 200 is the SAME envelope for an identical rebuild that
  // was already running and absorbed this one. Both are success.
  it.each([201, 200])('resolves with the job envelope on %i', async status => {
    const job = { id: 'j1', kind: 'index-rebuild', state: 'running' };
    globalThis.fetch = vi.fn().mockResolvedValue(respond(status, job));
    await expect(api.rebuild('kb', 'agent/x')).resolves.toEqual(job);
    expect(globalThis.fetch).toHaveBeenCalledWith(
      expect.stringMatching(/\/api\/v1\/repos\/kb\/branches\/agent:x\/index-rebuilds$/), { method: 'POST' });
  });
});

describe('api.cancelIndex', () => {
  it('POSTs the cancel-index link and resolves with the repo row', async () => {
    const row = { name: 'kb', uid: 'u1', state: 'active', stage: 'ready', index_state: 'error', index_reason: 'indexing cancelled' };
    globalThis.fetch = vi.fn().mockResolvedValue(respond(200, row));
    await expect(api.cancelIndex('/api/v1/repos/kb/index:cancel')).resolves.toEqual(row);
    expect(globalThis.fetch).toHaveBeenCalledWith(
      expect.stringMatching(/\/api\/v1\/repos\/kb\/index:cancel$/), { method: 'POST' });
  });

  it('rejects when the repo is not open (503)', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue(respond(503, { detail: 'repo is not open' }));
    await expect(api.cancelIndex('/api/v1/repos/kb/index:cancel')).rejects.toThrow(/503 repo is not open/);
  });
});

describe('the 409 "Repo is indexing" refusal', () => {
  it('deleteOrigin throws RepoIndexingError carrying the detail and the cancel link', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue(respond(409, INDEXING_PROBLEM));
    const err = await api.deleteOrigin('kb').catch(e => e);
    expect(err).toBeInstanceOf(RepoIndexingError);
    expect(err.message).toBe('wait for it to finish or cancel indexing');
    expect(err.cancelHref).toBe('/api/v1/repos/kb/index:cancel');
  });

  // Keyed on the link: a 409 without one is an ordinary failure.
  it('deleteOrigin keeps its plain error for any other refusal', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue(respond(409, { title: 'Conflict', detail: 'subscription' }));
    const err = await api.deleteOrigin('kb').catch(e => e);
    expect(err).not.toBeInstanceOf(RepoIndexingError);
    expect(String(err)).toMatch(/disconnect → 409/);
  });

  it('readSSEStream throws RepoIndexingError for it before any stream opens', async () => {
    await expect(readSSEStream(respond(409, INDEXING_PROBLEM))).rejects.toBeInstanceOf(RepoIndexingError);
  });

  it('readSSEStream throws other pre-stream problems with their detail', async () => {
    const err = await readSSEStream(respond(502, { title: 'Origin not attached', detail: 'origin was not attached: timeout' })).catch(e => e);
    expect(err).not.toBeInstanceOf(RepoIndexingError);
    expect(err.message).toBe('origin was not attached: timeout');
  });
});

describe('streamCommit', () => {
  const emptyStream = () => ({
    ok: true, status: 200,
    body: { getReader: () => ({ read: async () => ({ done: true, value: undefined }) }) },
  } as unknown as Response);

  it('POSTs with no body by default', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue(emptyStream());
    await streamCommit('kb', 's1', () => {});
    expect(globalThis.fetch).toHaveBeenCalledWith(expect.stringMatching(/\/origin-sessions\/s1\/commit$/), { method: 'POST' });
  });

  it('sends {"cancel_indexing": true} as JSON when asked to cancel indexing', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue(emptyStream());
    await streamCommit('kb', 's1', () => {}, { cancelIndexing: true });
    expect(globalThis.fetch).toHaveBeenCalledWith(expect.stringMatching(/\/origin-sessions\/s1\/commit$/), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ cancel_indexing: true }),
    });
  });
});

describe('index_reason', () => {
  it('rides from the branch root into Status', () => {
    expect(statusFromBranchBody({ index_state: 'error', index_reason: 'indexing cancelled' }, 'main').index_reason)
      .toBe('indexing cancelled');
  });
});

// Archive: no note → a bare DELETE (what every server before reasons
// accepts); a note → a JSON body {note}, trimmed. Whitespace alone is no note.
describe('api.archiveRepo', () => {
  const row = { id: 'u1', name: 'kb', origin: '', archivedAt: '2026-10-09T00:00:00Z', reason: null };
  it('sends a bare DELETE without a note', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue(respond(200, row));
    await api.archiveRepo('kb', '   ');
    expect(globalThis.fetch).toHaveBeenCalledWith(expect.stringMatching(/\/api\/v1\/repos\/kb$/), { method: 'DELETE' });
  });
  it('sends the trimmed note as JSON', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue(respond(200, row));
    await api.archiveRepo('kb', '  gone for good ');
    expect(globalThis.fetch).toHaveBeenCalledWith(expect.stringMatching(/\/api\/v1\/repos\/kb$/), {
      method: 'DELETE',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ note: 'gone for good' }),
    });
  });
});
