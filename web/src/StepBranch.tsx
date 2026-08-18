import type { WizardAction, WizardState } from './wizardState';
import { hostOf, branchCheckBlocked } from './wizardState';
import { OutcomeCard } from './OutcomeCard';

// StepBranch asks which branch on the remote is the CONSENSUS branch — the one
// knomit reads as the project's agreed state and eventually merges into.
//
// It is its own step for two reasons, neither cosmetic:
//
//   1. Its answer decides whether an ontology step exists at all. A branch that
//      already carries .knomit/ontology.yaml is joined and its ontology governs;
//      one that does not is initialized with an ontology the user picks. The
//      wizard cannot know which shape it is until this is answered.
//   2. The check that establishes that runs on LEAVING it, and can fail. A step
//      that can block needs somewhere to render the block.
//
// The check is per-branch and not merely per-remote: a repository can carry the
// ontology on main and not on develop, so the same URL has as many answers as
// it has branches. That is why this cannot be folded into the access step's
// probe, which runs before any branch is chosen.
//
// knomit never writes to whichever branch is chosen here. It writes to its own
// agent branch, cut from this one — which is why the create needs no push
// access to a protected `main`.
//
// ── Layout ───────────────────────────────────────────────────────────────
//
// The question is FRAMED, in the same box grammar the outcome cards use, and
// the step's forward action lives in the wizard footer with every other step's.
// Before that it was a bare 12px label, a dim paragraph, one lonely chip that
// read as a text field, and a primary button stacked directly above the footer
// row — two rows of buttons, and the only step whose forward motion was not
// where the reader had learned to look for it.
//
// The frame is built here rather than by borrowing OutcomeCard. That component
// reports what a remote SAID; this asks the reader a question, and giving a
// question the reporting component's identity would blur what a card means.
// They share the tone palette instead, so they still read as one system.
export function StepBranch({ state, dispatch }: {
  state: WizardState;
  dispatch: (a: WizardAction) => void;
}) {
  const probe = state.probe;
  const branches = probe?.branches ?? [];
  const selected = state.branch || probe?.upstream_branch || 'main';
  const host = hostOf(state.url) || 'the remote';
  const blocked = branchCheckBlocked(state);
  // Which branch the established answer is ABOUT. Falls back to the selection
  // when the server did not say — an older server, or the unestablished state,
  // where nothing is claimed anyway.
  const answeredAbout = state.initializedBranch || selected;
  const ownBranchAnswered = state.initializedBranch !== '' && state.initializedBranch !== selected;

  return (
    <div data-testid="step-branch">
      <section style={panel}>
        <h4 style={panelHead}>Consensus branch on {host}</h4>
        <p style={panelBody}>
          The branch knomit treats as the project's agreed state. knomit never writes
          to it — it works on its own branch, and you merge when you're ready.
        </p>

        {branches.length > 0 ? (
          <>
            {/* Says how much of a choice this actually is. One branch is the
                common case for a repository made for a knowledge base, and a
                lone chip with no caption reads as an input the reader is
                supposed to type in rather than the answer already found. */}
            <div style={caption}>
              {branches.length === 1
                ? `the only branch on ${host}`
                : `${branches.length} branches on ${host}`}
            </div>
            <div style={branchGroup} role="group" aria-label="Consensus branch">
              {branches.map(b => {
                const on = b === selected;
                return (
                  <button key={b} type="button" data-testid={`branch-option-${b}`} style={chip(on)}
                    aria-pressed={on} onClick={() => dispatch({ type: 'SET_BRANCH', branch: b })}>
                    {/* The selected chip is marked, not merely tinted: with one
                        chip there is nothing to compare a tint against, and the
                        rail above already spends this glyph on "settled". */}
                    {on && <span aria-hidden="true" style={tick}>✓</span>}
                    {b}
                  </button>
                );
              })}
            </div>
          </>
        ) : (
          // An auth_required probe returns branches: [] because it was REFUSED,
          // not because the remote has none — so this falls back to a free-text
          // field rather than claiming the remote is branchless. The access step
          // re-probes before letting anyone reach this step, so it should be
          // unreachable; it is handled anyway, because the alternative is a step
          // with no control on it at all.
          <>
            <div style={caption}>branch name</div>
            <input data-testid="branch-input" style={input} value={state.branch}
              placeholder="main" autoCapitalize="off" autoCorrect="off" spellCheck={false}
              onChange={e => dispatch({ type: 'SET_BRANCH', branch: e.target.value })} />
          </>
        )}
      </section>

      {/* THE THIRD STATE, rendered as itself. Not "this branch has no
          knowledge base" — that is an answer, and we do not have one. Both
          guesses are unrecoverable: the ontology is fixed at create time, so
          guessing "already one" discards the ontology the reader is about to
          choose, and guessing "not one" writes over the ontology that already
          governs their knowledge base. So the wizard stops here and offers
          the one thing that can still help: another attempt. */}
      {blocked && (
        <div style={{ marginTop: 12 }}>
          <OutcomeCard
            testid="branch-blocked"
            tone="bad"
            headline="Couldn't tell whether this branch already has a knowledge base"
            body={<>
              knomit stops rather than guess. Its ontology is set once, when the
              repository is created, and can't be changed afterwards — so guessing
              wrong would either throw away the ontology you pick or write over one
              that's already there.
            </>}
            detail={state.initializedDetail}
            detailLabel={`${host} said`}
          />
        </div>
      )}

      {/* The two established answers, stated as consequences rather than as a
          verdict — the reader's next step differs, and that is what they need
          to know. Neither is a failure, so neither is amber. */}
      {state.initialized === 'yes' && (
        <div style={{ marginTop: 12 }}>
          {/* NAME the branch the answer is about. It is not always the one on
              screen: a create reads whatever knomit adopts, which is this
              machine's own branch when the remote already carries one — so a
              remote set up from this machine before answers about
              agent/<host> while `main`, the branch selected here, has no
              ontology and never will. Saying "main already holds a knowledge
              base" there names the one branch that provably does not. */}
          <OutcomeCard
            testid="branch-initialized"
            tone="good"
            headline={`${answeredAbout} already holds a knowledge base`}
            body={ownBranchAnswered
              ? <>That is knomit&rsquo;s own branch on this remote, from an earlier setup on this
                machine — <span style={mono}>{selected}</span> itself has no ontology. knomit will
                rejoin that branch and pick up where it left off; its ontology governs, so there is
                nothing to choose.</>
              : <>knomit will join it. Its ontology comes from the remote, so there&rsquo;s nothing to choose.</>}
          />
        </div>
      )}
      {state.initialized === 'no' && (
        <div style={{ marginTop: 12 }}>
          <OutcomeCard
            testid="branch-uninitialized"
            tone="good"
            headline={`${selected} isn't a knowledge base yet`}
            body={<>Next you'll pick an ontology. knomit writes it to its own branch, cut from {selected} — {selected} itself isn't changed.</>}
          />
        </div>
      )}
    </div>
  );
}

