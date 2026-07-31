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
 *  opposed to the pane-level ⋯ menu's whole-object actions. */
export const cardIconBtn: React.CSSProperties = {
  display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
  width: 24, height: 24, borderRadius: 4, padding: 0,
  background: 'none', border: 'none', cursor: 'pointer',
};

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
