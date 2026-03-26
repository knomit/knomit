import { describe, it, expect, vi, beforeEach } from 'vitest';
import type { Dispatch } from 'react';
import type { Action } from './state';
import { resolveNavRequest } from './useNavigationManager';
import type { NavRequest } from './useNavigationManager';
import { api } from './api';

vi.mock('./api', () => ({
  api: {
    commitDetail: vi.fn(),
  },
}));

const mockCommitDetail = api.commitDetail as ReturnType<typeof vi.fn>;

describe('resolveNavRequest', () => {
  let dispatch: Dispatch<Action>;

  beforeEach(() => {
    dispatch = vi.fn() as unknown as Dispatch<Action>;
    vi.clearAllMocks();
  });

  it('fully-specified history request dispatches synchronously without API call', async () => {
    const req: NavRequest = {
      view: 'history',
      historyCommit: 'abc123',
      factPath: 'kb/foo.md',
      factCommit: 'abc123',
    };
    await resolveNavRequest(req, 'myrepo', dispatch);
    expect(mockCommitDetail).not.toHaveBeenCalled();
    expect(dispatch).toHaveBeenCalledOnce();
    expect(dispatch).toHaveBeenCalledWith({
      type: 'APPLY_NAV',
      view: 'history',
      historyCommit: 'abc123',
      factPath: 'kb/foo.md',
      factCommit: 'abc123',
    });
  });

  it('fully-specified history request without factCommit defaults factCommit to historyCommit', async () => {
    const req: NavRequest = { view: 'history', historyCommit: 'abc123', factPath: 'kb/foo.md' }; // no factCommit
    await resolveNavRequest(req, 'myrepo', dispatch);
    expect(mockCommitDetail).not.toHaveBeenCalled();
    expect(dispatch).toHaveBeenCalledWith({
      type: 'APPLY_NAV',
      view: 'history',
      historyCommit: 'abc123',
      factPath: 'kb/foo.md',
      factCommit: 'abc123', // defaults to historyCommit
    });
  });

  it('history request with factPath null fetches commitDetail and dispatches first file', async () => {
    mockCommitDetail.mockResolvedValue({
      commit: 'abc123', date: '', message: '',
      files: [{ path: 'kb/first.md', action: 'modified' }, { path: 'kb/second.md', action: 'added' }],
    });
    const req: NavRequest = { view: 'history', historyCommit: 'abc123', factPath: null };
    await resolveNavRequest(req, 'myrepo', dispatch);
    expect(mockCommitDetail).toHaveBeenCalledWith('myrepo', 'abc123');
    expect(dispatch).toHaveBeenCalledWith({
      type: 'APPLY_NAV',
      view: 'history',
      historyCommit: 'abc123',
      factPath: 'kb/first.md',
      factCommit: 'abc123',
    });
  });

  it('history request with factPath null and empty files dispatches with factPath null', async () => {
    mockCommitDetail.mockResolvedValue({ commit: 'abc123', date: '', message: '', files: [] });
    const req: NavRequest = { view: 'history', historyCommit: 'abc123', factPath: null };
    await resolveNavRequest(req, 'myrepo', dispatch);
    expect(dispatch).toHaveBeenCalledWith({
      type: 'APPLY_NAV',
      view: 'history',
      historyCommit: 'abc123',
      factPath: null,
      factCommit: 'abc123',
    });
  });

  it('history request with factPath null: api failure dispatches APPLY_NAV with factPath null', async () => {
    mockCommitDetail.mockRejectedValue(new Error('network error'));
    const req: NavRequest = { view: 'history', historyCommit: 'abc123', factPath: null };
    await resolveNavRequest(req, 'myrepo', dispatch);
    expect(dispatch).toHaveBeenCalledWith({
      type: 'APPLY_NAV',
      view: 'history',
      historyCommit: 'abc123',
      factPath: null,
      factCommit: 'abc123',
    });
  });

  it('tree request dispatches with historyCommit null and factCommit null', async () => {
    const req: NavRequest = { view: 'tree', factPath: 'kb/foo.md' };
    await resolveNavRequest(req, 'myrepo', dispatch);
    expect(dispatch).toHaveBeenCalledWith({
      type: 'APPLY_NAV',
      view: 'tree',
      historyCommit: null,
      factPath: 'kb/foo.md',
      factCommit: null,
    });
  });

  it('chrono request with null factPath dispatches with factPath null', async () => {
    const req: NavRequest = { view: 'chrono', factPath: null };
    await resolveNavRequest(req, 'myrepo', dispatch);
    expect(dispatch).toHaveBeenCalledWith({
      type: 'APPLY_NAV',
      view: 'chrono',
      historyCommit: null,
      factPath: null,
      factCommit: null,
    });
  });
});
