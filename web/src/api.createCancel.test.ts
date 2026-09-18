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

// THE POLL LOOP MUST RIDE 'cancelling' THROUGH TO A TERMINAL STATE.
//
// This is the defect that reached a real browser. createRepo's loop tested
// `state === 'running'`, which is not the same question as "is it over":
// 'cancelling' is non-terminal and not 'running', so a cancel accepted
// mid-transfer ended the loop on the 202 and resolved the create with a job
// that was still working. The wizard read that as an outcome and navigated the
// user to the settings page of a repository that did not exist — while the
// server went on fetching for another 48 seconds.
//
// It is tested HERE, against the real fetch, because that is where the bug
// lived. A test that mocks api.createRepo cannot see this: it replaces the
// very loop under test, which is exactly why the earlier wizard tests were
// green through the whole of it.
describe('api.createRepo polling across a cancel', () => {
  const realFetch = globalThis.fetch;
  afterEach(() => { globalThis.fetch = realFetch; });

  it('keeps polling while the job is cancelling and resolves only when terminal', async () => {
    const polled: string[] = [];
    // The server's answers, in order: the 202, then two 'cancelling' polls
    // (the non-interruptible fetch still running), then the outcome.
    const sequence = [
      { state: 'cancelling', step: 'subscribe' },
      { state: 'cancelling', step: 'subscribe' },
      { state: 'cancelled', step: 'subscribe' },
    ];
    globalThis.fetch = vi.fn(async (url: string, init?: { method?: string }) => {
      if (init?.method === 'POST') {
        return { ok: true, status: 202, json: async () => ({ create_id: 'job1', name: 'kb', mode: 'subscribe', state: 'running', step: 'subscribe' }) };
      }
      polled.push(String(url));
      return { ok: true, status: 200, json: async () => ({ create_id: 'job1', name: 'kb', mode: 'subscribe', ...sequence.shift() }) };
    }) as unknown as typeof fetch;

    const seen: string[] = [];
    const final = await api.createRepo(
      { name: 'kb', mode: 'subscribe' } as Parameters<typeof api.createRepo>[0],
      s => seen.push(s.state));

    // THE ASSERTION: it did not stop at 'cancelling'.
    expect(final.state).toBe('cancelled');
    expect(seen).toEqual(['running', 'cancelling', 'cancelling', 'cancelled']);
    // Three polls, not one — the loop ran on past the two cancelling answers.
    expect(polled).toHaveLength(3);
    expect(polled[0]).toBe('/api/v1/repo-creates/job1');
  });
});
