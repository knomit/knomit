import { describe, it, expect, vi } from 'vitest';
import { bootstrapStatusWithRetry } from './bootstrap';
import type { Status } from './api';

const ok: Status = { head: 'h1', branch: 'agent/test', index_commit: 'i1', embeddings_enabled: true, ontology_root: 'kb' };

describe('bootstrapStatusWithRetry', () => {
  it('resolves on first attempt when both calls succeed', async () => {
    const getAgentBranch = vi.fn().mockResolvedValue('agent/test');
    const getStatus = vi.fn().mockResolvedValue(ok);
    const onSuccess = vi.fn();
    const sleep = vi.fn().mockResolvedValue(undefined);

    await bootstrapStatusWithRetry({
      repo: 'r', initialBranch: '', getAgentBranch, getStatus,
      onSuccess, shouldStop: () => false, sleep,
    });

    expect(getAgentBranch).toHaveBeenCalledTimes(1);
    expect(getStatus).toHaveBeenCalledTimes(1);
    expect(onSuccess).toHaveBeenCalledWith(ok);
    expect(sleep).not.toHaveBeenCalled();
  });

  it('skips getAgentBranch when initialBranch is already known', async () => {
    const getAgentBranch = vi.fn();
    const getStatus = vi.fn().mockResolvedValue(ok);
    const onSuccess = vi.fn();

    await bootstrapStatusWithRetry({
      repo: 'r', initialBranch: 'agent/test', getAgentBranch, getStatus,
      onSuccess, shouldStop: () => false, sleep: vi.fn().mockResolvedValue(undefined),
    });

    expect(getAgentBranch).not.toHaveBeenCalled();
    expect(getStatus).toHaveBeenCalledWith('r', 'agent/test');
  });

  it('retries with exponential backoff until success', async () => {
    const getAgentBranch = vi.fn()
      .mockRejectedValueOnce(new Error('network'))
      .mockRejectedValueOnce(new Error('network'))
      .mockResolvedValue('agent/test');
    const getStatus = vi.fn().mockResolvedValue(ok);
    const onSuccess = vi.fn();
    const sleeps: number[] = [];
    const sleep = vi.fn().mockImplementation((ms: number) => { sleeps.push(ms); return Promise.resolve(); });
    const onAttemptFailed = vi.fn();

    await bootstrapStatusWithRetry({
      repo: 'r', initialBranch: '', getAgentBranch, getStatus,
      onSuccess, shouldStop: () => false, sleep, onAttemptFailed,
      delaysMs: [10, 20, 40, 80],
    });

    expect(getAgentBranch).toHaveBeenCalledTimes(3);
    expect(onAttemptFailed).toHaveBeenCalledTimes(2);
    expect(sleeps).toEqual([10, 20]);
    expect(onSuccess).toHaveBeenCalledWith(ok);
  });

  it('caps backoff at the last delay value', async () => {
    const getAgentBranch = vi.fn()
      .mockRejectedValueOnce(new Error('e'))
      .mockRejectedValueOnce(new Error('e'))
      .mockRejectedValueOnce(new Error('e'))
      .mockRejectedValueOnce(new Error('e'))
      .mockResolvedValue('agent/test');
    const getStatus = vi.fn().mockResolvedValue(ok);
    const sleeps: number[] = [];
    const sleep = vi.fn().mockImplementation((ms: number) => { sleeps.push(ms); return Promise.resolve(); });

    await bootstrapStatusWithRetry({
      repo: 'r', initialBranch: '', getAgentBranch, getStatus,
      onSuccess: vi.fn(), shouldStop: () => false, sleep,
      delaysMs: [10, 20, 50],
    });

    expect(sleeps).toEqual([10, 20, 50, 50]);
  });

  it('stops retrying when shouldStop returns true', async () => {
    const getAgentBranch = vi.fn().mockRejectedValue(new Error('network'));
    const getStatus = vi.fn();
    const onSuccess = vi.fn();
    let calls = 0;
    const sleep = vi.fn().mockResolvedValue(undefined);
    // Stop after 2 failed attempts.
    const shouldStop = () => ++calls >= 4;  // shouldStop is checked at loop top + after each await; tune to stop quickly.

    await bootstrapStatusWithRetry({
      repo: 'r', initialBranch: '', getAgentBranch, getStatus,
      onSuccess, shouldStop, sleep,
    });

    expect(onSuccess).not.toHaveBeenCalled();
    // No assertion on exact retry count — the loop must simply terminate.
  });

  it('aborts before calling onSuccess if shouldStop becomes true mid-flight', async () => {
    const getAgentBranch = vi.fn().mockResolvedValue('agent/test');
    const onSuccess = vi.fn();
    let stopped = false;
    const shouldStop = () => stopped;

    // Flip stopped before getStatus resolves by intercepting its return.
    const slowGetStatus = vi.fn().mockImplementation(async () => {
      stopped = true;
      return ok;
    });

    await bootstrapStatusWithRetry({
      repo: 'r', initialBranch: '', getAgentBranch, getStatus: slowGetStatus,
      onSuccess, shouldStop, sleep: vi.fn().mockResolvedValue(undefined),
    });

    expect(onSuccess).not.toHaveBeenCalled();
  });
});
