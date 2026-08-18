import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import { StepReview } from './StepReview';
import { initialWizardState, type WizardState } from './wizardState';
import type { ProbeResult } from './api';

// `initialized` is what decides which of the two remote cases renders, so it is
// a required argument here rather than something a caller can forget: a state
// with it left at '' renders neither, which would make an assertion pass or
// fail for reasons that have nothing to do with what it is testing.
const remote = (initialized: 'yes' | 'no', probe: Partial<ProbeResult> = {}): WizardState => ({
  ...initialWizardState, choice: 'remote', url: 'https://h/r.git', name: 'kb',
  branch: 'main', initialized, stepIndex: 3,
  probe: { reachable: true, empty: false, auth_required: false, upstream_branch: 'main', branches: [], ...probe },
});

describe('StepReview joining an existing knowledge base', () => {
  it('lists the branches the probe actually saw', () => {
    render(<StepReview state={remote('yes', { branches: ['main', 'topic'] })} />);
    expect(screen.getByText(/Branches already there: main, topic\./)).toBeInTheDocument();
  });

  it('says no branches were found only when the probe could look', () => {
    render(<StepReview state={remote('yes', { branches: [] })} />);
    expect(screen.getByText(/No other branches were found on the remote\./)).toBeInTheDocument();
  });

  // A refused probe returns branches: [] because it was REFUSED, not because
  // the remote has none. Reading that as "no other branches were found" states
  // as fact something the probe never established — exactly what design §3's
  // "What the review may claim" forbids, and the same class of mistake as
  // claiming a fact count a ListContext probe cannot know.
  it('never claims the remote has no branches when the probe was refused', () => {
    render(<StepReview state={remote('yes', { auth_required: true, branches: [] })} />);
    expect(screen.queryByText(/No other branches were found/)).not.toBeInTheDocument();
    expect(screen.getByText(/without access to them/)).toBeInTheDocument();
  });

  // The ontology is not a choice on this path, and saying so is what stops a
  // reader wondering where the ontology step went.
  it('says the ontology comes from the remote', () => {
    render(<StepReview state={remote('yes')} />);
    expect(screen.getByText(/ontology comes from the remote itself/)).toBeInTheDocument();
  });
});

// THE TWO STATEMENTS this case is required to make.
//
// Both correct an expectation the old flow created. The deleted "seed" mode
// pushed the CONSENSUS branch — which is exactly why it failed on hosts that
// protect a new project's default branch — so a reader carrying that model
// needs to be told plainly that it no longer happens. And the merge request is
// the only part of this the reader must do themselves; nothing else in the
// product will bring it up.
describe('StepReview initializing a remote that is not a knowledge base yet', () => {
  it('states that the consensus branch is not changed', () => {
    render(<StepReview state={remote('no')} />);
    expect(screen.getByText(/main is not changed/)).toBeInTheDocument();
  });

  it('names the merge request as the next step', () => {
    render(<StepReview state={remote('no')} />);
    expect(screen.getByText(/merge request from knomit's branch into main/)).toBeInTheDocument();
  });

  it('names the ontology it will actually write', () => {
    render(<StepReview state={{ ...remote('no'), preset: 'code', seedPreset: 'code' }} />);
    expect(screen.getByText(/the "code" ontology/)).toBeInTheDocument();
  });

  // The one thing this path must NOT claim. knomit writes its own branch and
  // pushes that; a review that described a first commit on main would be
  // describing the behaviour this whole design removed.
  it('never says it writes the first commit to the consensus branch', () => {
    render(<StepReview state={remote('no')} />);
    expect(screen.queryByText(/very first commit/)).not.toBeInTheDocument();
  });
});

describe('StepReview push-access notes', () => {
  // A refusal is a stated risk, not a surprise at 70%.
  it('warns when the access check was refused push access', () => {
    render(<StepReview state={remote('no', { write_access: 'denied' })} />);
    expect(screen.getByTestId('review-write-denied')).toBeInTheDocument();
  });

  // '' is NOT ESTABLISHED, a third state, and must render as neither answer.
  it('says push access was not established when the check never ran', () => {
    render(<StepReview state={remote('no')} />);
    expect(screen.getByTestId('review-write-unknown')).toBeInTheDocument();
  });

  // And an 'ok' must not become a promise. The check is a receive-pack
  // advertisement: it establishes that the host will talk to these credentials
  // about pushing, and cannot predict a pre-receive hook, which runs on the
  // content of the push. So there is no green "this will work" card at all.
  it('makes no claim at all when push access looked fine', () => {
    render(<StepReview state={remote('no', { write_access: 'ok' })} />);
    expect(screen.queryByTestId('review-write-denied')).not.toBeInTheDocument();
    expect(screen.queryByTestId('review-write-unknown')).not.toBeInTheDocument();
  });
});
