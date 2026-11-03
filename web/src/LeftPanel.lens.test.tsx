// Task 17: per-fact history anchored on the open fact's source mount. LeftPanel
// feeds TimelineNav the temporal anchor. In a repo context that stays
// {state.repo, state.branch} + the bare path (byte-identical to before). In a
// lens context it must anchor on the OPEN FACT's source mount and pass the
// RELATIVE path (the kb://<id12>/ qualifier stripped) so the mount's repo-scoped
// history endpoints resolve.
import { it, expect, vi, beforeEach } from 'vitest';
import { render } from '@testing-library/react';
import { LeftPanel } from './LeftPanel';
import { init } from './state';
import type { AppState, AsOf } from './state';
import type { Lens, LensSource } from './api';

// Capture the props TimelineNav receives so we can assert the anchor + path.
const timelineProps: any[] = [];
vi.mock('./Library', () => ({ Library: () => <div>LIBRARY</div> }));
vi.mock('./TimelineNav', () => ({
  TimelineNav: (props: any) => { timelineProps.push(props); return <div>TIMELINE</div>; },
}));

const lens: Lens = { name: 'eng', write: { uid: 'uid-core', name: 'core' }, reads: [{ uid: 'uid-core', name: 'core' }, { uid: 'uid-docs', name: 'docs' }] };
const readSource: LensSource = { repo: 'docs', id: 'docsid123456', branch: 'main' };
const writeSource: LensSource = { repo: 'core', id: 'coreid123456', branch: 'agent/main' };

function lensState(factPath: string, factSource: LensSource, asOf: AsOf): AppState {
  return {
    ...init, repo: 'core', branch: 'agent/main',
    context: { kind: 'lens', name: 'eng' }, lens, factPath, factSource, asOf,
  };
}

const props = () => ({ dispatch: vi.fn(), navigate: vi.fn(), onScrub: vi.fn(), onOpenFileAt: vi.fn(), onReturnToLive: vi.fn() });

beforeEach(() => { timelineProps.length = 0; });

it('lens read-mount fact: anchors TimelineNav on the mount + relative path (kb://<id>/ stripped)', () => {
  render(<LeftPanel state={lensState('kb://docsid123456/kb/api/auth.md', readSource, { mode: 'history', commit: 'c1' })} {...props()} />);
  const p = timelineProps.at(-1);
  expect(p.repo).toBe('docs');
  expect(p.branch).toBe('main');
  expect(p.factPath).toBe('kb/api/auth.md');
});

it('lens write-repo fact: anchors on the write mount, bare path passes through', () => {
  render(<LeftPanel state={lensState('kb/ops/rollback.md', writeSource, { mode: 'history', commit: 'c1' })} {...props()} />);
  const p = timelineProps.at(-1);
  expect(p.repo).toBe('core');
  expect(p.branch).toBe('agent/main');
  expect(p.factPath).toBe('kb/ops/rollback.md');
});

it('repo context: still {state.repo, state.branch} + the bare path (byte-identical)', () => {
  const s = { ...init, repo: 'r', branch: 'b', factPath: 'kb/a.md', asOf: { mode: 'history', commit: 'c1' } as AsOf } as AppState;
  render(<LeftPanel state={s} {...props()} />);
  const p = timelineProps.at(-1);
  expect(p).toMatchObject({ repo: 'r', branch: 'b', factPath: 'kb/a.md' });
});

// The fact can move to another mount while the timeline is open (the mounts
// picker re-selects the first row and keeps asOf). Until its source resolves,
// factSource is the PREVIOUS fact's mount: no timeline may be anchored on it.
it('lens fact whose source is still the previous mount: no TimelineNav until it resolves', () => {
  const { rerender } = render(<LeftPanel state={lensState('kb://docsid123456/kb/api/auth.md', writeSource, { mode: 'history', commit: 'c1' })} {...props()} />);
  expect(timelineProps).toHaveLength(0);
  rerender(<LeftPanel state={lensState('kb://docsid123456/kb/api/auth.md', readSource, { mode: 'history', commit: 'c1' })} {...props()} />);
  expect(timelineProps.at(-1)).toMatchObject({ repo: 'docs', branch: 'main', factPath: 'kb/api/auth.md' });
});
