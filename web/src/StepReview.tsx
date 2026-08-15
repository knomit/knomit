import type { WizardState } from './wizardState';
import { cardLabel } from './manageStyles';

// StepReview states CONSEQUENCES of the choices already made, not the inputs
// themselves — the wizard's earlier steps already showed those. It never
// claims fact or commit counts: a probe is remote.ListContext and sees refs
// only, so it cannot know either, and claiming them would be a promise the
// backend cannot back up.
//
// Warning styling here is reserved for things that FAIL (an ontology that
// doesn't validate, a name already taken) — by the time a case reaches
// review, its own failure mode (an unreachable remote) has already been
// caught on an earlier step, so this component has nothing to render in
// amber. Trade-offs — like the local-only case losing revision history —
// are plain text: amber on every line would mean nobody reads it on the one
// that matters.
export function StepReview({ state }: { state: WizardState }) {
  const presetLabel = state.yaml ? 'a custom' : `the "${state.preset || 'default'}"`;
  const branch = state.branch || state.probe?.upstream_branch || 'main';

  return (
    <div data-testid="step-review">
      <div style={cardLabel}>What will happen</div>
      {state.choice === 'local' && (
        <ol style={list}>
          <li>A new local-only repository named "{state.name}" is created on this machine, seeded with {presetLabel} ontology.</li>
          {/* Verbatim, agreed copy — do not paraphrase. */}
          <li>You can connect a remote whenever you like, and all your facts come across. The only thing that doesn't follow is each fact's earlier revisions — starting from a remote keeps that full timeline.</li>
        </ol>
      )}
      {state.choice === 'remote' && state.probe?.empty && (
        <ol style={list}>
          <li>A new repository named "{state.name}" is created and connected to {state.url} (branch {branch}).</li>
          <li>Because the remote has no history yet, knomit writes {presetLabel} ontology as the very first commit there.</li>
          <li>From here on, every change you make syncs to that remote.</li>
        </ol>
      )}
      {state.choice === 'remote' && state.probe && !state.probe.empty && (
        <ol style={list}>
          <li>Repository "{state.name}" is created by cloning {state.url} (branch {branch}).</li>
          <li>Its ontology comes from the remote itself — not a choice made here.</li>
          {/* An auth_required probe returns branches: [] because it was
              REFUSED, not because the remote has none. Saying "no other
              branches were found" there states as fact something the probe
              never established — exactly what design §3's "What the review may
              claim" forbids. Under the current flow the access step re-probes
              before letting you reach review, so this should be unreachable;
              it is guarded anyway, because the cost of being wrong is a false
              claim about the user's own data. */}
          <li>
            {state.probe.auth_required
              ? 'Its branches were not listed — the check ran without access to them.'
              : state.probe.branches.length > 0
                ? `Branches already there: ${state.probe.branches.join(', ')}.`
                : 'No other branches were found on the remote.'}
          </li>
        </ol>
      )}
    </div>
  );
}

const list: React.CSSProperties = { margin: '8px 0 0', paddingLeft: 20, fontSize: 13, color: '#ccc', lineHeight: 1.7 };
