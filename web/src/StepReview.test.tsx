import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
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
    render(<StepReview state={remote('yes', { branches: ['main', 'topic'] })} dispatch={vi.fn()} />);
    expect(screen.getByText(/Branches already there: main, topic\./)).toBeInTheDocument();
  });

  it('says no branches were found only when the probe could look', () => {
    render(<StepReview state={remote('yes', { branches: [] })} dispatch={vi.fn()} />);
    expect(screen.getByText(/No other branches were found on the remote\./)).toBeInTheDocument();
  });

  // A refused probe returns branches: [] because it was REFUSED, not because
  // the remote has none. Reading that as "no other branches were found" states
  // as fact something the probe never established — exactly what design §3's
  // "What the review may claim" forbids, and the same class of mistake as
  // claiming a fact count a ListContext probe cannot know.
  it('never claims the remote has no branches when the probe was refused', () => {
    render(<StepReview state={remote('yes', { auth_required: true, branches: [] })} dispatch={vi.fn()} />);
    expect(screen.queryByText(/No other branches were found/)).not.toBeInTheDocument();
    expect(screen.getByText(/without access to them/)).toBeInTheDocument();
  });

  // The ontology is not a choice on this path, and saying so is what stops a
  // reader wondering where the ontology step went.
  it('says the ontology comes from the remote', () => {
    render(<StepReview state={remote('yes')} dispatch={vi.fn()} />);
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
    render(<StepReview state={remote('no')} dispatch={vi.fn()} />);
    expect(screen.getByText(/main is not changed/)).toBeInTheDocument();
  });

  it('names the merge request as the next step', () => {
    render(<StepReview state={remote('no')} dispatch={vi.fn()} />);
    expect(screen.getByText(/merge request from knomit's branch into main/)).toBeInTheDocument();
  });

  it('names the ontology it will actually write', () => {
    render(<StepReview state={{ ...remote('no'), preset: 'code', seedPreset: 'code' }} dispatch={vi.fn()} />);
    expect(screen.getByText(/the "code" ontology/)).toBeInTheDocument();
  });

  // The one thing this path must NOT claim. knomit writes its own branch and
  // pushes that; a review that described a first commit on main would be
  // describing the behaviour this whole design removed.
  it('never says it writes the first commit to the consensus branch', () => {
    render(<StepReview state={remote('no')} dispatch={vi.fn()} />);
    expect(screen.queryByText(/very first commit/)).not.toBeInTheDocument();
  });
});

