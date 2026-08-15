import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import { StepReview } from './StepReview';
import { initialWizardState, type WizardState } from './wizardState';
import type { ProbeResult } from './api';

const remote = (probe: Partial<ProbeResult>): WizardState => ({
  ...initialWizardState, choice: 'remote', url: 'https://h/r.git', name: 'kb', stepIndex: 2,
  probe: { reachable: true, empty: false, auth_required: false, upstream_branch: 'main', branches: [], ...probe },
});

describe('StepReview', () => {
  it('lists the branches the probe actually saw', () => {
    render(<StepReview state={remote({ branches: ['main', 'topic'] })} />);
    expect(screen.getByText(/Branches already there: main, topic\./)).toBeInTheDocument();
  });

  it('says no branches were found only when the probe could look', () => {
    render(<StepReview state={remote({ branches: [] })} />);
    expect(screen.getByText(/No other branches were found on the remote\./)).toBeInTheDocument();
  });

  // A refused probe returns branches: [] because it was REFUSED, not because
  // the remote has none. Reading that as "no other branches were found" states
  // as fact something the probe never established — exactly what design §3's
  // "What the review may claim" forbids, and the same class of mistake as
  // claiming a fact count a ListContext probe cannot know.
  it('never claims the remote has no branches when the probe was refused', () => {
    render(<StepReview state={remote({ auth_required: true, branches: [] })} />);
    expect(screen.queryByText(/No other branches were found/)).not.toBeInTheDocument();
    expect(screen.getByText(/without access to them/)).toBeInTheDocument();
  });
});
