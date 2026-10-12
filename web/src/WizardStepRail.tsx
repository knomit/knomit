import type { CSSProperties } from 'react';
import { stepsFor, currentStep, stepSummary, type StepId, type WizardAction, type WizardState } from './wizardState';

// WizardStepRail is the create wizard's progress, drawn as the left index
// column of the repository draft: one numbered circle per step, a green check
// and a mono summary of what a DONE step settled, and the current step in the
// index's active style (the selected rail row).
//
// Markup and accessibility stay RemoteConnectWizard's (its <nav
// aria-label="Progress">): the same aria-current="step" on the active item and
// the same data-testid="wizard-step-N".
//
// The step list is DERIVED, not fixed: stepsFor(state) is the authority and
// the list changes shape when the probe does. Never hardcode a list here.
//
// Completed steps are buttons, because they ARE reachable — BACK already walks
// the same path. Only completed ones: a todo step is not clickable, so the rail
// can never skip a step the user has not finished. That is why GOTO exists in
// wizardState and why this is the only thing that dispatches it.
//
// Shown even with ONE step. As a row inside a panel it hid itself there (a
// one-item tracker read as a claim that the wizard is one step long); as the
// index column it is the column's content, and an empty column under a "New
// repository" heading would read as broken. stepsFor only ever lists steps
// that have been established, so a single item claims nothing about the rest.
const LABELS: Record<StepId, string> = {
  source: 'Source', access: 'Access', branch: 'Branch', ontology: 'Ontology', review: 'Review',
};

export function WizardStepRail({ state, dispatch }: { state: WizardState; dispatch: (a: WizardAction) => void }) {
  const steps = stepsFor(state);
  const active = steps.indexOf(currentStep(state));

  return (
    <nav style={rail} aria-label="Progress">
      {steps.map((id, i) => {
        const st: StepState = i === active ? 'active' : i < active ? 'done' : 'todo';
        const testid = `wizard-step-${i + 1}`;
        const summary = st === 'done' ? stepSummary(state, id) : '';
        const inner = (
          <>
            <span data-testid={st === 'done' ? `${testid}-check` : undefined} style={railMark(st)}>
              {st === 'done' ? <Check /> : i + 1}
            </span>
            <span style={{ flexShrink: 0 }}>{LABELS[id]}</span>
            {summary && <span data-testid={`${testid}-summary`} style={summaryStyle}>{summary}</span>}
          </>
        );
        if (st === 'done') {
          return (
            <button key={id} type="button" data-testid={testid} style={{ ...railItem(st), cursor: 'pointer' }}
              title={summary || undefined}
              onClick={() => dispatch({ type: 'GOTO', step: id })}>
              {inner}
            </button>
          );
        }
        return (
          <div key={id} data-testid={testid} aria-current={st === 'active' ? 'step' : undefined} style={railItem(st)}>
            {inner}
          </div>
        );
      })}
    </nav>
  );
}

const Check = () => (
  <svg width="9" height="9" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
    <polyline points="20 6 9 17 4 12" />
  </svg>
);

// ── styles ──
// Geometry is the index column's item (IndexColumn): 12.5px, padding 6px 10px,
// radius 4, and the active item wears the selected rail row's green.
type StepState = 'active' | 'done' | 'todo';

const rail: CSSProperties = { display: 'flex', flexDirection: 'column', gap: 1 };
const railItem = (state: StepState): CSSProperties => ({
  display: 'flex', alignItems: 'center', gap: 9, width: '100%', boxSizing: 'border-box',
  padding: '6px 10px', borderRadius: 4, fontSize: 12.5, textAlign: 'left',
  border: 'none', fontFamily: 'inherit',
  background: state === 'active' ? '#1a231d' : 'transparent',
  color: state === 'active' ? '#dfe9e2' : state === 'done' ? '#bbb' : '#5e5e5e',
  boxShadow: state === 'active' ? 'inset 2px 0 0 #7c9' : 'none',
});
const railMark = (state: StepState): CSSProperties => ({
  width: 17, height: 17, borderRadius: '50%', flexShrink: 0,
  display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
  fontSize: 9.5, fontWeight: 700,
  background: state === 'active' ? '#24352b' : state === 'done' ? '#1c2a20' : '#161616',
  border: '1px solid ' + (state === 'active' ? '#3e5c4a' : state === 'done' ? '#2e4636' : '#2c2c2c'),
  color: state === 'todo' ? '#555' : state === 'done' ? '#7c9' : '#dfe9e2',
});
// The summary truncates inside the column: the name of a long remote must not
// push the column wider than the Library it lines up with.
const summaryStyle: CSSProperties = {
  marginLeft: 'auto', minWidth: 0, maxWidth: '55%',
  overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
  fontFamily: 'var(--k-font-mono)', fontSize: 10.5, color: '#777',
};