describe('StepReview push-access notes', () => {
  // A refusal is a stated risk, not a surprise at 70%.
  it('warns when the access check was refused push access', () => {
    render(<StepReview state={remote('no', { write_access: 'denied' })} dispatch={vi.fn()} />);
    expect(screen.getByTestId('review-write-denied')).toBeInTheDocument();
  });

  // '' is NOT ESTABLISHED, a third state, and must render as neither answer.
  it('says push access was not established when the check never ran', () => {
    render(<StepReview state={remote('no')} dispatch={vi.fn()} />);
    expect(screen.getByTestId('review-write-unknown')).toBeInTheDocument();
  });

  // A subscription NEVER pushes — its own step 2 says so — so a push note
  // under it contradicts the list directly above. This was the shipped
  // behaviour: the write_access guard predates subscribe mode, and the state
  // reducer preselects 'subscribe' on a refused probe, which made the
  // contradiction the DEFAULT rendering of exactly the case the note exists
  // for.
  it('says nothing about pushing when subscribing, because nothing is pushed', () => {
    render(<StepReview state={{ ...remote('yes', { write_access: 'denied' }), access: 'subscribe' }} dispatch={vi.fn()} />);
    expect(screen.queryByTestId('review-write-denied')).not.toBeInTheDocument();
    expect(screen.queryByTestId('review-write-unknown')).not.toBeInTheDocument();
  });

  // Pinned separately, because the test above cannot pin it: a 'denied' probe
  // already makes !write_access false, so its unknown-note assertion would
  // hold with the subscribe guard deleted. The state this one describes is
  // reachable — initialized 'yes', a probe that never established push access,
  // Subscribe chosen — and it is the same contradiction the denied case is:
  // "push access was not established" sitting under a list item that says
  // nothing is ever pushed.
  it('says nothing about an unestablished push either, when subscribing', () => {
    render(<StepReview state={{ ...remote('yes'), access: 'subscribe' }} dispatch={vi.fn()} />);
    expect(screen.queryByTestId('review-write-unknown')).not.toBeInTheDocument();
  });

  it('still warns about a refused push when joining an existing knowledge base', () => {
    render(<StepReview state={{ ...remote('yes', { write_access: 'denied' }), access: 'join' }} dispatch={vi.fn()} />);
    expect(screen.getByTestId('review-write-denied')).toBeInTheDocument();
  });

  // And an 'ok' must not become a promise. The check is a receive-pack
  // advertisement: it establishes that the host will talk to these credentials
  // about pushing, and cannot predict a pre-receive hook, which runs on the
  // content of the push. So there is no green "this will work" card at all.
  it('makes no claim at all when push access looked fine', () => {
    render(<StepReview state={remote('no', { write_access: 'ok' })} dispatch={vi.fn()} />);
    expect(screen.queryByTestId('review-write-denied')).not.toBeInTheDocument();
    expect(screen.queryByTestId('review-write-unknown')).not.toBeInTheDocument();
  });
});

// Join vs Subscribe is the one create decision the wizard does NOT derive, and
// this is the first step that knows the branch is a knowledge base (the access
// step runs before the branch check), so it is where the choice can be offered
// truthfully.
describe('StepReview — join or subscribe', () => {
  it('offers both ways to attach, and dispatches the choice', () => {
    const dispatch = vi.fn();
    render(<StepReview state={remote('yes')} dispatch={dispatch} />);

    expect(screen.getByTestId('access-join')).toBeInTheDocument();
    const sub = screen.getByTestId('access-subscribe');
    expect(sub).toBeInTheDocument();

    fireEvent.click(sub);
    expect(dispatch).toHaveBeenCalledWith({ type: 'SET_ACCESS', access: 'subscribe' });
  });

  // The same control StepSource uses for its own binary, so the wizard asks
  // its two questions the same way; aria-checked is how that control says
  // which side is on, and the two options are radios in one radiogroup rather
  // than independent toggles (SegmentedChoice.test.tsx pins the semantics
  // themselves).
  it('shows which way is chosen', () => {
    render(<StepReview state={{ ...remote('yes'), access: 'subscribe' }} dispatch={vi.fn()} />);
    expect(screen.getByTestId('access-subscribe')).toHaveAttribute('aria-checked', 'true');
    expect(screen.getByTestId('access-join')).toHaveAttribute('aria-checked', 'false');
  });

  // Asserted against the step's own list, not the whole pane's text: the
  // Subscribe segment's subtitle now says "read-only" too, so scanning
  // textContent would pass on the label alone and stop noticing if the list
  // that explains the consequences went missing.
  it('describes a subscription as read-only once it is chosen', () => {
    render(<StepReview state={{ ...remote('yes'), access: 'subscribe' }} dispatch={vi.fn()} />);
    expect(screen.getByText(/It is read-only: no facts can be written here, and nothing is ever pushed\./)).toBeInTheDocument();
  });

  // Not a knowledge base yet — there is nothing to follow, so the choice does
  // not exist here and offering it would imply a mode the backend refuses.
  it('does not offer the choice for a branch that is not a knowledge base', () => {
    render(<StepReview state={remote('no')} dispatch={vi.fn()} />);
    expect(screen.queryByTestId('access-join')).not.toBeInTheDocument();
    expect(screen.queryByTestId('access-subscribe')).not.toBeInTheDocument();
  });
});
