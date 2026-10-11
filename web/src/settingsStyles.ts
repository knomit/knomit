import { repoHueBg, repoHueBorder } from './utils';

export const detailHead: React.CSSProperties = { display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start' };

// headActions is the page header's button cluster — just Browse now that every
// other action sits in the block that owns it. It never wraps under the title.
export const headActions: React.CSSProperties = { display: 'flex', alignItems: 'center', gap: 8, flexShrink: 0 };

// dangerBox tints the destructive block apart from every other body on the
// page. It is a box where the others are bare precisely because it should read
// as a fenced-off area rather than one more setting.
export const dangerBox: React.CSSProperties = {
  background: '#161111', border: '1px solid #3a2626', borderRadius: 6, padding: '11px 13px',
};
// Monospace: a repo name is an identifier that appears in URLs, so it is typed
// and compared character by character.
export const renameInput: React.CSSProperties = {
  background: '#141414', border: '1px solid #3a2020', borderRadius: 4,
  color: '#ddd', fontFamily: 'var(--k-font-mono)', fontSize: 12,
  padding: '5px 8px', minWidth: 170, outline: 'none',
};

// browseBtn is the header's "Browse" button, shared by the lens and repo pages.
//
// It was a filled LENS.accent pill — right for a small pane where it was one of
// two controls, wrong on a full page, where a saturated fill made the quietest
// action on screen the loudest thing on it. It was also a LENS colour on a repo
// page, which said the wrong thing twice.
//
// It reads as a button in the page's own vocabulary now — the same bordered
// dark shape as Rebuild, Restore and Connect — and carries the entity's colour
// in its ICON instead, so it still says which thing it goes to. It does not
// need a fill to be found: it is the only control in the header.
export const browseBtn: React.CSSProperties = {
  display: 'flex', alignItems: 'center', gap: 6, flexShrink: 0,
  background: '#242424', color: '#e6e6e6', border: '1px solid #3a3a3a', borderRadius: 5,
  padding: '6px 12px', fontSize: 13, cursor: 'pointer',
};

// repoIconBox mirrors lensIconBox but tinted in the repo's own deterministic
// hue (RepoDot's palette), so a repo detail reads as structurally identical to
// a lens detail while staying colour-coded per repo.
export const repoIconBox = (repo: string): React.CSSProperties => ({
  width: 30, height: 30, borderRadius: 7, flexShrink: 0,
  background: repoHueBg(repo), border: '1px solid ' + repoHueBorder(repo),
  display: 'flex', alignItems: 'center', justifyContent: 'center',
});

export const writeReadTag: React.CSSProperties = { fontSize: 10, color: '#7c9', background: '#1a2e1a', border: '1px solid #2a4a2a', padding: '1px 7px', borderRadius: 3, letterSpacing: '0.03em' };
export const readTag: React.CSSProperties = { fontSize: 10, color: '#777', border: '1px solid #333', padding: '1px 7px', borderRadius: 3, letterSpacing: '0.03em' };

// The spec's warning callout: amber on dark amber. Amber, not red — the
// registration is intact, and several of these states clear themselves. Used
// by a repo with no store and by a lens with a mount that has none.
export const warnCallout: React.CSSProperties = {
  background: '#1d1a12', border: '1px solid #4a3f22', borderRadius: 6,
  color: '#e2c07a', padding: '10px 12px', display: 'flex', flexDirection: 'column', gap: 6,
};
export const warnCalloutDot: React.CSSProperties = { width: 7, height: 7, borderRadius: '50%', background: '#e2c07a', flexShrink: 0 };
export const warnDetail: React.CSSProperties = {
  fontSize: 12, color: '#d9cfae', lineHeight: 1.6, fontFamily: 'var(--k-font-mono)',
  paddingLeft: 15, overflowWrap: 'anywhere',
};
