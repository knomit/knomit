import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { api } from './api';

// The cancel route's spelling is part of its contract.
//
// It is `POST /api/v1/repo-creates/{id}:cancel` — a colon-suffixed ACTION on
// the job, the same shape as /repos:probe-origin — and not
// `DELETE /repo-creates/{id}`, which is dismiss and means something else
// entirely (it refuses a running job and never touches a repo). Getting the
// method or the suffix wrong produces a 404 or, worse, silently dismisses a
// row instead of cancelling the work, so the URL is asserted literally here
// rather than through a component that mocks this function away.
describe('api.cancelRepoCreate', () => {
  const realFetch = globalThis.fetch;
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 202,
      json: async () => ({ create_id: 'job1', name: 'kb', mode: 'clone', state: 'cancelled' }),
    });
    globalThis.fetch = fetchMock as unknown as typeof fetch;
  });
  afterEach(() => { globalThis.fetch = realFetch; });

  it('POSTs to the job\'s :cancel sub-resource and returns the status body', async () => {
    const status = await api.cancelRepoCreate('job1');

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe('/api/v1/repo-creates/job1:cancel');
    expect(init).toMatchObject({ method: 'POST' });
    // The 202 body IS the job snapshot, which is what lets a caller feed it
    // straight back into whatever already renders a RepoCreateStatus.
    expect(status).toMatchObject({ create_id: 'job1', state: 'cancelled' });
  });

  it('escapes the id but NOT the action colon', async () => {
    await api.cancelRepoCreate('a/b');
    // %2F for the id's slash — otherwise it would address a different path —
    // while the colon stays literal, because it belongs to the route and not
    // to the id.
    expect(fetchMock.mock.calls[0][0]).toBe('/api/v1/repo-creates/a%2Fb:cancel');
  });

  it('throws with the problem detail when the job is already finished (409)', async () => {
    fetchMock.mockResolvedValue({
      ok: false,
      status: 409,
      statusText: 'Conflict',
      json: async () => ({ title: 'Create already finished', detail: 'there is nothing to undo' }),
    });
    await expect(api.cancelRepoCreate('job1')).rejects.toThrow(/409 there is nothing to undo/);
  });

  it('throws for an unknown job (404)', async () => {
    fetchMock.mockResolvedValue({
      ok: false, status: 404, statusText: 'Not Found',
      json: async () => ({ detail: 'no create job with that id' }),
    });
    await expect(api.cancelRepoCreate('nope')).rejects.toThrow(/404/);
  });
});