// ── styles ──
//
// Blue throughout, from OutcomeCard's 'ask' tone: the hue this UI already
// spends on branches and remote refs, and the tone reserved for "knomit needs
// something from you before it can go on", which is exactly what this step is.
// Reusing those values rather than inventing near-misses is what keeps the
// question and the answer beneath it looking like one surface.
const panel: React.CSSProperties = {
  borderRadius: 6, padding: '12px 14px', background: '#101720', border: '1px solid #24405e',
};
const panelHead: React.CSSProperties = {
  margin: 0, fontSize: 13.5, fontWeight: 600, color: '#a8c8e8',
};
const panelBody: React.CSSProperties = {
  margin: '8px 0 0', fontSize: 12.5, lineHeight: 1.55, color: '#8aa8c2', maxWidth: '64ch',
};
// The same caption grammar OutcomeCard uses over a quoted server message: this
// is a small label naming what sits under it, and there is no reason for the
// two to differ.
const caption: React.CSSProperties = {
  margin: '14px 0 6px', fontFamily: 'var(--k-font-mono)', fontSize: 10,
  letterSpacing: 1.2, textTransform: 'uppercase', color: '#5f7d96',
};
const branchGroup: React.CSSProperties = { display: 'flex', flexWrap: 'wrap', gap: 6 };
const chip = (on: boolean): React.CSSProperties => ({
  display: 'inline-flex', alignItems: 'center', gap: 6,
  padding: '5px 11px', borderRadius: 5, cursor: 'pointer', fontSize: 12.5,
  fontFamily: 'var(--k-font-mono)',
  // Darker than the panel it sits on, so a chip reads as set INTO the card
  // rather than floating over it.
  background: on ? '#17293c' : '#0c1219',
  border: '1px solid ' + (on ? '#3a5f8a' : '#1e3348'),
  color: on ? '#a8c8e8' : '#7d93a8',
});
const mono: React.CSSProperties = { fontFamily: 'var(--k-font-mono)', color: '#a8c8e8' };
const tick: React.CSSProperties = { fontSize: 10, lineHeight: 1, color: '#6f9ac9' };
const input: React.CSSProperties = {
  width: '100%', boxSizing: 'border-box', background: '#0c1219', border: '1px solid #1e3348',
  color: '#dce7f2', padding: '6px 8px', borderRadius: 4, fontSize: 13,
  fontFamily: 'var(--k-font-mono)',
};
