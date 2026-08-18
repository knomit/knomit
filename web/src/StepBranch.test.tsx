import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { StepBranch } from './StepBranch';
import { initialWizardState, type WizardState } from './wizardState';
import type { ProbeResult } from './api';

const state = (over: Partial<WizardState> = {}, probe: Partial<ProbeResult> = {}): WizardState => ({
  ...initialWizardState, choice: 'remote', url: 'https://github.com/org/kb.git', name: 'kb',
  branch: 'main',
  probe: { reachable: true, empty: false, auth_required: false, upstream_branch: 'main', branches: ['main'], ...probe },
  ...over,
});

// The step renders the QUESTION and the answer cards; the action that runs the
// check lives in the wizard footer with every other step's forward action, so
// the tests for it live in CreateRepoWizard.test.tsx alongside the rest of the
// footer's behaviour.
function renderStep(s: WizardState) {
  const dispatch = vi.fn();
  render(<StepBranch state={s} dispatch={dispatch} />);
  return { dispatch };
}

describe('StepBranch choosing the consensus branch', () => {
  it('offers every branch the probe saw', () => {
    renderStep(state({}, { branches: ['main', 'develop', 'topic'] }));
    expect(screen.getByTestId('branch-option-main')).toBeInTheDocument();
    expect(screen.getByTestId('branch-option-develop')).toBeInTheDocument();
    expect(screen.getByTestId('branch-option-topic')).toBeInTheDocument();
  });

  it('marks the tracked branch as selected', () => {
    renderStep(state({ branch: 'develop' }, { branches: ['main', 'develop'] }));
    expect(screen.getByTestId('branch-option-develop')).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByTestId('branch-option-main')).toHaveAttribute('aria-pressed', 'false');
  });

  it('dispatches the choice', () => {
    const { dispatch } = renderStep(state({}, { branches: ['main', 'develop'] }));
    fireEvent.click(screen.getByTestId('branch-option-develop'));
    expect(dispatch).toHaveBeenCalledWith({ type: 'SET_BRANCH', branch: 'develop' });
  });

  // An auth_required probe returns branches: [] because it was REFUSED, not
  // because the remote has none — so the step must not present an empty chip
  // list as though it were the complete set. A free-text field is the honest
  // fallback, and leaves the user a way forward.
  it('falls back to a text field rather than claiming there are no branches', () => {
    renderStep(state({}, { auth_required: true, branches: [] }));
    expect(screen.getByTestId('branch-input')).toBeInTheDocument();
  });

  // knomit writes to its own branch, cut from this one. Saying so here is what
  // stops a reader assuming they need push access to whatever they pick.
  it('says knomit never writes to the branch being chosen', () => {
    renderStep(state());
    expect(screen.getByText(/never writes to it/i)).toBeInTheDocument();
  });
});

describe('StepBranch reporting the check', () => {
  it('says the branch already holds a knowledge base, and that there is nothing to choose', () => {
    renderStep(state({ initialized: 'yes' }));
    const card = screen.getByTestId('branch-initialized');
    expect(card).toHaveAttribute('data-tone', 'good');
    expect(card).toHaveTextContent(/ontology comes from the remote/i);
  });

  it('says an uninitialized branch is not changed, only cut from', () => {
    renderStep(state({ initialized: 'no' }));
    const card = screen.getByTestId('branch-uninitialized');
    expect(card).toHaveAttribute('data-tone', 'good');
    expect(card).toHaveTextContent(/main itself isn't changed/i);
  });

  // THE THIRD STATE. It is rendered as itself — "couldn't tell" — never as an
  // answer, and it explains WHY knomit refuses to guess: the ontology is fixed
  // at create time, so a wrong guess either throws away the ontology the reader
  // is about to pick or writes over the one that already governs their
  // knowledge base. Neither is recoverable.
  it('reports an unestablished check as unestablished, with the reason', () => {
    renderStep(state({ initialized: '', initializedDetail: 'repository not found' }));
    const card = screen.getByTestId('branch-blocked');
    expect(card).toHaveAttribute('data-tone', 'bad');
    expect(card).toHaveTextContent(/couldn't tell/i);
    expect(card).toHaveTextContent(/repository not found/);
    // It must not resolve into either answer.
    expect(screen.queryByTestId('branch-initialized')).not.toBeInTheDocument();
    expect(screen.queryByTestId('branch-uninitialized')).not.toBeInTheDocument();
  });

  // '' is ALSO the state before anything has been pressed, and that is not a
  // failure. Rendering the block on arrival would put a red card in front of a
  // reader who has done nothing — which is why `initializedDetail`, not
  // `initialized`, is what distinguishes "asked and could not tell" from "not
  // asked yet".
  it('shows no failure before the check has run', () => {
    renderStep(state());
    expect(screen.queryByTestId('branch-blocked')).not.toBeInTheDocument();
  });

  // The choice is framed as one: a caption says how many branches there are,
  // and the chosen chip is MARKED rather than only tinted. With a single branch
  // — the common case for a repository made to hold a knowledge base — a lone
  // untinted chip read as a text field the reader was meant to fill in.
  it('says how much of a choice there is, and marks the one in force', () => {
    renderStep(state());
    expect(screen.getByTestId('step-branch')).toHaveTextContent(/the only branch on github\.com/i);
    expect(screen.getByTestId('branch-option-main')).toHaveAttribute('aria-pressed', 'true');
  });

  it('counts the branches when there is a real choice', () => {
    renderStep(state({}, { branches: ['main', 'develop', 'topic'] }));
    expect(screen.getByTestId('step-branch')).toHaveTextContent(/3 branches on github\.com/i);
  });
});

// When the answer is about a DIFFERENT branch than the one on screen, the card
// has to say which. A remote this machine initialized earlier answers 'yes'
// about agent/<host>; `main` is on screen and has no ontology at all, so
// "main already holds a knowledge base" names the one branch that provably
// does not, and the reader has no way to tell what knomit found.
describe('StepBranch when the answer is about knomit\'s own branch', () => {
  it('names the inspected branch instead of the chosen one', () => {
    renderStep(state({ initialized: 'yes', initializedBranch: 'agent/box-1' }));
    const card = screen.getByTestId('branch-initialized');
    expect(card).toHaveTextContent(/agent\/box-1/);
    expect(card).toHaveTextContent(/knomit's own branch|earlier/i);
  });

  it('says nothing extra when the inspected branch IS the chosen one', () => {
    renderStep(state({ initialized: 'yes', initializedBranch: 'main' }));
    const card = screen.getByTestId('branch-initialized');
    expect(card).toHaveTextContent(/^main already holds a knowledge base/);
    expect(card).not.toHaveTextContent(/earlier/i);
  });
});
