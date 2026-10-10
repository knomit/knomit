import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup } from '@testing-library/react';
import { WizardStepRail } from './WizardStepRail';
import { initialWizardState, type WizardState } from './wizardState';
import type { ProbeResult } from './api';

// The step rail is the wizard's index column now: a column of numbered
// circles, done steps with a green check and a mono summary of what they
// settled, the current step in the active style — and shown even with one step.

const probe = { reachable: true, empty: false, auth_required: false, branches: ['main'], default_branch: 'main' } as unknown as ProbeResult;

afterEach(() => { cleanup(); });

describe('WizardStepRail', () => {
  it('is shown with a single step, which is the current one', () => {
    render(<WizardStepRail state={initialWizardState} dispatch={() => {}} />);
    const items = screen.getAllByTestId(/^wizard-step-\d$/);
    expect(items).toHaveLength(1);
    expect(items[0]).toHaveAttribute('aria-current', 'step');
    expect(items[0]).toHaveTextContent('Source');
  });

  it('draws a done step with a check and its summary, and the current step in the active style', () => {
    // A probed remote at the branch step: source and access are done.
    const s: WizardState = {
      ...initialWizardState, url: 'https://github.com/acme/kb.git', probe, authMethod: 'token',
      probeKey: ['https://github.com/acme/kb.git', 'token', ''].join('\u0000'), stepIndex: 2,
    };
    render(<WizardStepRail state={s} dispatch={() => {}} />);
    expect(screen.getByTestId('wizard-step-1-check')).toBeInTheDocument();
    expect(screen.getByTestId('wizard-step-1-summary')).toHaveTextContent('github.com/acme/kb');
    expect(screen.getByTestId('wizard-step-1-summary')).toHaveStyle({ fontFamily: 'var(--k-font-mono)', textOverflow: 'ellipsis' });
    expect(screen.getByTestId('wizard-step-2-summary')).toHaveTextContent('token');
    // The current step: no check, no summary, the active treatment.
    const current = screen.getByTestId('wizard-step-3');
    expect(current).toHaveAttribute('aria-current', 'step');
    expect(screen.queryByTestId('wizard-step-3-check')).toBeNull();
    expect(screen.queryByTestId('wizard-step-3-summary')).toBeNull();
    expect(current).toHaveStyle({ background: '#1a231d', color: '#dfe9e2' });
  });

  it('a done step goes back to that step; the current step is not a button', () => {
    const s: WizardState = { ...initialWizardState, choice: 'local', name: 'notes', stepIndex: 1 };
    const dispatch = vi.fn();
    render(<WizardStepRail state={s} dispatch={dispatch} />);
    expect(screen.getByTestId('wizard-step-2').tagName).not.toBe('BUTTON');
    fireEvent.click(screen.getByTestId('wizard-step-1'));
    expect(dispatch).toHaveBeenCalledWith({ type: 'GOTO', step: 'source' });
  });
});
