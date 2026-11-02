import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, cleanup } from '@testing-library/react';
import { TargetRail } from './TargetRail';
import type { RepoInfo } from './api';
import { __resetRepoCreatesForTest } from './useRepoCreates';

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>();
  return { ...actual, api: { ...actual.api, listRepoCreates: vi.fn().mockResolvedValue([]) } };
});

const repos: RepoInfo[] = [
  { name: 'old', uid: 'u1', credential_status: 'expired', credential_secret: 'gh-pat' },
  { name: 'soon', uid: 'u2', credential_status: 'expiring', credential_secret: 'gl-pat' },
  { name: 'fine', uid: 'u3' },
];

beforeEach(() => { __resetRepoCreatesForTest(); });
afterEach(() => { cleanup(); vi.clearAllMocks(); });

describe('TargetRail: credential expiry chip', () => {
  it('flags a repo whose linked secret expired or expires soon, titled with the secret, and nothing otherwise', () => {
    render(<TargetRail repos={repos} lenses={[]} context={{ kind: 'repo', repo: 'fine' }} currentRepo="fine"
      remoteError="" serverReadOnly={false} onSelect={vi.fn()} />);
    const expired = screen.getByTestId('rail-credential-old');
    expect(expired).toHaveAttribute('data-status', 'expired');
    expect(expired.getAttribute('title')).toMatch(/gh-pat.*expired/);
    expect(screen.getByTestId('rail-credential-soon')).toHaveAttribute('data-status', 'expiring');
    expect(screen.queryByTestId('rail-credential-fine')).toBeNull();
  });
});
