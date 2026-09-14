// Shared visual tokens for the Manage dialog (RepoManager + RemoteStatus).
//
// These lived as private consts in RepoManager.tsx, with RemoteStatus.tsx
// carrying its own near-identical copies of btn/confirmBox and a `sectionLabel`
// that did NOT match descLabel — which is why the Remote block read as a
// bolted-on section rather than one of the detail pane's cards. One module, one
// definition, so every card in the pane is structurally identical.

/** card is the standard detail-pane card: a bordered dark box with a label. */
export const card: React.CSSProperties = {
  marginTop: 14, padding: '10px 12px',
  background: '#111', border: '1px solid #2a2a2a', borderRadius: 6,
};

/** cardLabel is the small uppercase caption at the top of a card. */
export const cardLabel: React.CSSProperties = {
  fontSize: 10, textTransform: 'uppercase', letterSpacing: 1.5,
  color: '#555', marginBottom: 6,
};

/** writeCard / writeCardLabel: the green "new facts land here" card. Used by
 *  BOTH a repo's Agent branch and a lens's Write target — they make the same
 *  statement, so they get the same treatment (green already reads as "write"
 *  in this UI via the write·read mount tag). */
export const writeCard: React.CSSProperties = {
  ...card, background: '#1a2a1a', borderColor: '#2a4a2a',
};
export const writeCardLabel: React.CSSProperties = { ...cardLabel, color: '#6a9a6a' };

/** cardIconBtn is a small square icon action in a card's header — for actions
 *  that edit THAT card's data (reconnect the remote, edit the read mounts), as
 *  opposed to the pane-level ⋯ menu's whole-object actions.
 *
 *  Takes `disabled` for the same reason btn() does: an inline style cannot
 *  express `:disabled`, so a disabled icon button keeps a pointer cursor and
 *  goes on reading as clickable. On a 13px glyph the dimmed tint is too small
 *  a cue to carry that alone. */
export const cardIconBtn = (disabled = false): React.CSSProperties => ({
  display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
  width: 24, height: 24, borderRadius: 4, padding: 0,
  background: 'none', border: 'none',
  cursor: disabled ? 'default' : 'pointer',
});

export const btn = (
  disabled: boolean,
  variant: 'primary' | 'secondary' | 'danger' = 'secondary',
): React.CSSProperties => ({
  background: disabled ? '#222' : variant === 'primary' ? '#1d4ed8' : variant === 'danger' ? '#7f1d1d' : '#2a2a2a',
  color: disabled ? '#666' : '#eee',
  border: '1px solid #333', borderRadius: 4,
  padding: '6px 12px', fontSize: 13,
  cursor: disabled ? 'default' : 'pointer',
});

export const confirmBox: React.CSSProperties = {
  marginTop: 14, padding: 14,
  background: '#111', border: '1px solid #333', borderRadius: 6,
};

export const confirmInput: React.CSSProperties = {
  width: '100%', boxSizing: 'border-box',
  background: '#0c0c0c', border: '1px solid #333', color: '#eee',
  padding: '6px 8px', borderRadius: 4, fontSize: 13,
};

/** linkBtn is the inline blue "change…" affordance inside a card. */
export const linkBtn: React.CSSProperties = {
  background: 'none', border: 'none', color: '#6ea8fe',
  cursor: 'pointer', fontSize: 12, padding: 0, textDecoration: 'underline',
};

/** Segmented choice — the wizard's control for a binary that one question
 *  settles (StepSource: where the history lives; StepReview: how to attach to
 *  a branch that is already a knowledge base). One definition so the wizard
 *  asks both questions the same way.
 *
 *  The tone encodes STATE, not rank. 'remote' is #8af, the hue this UI already
 *  spends on branches and remote refs, for the side that has a remote to talk
 *  about — connect a repository, or join and push. 'neutral' is a plain slate
 *  for the side that does not: keep it local, or subscribe and never write.
 *  Neither is amber, green or purple — those mean failure, write target and
 *  lens respectively. */
export type SegTone = 'remote' | 'neutral';

const REMOTE_ACCENT = '#8af';
const NEUTRAL_ACCENT = '#8b9199';

export const segGroup: React.CSSProperties = { display: 'flex', gap: 6, flexWrap: 'wrap' };

export const segment = (on: boolean, tone: SegTone): React.CSSProperties => ({
  flex: '1 1 220px', display: 'flex', alignItems: 'flex-start', gap: 9,
  padding: '10px 12px', borderRadius: 6, cursor: 'pointer', textAlign: 'left',
  fontSize: 13, fontFamily: 'inherit',
  background: on ? (tone === 'remote' ? '#10161f' : '#17181a') : '#0f0f0f',
  border: '1px solid ' + (on ? (tone === 'remote' ? '#24405e' : '#3a3d42') : '#242424'),
  color: on ? '#eee' : '#999',
});

export const segDot = (on: boolean, tone: SegTone): React.CSSProperties => {
  const accent = tone === 'remote' ? REMOTE_ACCENT : NEUTRAL_ACCENT;
  return {
    width: 9, height: 9, borderRadius: '50%', flexShrink: 0, marginTop: 5,
    background: on ? accent : 'transparent',
    border: '1.5px solid ' + (on ? accent : '#4a4a4a'),
  };
};

export const segSub = (on: boolean, tone: SegTone): React.CSSProperties => ({
  display: 'block', fontSize: 11.5, marginTop: 1,
  color: on ? (tone === 'remote' ? '#6a89ad' : '#7d838b') : '#555',
});

/** segDisclosure is the rule between a segmented choice and what it discloses:
 *  the content below belongs to the segment above, and without it the pane
 *  reads as a second, unrelated section. */
export const segDisclosure: React.CSSProperties = {
  borderTop: '1px solid #242424', marginTop: 14, paddingTop: 2,
};
