import { describe, it, expect, vi } from 'vitest';
import { bootstrapStatusWithRetry } from './bootstrap';
import type { Status } from './api';

const ok: Status = { head: 'h1', branch: 'agent/test', index_commit: 'i1', embeddings_enabled: true, ontology_root: 'kb' };

// Regression: without onAttemptFailed being invoked per failure, a permanently
// broken backend would leave the UI hung on "Loading…" with no console signal.
// This test pins that the callback fires for each failed attempt and carries
// the underlying error.
describe('bootstrapStatusWithRetry — onAttemptFailed surface', () => {
  it('invokes onAttemptFailed with the error on every failed attempt', async () => {
    const err1 = new Error('boom 1');
    const err2 = new Error('boom 2');
    const getAgentBranch = vi.fn()
      .mockRejectedValueOnce(err1)
      .mockRejectedValueOnce(err2)
      .mockResolvedValue('agent/test');
    const getStatus = vi.fn().mockResolvedValue(ok);
    const sleep = vi.fn().mockResolvedValue(undefined);
    const onAttemptFailed = vi.fn();

    await bootstrapStatusWithRetry({
      repo: 'r', initialBranch: '', getAgentBranch, getStatus,
      onSuccess: vi.fn(), shouldStop: () => false, sleep, onAttemptFailed,
      delaysMs: [1, 2, 4],
    });

    expect(onAttemptFailed).toHaveBeenCalledTimes(2);
    expect(onAttemptFailed).toHaveBeenNthCalledWith(1, err1, 0);
    expect(onAttemptFailed).toHaveBeenNthCalledWith(2, err2, 1);
  });
});
