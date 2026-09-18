import { describe, it, expect, vi } from 'vitest';
import { bootstrapStatusWithRetry, bootBranchOf } from './bootstrap';
import type { RepoDetails, Status } from './api';

const ok: Status = { head: 'h1', branch: 'agent/test', index_commit: 'i1', embeddings_enabled: true, ontology_root: 'kb' };
const embedded: Status = { head: 'h2', branch: 'agent/test', index_commit: 'i2', embeddings_enabled: true, ontology_root: '' };

const repoWithEmbed: RepoDetails = { name: 'r', read_branch: 'agent/test', agent_branch: 'agent/test', branch: embedded };
const repoNoEmbed: RepoDetails = { name: 'r', read_branch: 'agent/test', agent_branch: 'agent/test' };

const noSleep = () => vi.fn().mockResolvedValue(undefined);

describe('bootstrapStatusWithRetry', () => {
  // The whole point of the embed: one request to a known branch.
  it('uses the embedded branch root and makes exactly one request', async () => {
    const getRepo = vi.fn().mockResolvedValue(repoWithEmbed);
    const getStatus = vi.fn();
    const onSuccess = vi.fn();

    await bootstrapStatusWithRetry({
      repo: 'r', initialBranch: '', getRepo, getStatus,
      onSuccess, shouldStop: () => false, sleep: noSleep(),
    });

    expect(getRepo).toHaveBeenCalledTimes(1);
    expect(getStatus).not.toHaveBeenCalled();
    expect(onSuccess).toHaveBeenCalledWith(embedded);
  });

  // Older server, or a repo whose store is still opening.
  it('falls back to getStatus on the read branch when the embed is absent', async () => {
    const getRepo = vi.fn().mockResolvedValue(repoNoEmbed);
    const getStatus = vi.fn().mockResolvedValue(ok);
    const onSuccess = vi.fn();

    await bootstrapStatusWithRetry({
      repo: 'r', initialBranch: '', getRepo, getStatus,
      onSuccess, shouldStop: () => false, sleep: noSleep(),
    });

    expect(getStatus).toHaveBeenCalledWith('r', 'agent/test');
    expect(onSuccess).toHaveBeenCalledWith(ok);
  });

  it('reports the phase sequence for the one-hop and the fallback paths', async () => {
    const fast: string[] = [];
    await bootstrapStatusWithRetry({
      repo: 'r', initialBranch: '', getRepo: vi.fn().mockResolvedValue(repoWithEmbed),
      getStatus: vi.fn(), onSuccess: vi.fn(), shouldStop: () => false, sleep: noSleep(),
      onPhase: (p) => fast.push(p),
    });
    // No `branch` phase: there was no branch request to wait on.
    expect(fast).toEqual(['opening', 'done']);

    const slow: string[] = [];
    await bootstrapStatusWithRetry({
      repo: 'r', initialBranch: '', getRepo: vi.fn().mockResolvedValue(repoNoEmbed),
      getStatus: vi.fn().mockResolvedValue(ok), onSuccess: vi.fn(),
      shouldStop: () => false, sleep: noSleep(), onPhase: (p) => slow.push(p),
    });
    expect(slow).toEqual(['opening', 'branch', 'done']);
  });

  it('prefers an explicit initialBranch over the repo details', async () => {
    const getRepo = vi.fn().mockResolvedValue(repoNoEmbed);
    const getStatus = vi.fn().mockResolvedValue(ok);

    await bootstrapStatusWithRetry({
      repo: 'r', initialBranch: 'main', getRepo, getStatus,
      onSuccess: vi.fn(), shouldStop: () => false, sleep: noSleep(),
    });

    expect(getStatus).toHaveBeenCalledWith('r', 'main');
  });

  it('retries with exponential backoff until success', async () => {
    const getRepo = vi.fn()
      .mockRejectedValueOnce(new Error('network'))
      .mockRejectedValueOnce(new Error('network'))
      .mockResolvedValue(repoWithEmbed);
    const onSuccess = vi.fn();
    const sleeps: number[] = [];
    const sleep = vi.fn().mockImplementation((ms: number) => { sleeps.push(ms); return Promise.resolve(); });
    const onAttemptFailed = vi.fn();

    await bootstrapStatusWithRetry({
      repo: 'r', initialBranch: '', getRepo, getStatus: vi.fn(),
      onSuccess, shouldStop: () => false, sleep, onAttemptFailed,
      delaysMs: [10, 20, 40, 80],
    });

    expect(getRepo).toHaveBeenCalledTimes(3);
    expect(onAttemptFailed).toHaveBeenCalledTimes(2);
    expect(sleeps).toEqual([10, 20]);
    expect(onSuccess).toHaveBeenCalledWith(embedded);
  });

  it('caps backoff at the last delay value', async () => {
    const getRepo = vi.fn()
      .mockRejectedValueOnce(new Error('e'))
      .mockRejectedValueOnce(new Error('e'))
      .mockRejectedValueOnce(new Error('e'))
      .mockRejectedValueOnce(new Error('e'))
      .mockResolvedValue(repoWithEmbed);
    const sleeps: number[] = [];
    const sleep = vi.fn().mockImplementation((ms: number) => { sleeps.push(ms); return Promise.resolve(); });

    await bootstrapStatusWithRetry({
      repo: 'r', initialBranch: '', getRepo, getStatus: vi.fn(),
      onSuccess: vi.fn(), shouldStop: () => false, sleep,
      delaysMs: [10, 20, 50],
    });

    expect(sleeps).toEqual([10, 20, 50, 50]);
  });

  it('stops retrying when shouldStop returns true', async () => {
    const getRepo = vi.fn().mockRejectedValue(new Error('network'));
    const onSuccess = vi.fn();
    let calls = 0;
    const shouldStop = () => ++calls >= 4;

    await bootstrapStatusWithRetry({
      repo: 'r', initialBranch: '', getRepo, getStatus: vi.fn(),
      onSuccess, shouldStop, sleep: noSleep(),
    });

    expect(onSuccess).not.toHaveBeenCalled();
  });

  it('aborts before calling onSuccess if shouldStop becomes true mid-flight', async () => {
    const onSuccess = vi.fn();
    let stopped = false;
    const getRepo = vi.fn().mockImplementation(async () => { stopped = true; return repoWithEmbed; });

    await bootstrapStatusWithRetry({
      repo: 'r', initialBranch: '', getRepo, getStatus: vi.fn(),
      onSuccess, shouldStop: () => stopped, sleep: noSleep(),
    });

    expect(onSuccess).not.toHaveBeenCalled();
  });
});

describe('bootBranchOf', () => {
  // read_branch is the CONTENT branch and is always present; a subscription
  // has no agent branch at all, so preferring agent_branch would leave us
  // fetching branch "" for exactly the repos that cannot afford it.
  it('prefers read_branch, which a subscription also has', () => {
    expect(bootBranchOf({ name: 'r', read_branch: 'main' })).toBe('main');
    expect(bootBranchOf({ name: 'r', read_branch: 'main', agent_branch: 'agent/test' })).toBe('main');
    expect(bootBranchOf({ name: 'r', agent_branch: 'agent/test' })).toBe('agent/test');
    expect(bootBranchOf({ name: 'r' })).toBe('');
  });
});
