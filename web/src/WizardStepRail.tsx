import { stepsFor, currentStep, type StepId, type WizardAction, type WizardState } from './wizardState';

// WizardStepRail is the create wizard's numbered progress rail.
//
// Markup and accessibility are RemoteConnectWizard's (RemoteConnectWizard.tsx,
// its <nav aria-label="Progress">): the same <nav>, the same aria-current="step"
// on the active item, the same data-testid="wizard-step-N", the same disc-plus-
// label item. Two wizards in the same product with different chrome and
// different accessibility is not a defensible end state, so this is deliberately
// that component's idiom rather than a second one.
//
// It differs in two ways, both forced by this wizard rather than chosen:
//
//   - The step list is DERIVED, not fixed. RemoteConnectWizard always has the
//     same three steps; here stepsFor(state) is the authority and the list
//     changes shape when the probe does (an empty remote gains an 'ontology'
//     step). Never hardcode a list here.
//   - Completed steps are buttons, because they ARE reachable — BACK already
//     walks the same path. Only completed ones: a todo step is not clickable,
//     so the rail can never skip a step the user has not finished. That is why
//     GOTO exists in wizardState and why this is the only thing that dispatches
//     it.
const LABELS: Record<StepId, string> = {
  source: 'Source', access: 'Access', branch: 'Branch', ontology: 'Ontology', review: 'Review',
};

export function WizardStepRail({ state, dispatch }: { state: WizardState; dispatch: (a: WizardAction) => void }) {
  const steps = stepsFor(state);
  // A one-item rail is not a tracker, it is a label — and on the source step it
  // would be a LIE by omission, claiming the wizard is one step long when it
  // has not yet learned what shape the remote makes it. stepsFor collapses to
  // ['source'] exactly while that is unknown, so this hides itself there.
  if (steps.length < 2) return null;
  const active = steps.indexOf(currentStep(state));

  return (
    <nav style={rail} aria-label="Progress">
      {steps.map((id, i) => {
        const st: StepState = i === active ? 'active' : i < active ? 'done' : 'todo';
        const mark = <span style={railMark(st)}>{st === 'done' ? '✓' : i + 1}</span>;
        const testid = `wizard-step-${i + 1}`;
        if (st === 'done') {
          return (
            <button key={id} type="button" data-testid={testid} style={{ ...railItem(st), cursor: 'pointer', border: 'none' }}
              onClick={() => dispatch({ type: 'GOTO', step: id })}>
              {mark}<span>{LABELS[id]}</span>
            </button>
          );
        }
        return (
          <div key={id} data-testid={testid} aria-current={st === 'active' ? 'step' : undefined} style={railItem(st)}>
            {mark}<span>{LABELS[id]}</span>
          </div>
        );
      })}
    </nav>
  );
}

// ── styles ──
//
// Values lifted from RemoteConnectWizard's rail so the two read as one grammar.
// Laid out as a ROW rather than that page's column: this wizard is a panel
// inside RepoManager, not a full page with a sidebar, and a column rail would
// need a two-column shell the panel does not have.
type StepState = 'active' | 'done' | 'todo';

const rail: React.CSSProperties = {
  display: 'flex', flexWrap: 'wrap', gap: 4, margin: '0 0 14px',
};
const railItem = (state: StepState): React.CSSProperties => ({
  display: 'flex', alignItems: 'center', gap: 8,
  padding: '4px 9px', borderRadius: 4, fontSize: 11.5, textAlign: 'left',
  background: state === 'active' ? '#1a231d' : 'transparent',
  color: state === 'active' ? '#dfe9e2' : state === 'done' ? '#7f9a89' : '#5e5e5e',
  boxShadow: state === 'active' ? 'inset 2px 0 0 -0.5px #7c9' : 'none',
});
const railMark = (state: StepState): React.CSSProperties => ({
  width: 15, height: 15, borderRadius: '50%', flexShrink: 0,
  display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
  fontSize: 9, fontWeight: 700,
  background: state === 'active' ? '#24352b' : state === 'done' ? '#1c2a20' : '#1c1c1c',
  border: '1px solid ' + (state === 'active' ? '#3e5c4a' : state === 'done' ? '#2e4636' : '#2c2c2c'),
  color: state === 'todo' ? '#666' : state === 'done' ? '#7c9' : '#dfe9e2',
});
