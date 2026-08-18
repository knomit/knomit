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
  // Same fallback createBodyFor uses, and for the same reason: this line must
  // name the ontology that will actually be sent, so reading 'default' off a
  // cleared state.preset here would make the review page confidently wrong
  // about the one choice that cannot be changed after creation.
  const presetLabel = state.yaml ? 'a custom' : `the "${state.preset || state.seedPreset}"`;
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
      {/* INITIALIZE. The two statements this case must make are that {branch}
          is not changed, and that a merge request is the reader's next step —
          both because they are true and because both correct an expectation
          the old flow created. The deleted "seed" mode DID write the consensus
          branch, which is what made it fail on protected branches; a reader who
          remembers that needs to be told plainly that it no longer happens.

          The merge request is listed as a step rather than buried in prose
          because it is the only part of this the reader has to do themselves,
          and nothing else in the product will remind them. */}
      {state.choice === 'remote' && state.initialized === 'no' && (
        <ol style={list}>
          <li>A new repository named "{state.name}" is created and connected to {state.url}.</li>
          <li>knomit takes its own branch from {branch}, and writes {presetLabel} ontology there as its first commit.</li>
          <li>
            That branch — and only that branch — is pushed. <b style={{ color: '#ddd' }}>{branch} is not changed</b>,
            so you don't need push access to it.
          </li>
          <li>From here on, every change you make syncs to that branch.</li>
          <li>
            When you want the knowledge base to become the project's agreed state, open a
            merge request from knomit's branch into {branch}.
          </li>
        </ol>
      )}
      {/* JOIN. No ontology line to write, because there is no choice being
          made here: the remote's own governs, and the backend refuses one
          supplied alongside a clone rather than silently dropping it. */}
      {state.choice === 'remote' && state.initialized === 'yes' && (
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
            {state.probe?.auth_required
              ? 'Its branches were not listed — the check ran without access to them.'
              : (state.probe?.branches.length ?? 0) > 0
                ? `Branches already there: ${state.probe?.branches.join(', ')}.`
                : 'No other branches were found on the remote.'}
          </li>
          <li>Your work goes to knomit's own branch, and syncs there. {branch} is not changed.</li>
        </ol>
      )}
      {/* Every remote case above PUSHES — the agent branch, never the
          consensus branch. Saying so where write access was refused (or never
          established) is the difference between a create that fails at 70% as
          a surprise and one that fails as a stated risk. Amber only for the
          refusal — an unestablished check has not failed at anything.

          Neither message may read as a GUARANTEE in the other direction. The
          access check is a receive-pack advertisement: it establishes that the
          host will talk to these credentials about pushing, and it cannot
          predict a pre-receive hook, which runs on the content of the push.
          That is why there is no green "push will succeed" card here at all. */}
      {state.choice === 'remote' && state.probe?.write_access === 'denied' && (
        <div data-testid="review-write-denied" style={warn}>
          The access check could read this remote but was refused push access, and
          the steps above push knomit's own branch. Unless that has changed since,
          this will fail when knomit writes.
        </div>
      )}
      {state.choice === 'remote' && !state.probe?.write_access && (
        <div data-testid="review-write-unknown" style={note}>
          Push access to this remote was not established — knomit will find out
          when it writes.
        </div>
      )}
    </div>
  );
}

const list: React.CSSProperties = { margin: '8px 0 0', paddingLeft: 20, fontSize: 13, color: '#ccc', lineHeight: 1.7 };
const warn: React.CSSProperties = { marginTop: 10, fontSize: 12.5, color: '#d2a24c', lineHeight: 1.55, maxWidth: '74ch' };
const note: React.CSSProperties = { marginTop: 10, fontSize: 12.5, color: '#777', lineHeight: 1.55, maxWidth: '74ch' };
