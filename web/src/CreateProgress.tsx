import type { RepoCreateStatus } from './api';

// CREATE_STEPS is the step list each create mode runs, in order, mirroring the
// emit() calls in internal/repos/lifecycle.go. It exists because polling
// reports a LATEST VALUE, not a stream: a local create finishes in
// milliseconds and is typically polled once, so a component that rendered only
// what it observed would flash "started → done" and show none of the work.
//
// Rendering the known list with the reported step as a CURSOR keeps the step
// log on every mode, and is honest about what it is — these are the steps the
// create WILL run, marked done/current/pending, not a claim that each one was
// individually witnessed.
//
// It is a mirror, so it can drift. Drift is visible rather than silent: an
// unknown reported step still renders (see cursor below), it simply does not
// match a row — a step missing here shows as an unmatched current step, not as
// a hang.
const CREATE_STEPS: Record<string, string[]> = {
  preset: ['validate', 'ontology', 'init-git', 'register', 'done'],
  custom: ['validate', 'ontology', 'init-git', 'register', 'done'],
  clone: ['validate', 'clone', 'persist-origin', 'register', 'sync', 'done'],
  initialize: ['validate', 'probe', 'ontology', 'clone', 'ontology-write', 'push', 'persist-origin', 'register', 'sync', 'done'],
};

const LABELS: Record<string, string> = {
  validate: 'Validating request',
  probe: 'Checking the remote',
  ontology: 'Resolving ontology',
  clone: 'Reading the remote',
  'init-git': 'Initialising git store',
  'ontology-write': 'Writing ontology',
  push: 'Pushing agent branch',
  'persist-origin': 'Saving remote config',
  register: 'Registering repo',
  sync: 'Activating sync',
  done: 'Repo ready',
};

// CreateProgress renders a create's progress — the same rendering
// CreateRepoForm did inline, pulled out so the wizard shell can drop it under
// whichever step is showing (only ever 'review', but kept presentational per
// the step-component convention).
export function CreateProgress({ status }: { status: RepoCreateStatus | null }) {
  if (!status) return null;

  if (status.state === 'failed') {
    return (
      <div data-testid="create-progress" style={box}>
        <div style={{ color: '#f88' }}>{status.error || 'create failed'}</div>
      </div>
    );
  }

  const steps = CREATE_STEPS[status.mode] ?? CREATE_STEPS.preset;
  // -1 for a step this list does not know: everything then reads as pending
  // rather than as spuriously complete, which is the safe direction to drift.
  const cursor = status.step ? steps.indexOf(status.step) : -1;

  return (
    <div data-testid="create-progress" style={box}>
      <div style={{ color: '#9c9', marginBottom: 6 }}>
        {status.pct ?? 0}% {status.message || ''}
      </div>
      {steps.map((s, i) => {
        const done = cursor > i || status.state === 'done';
        const current = cursor === i && status.state !== 'done';
        return (
          <div key={s} data-testid={`create-step-${s}`}
            style={{ color: done ? '#9c9' : current ? '#eee' : '#666' }}>
            {done ? '\u2713' : current ? '\u2026' : '\u00b7'} {LABELS[s] ?? s}
          </div>
        );
      })}
    </div>
  );
}

const box: React.CSSProperties = { marginTop: 12, padding: 10, background: '#0c0c0c', borderRadius: 4, fontSize: 12, fontFamily: 'var(--k-font-mono)', maxHeight: 220, overflow: 'auto' };
