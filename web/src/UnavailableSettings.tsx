import { useState } from 'react';
import { api, MAX_ARCHIVE_NOTE_BYTES, type RepoInfo } from './api';
import { RepoStateChip } from './RepoStateChip';
import { btn, confirmBox, confirmInput } from './manageStyles';
import { dangerBox } from './settingsStyles';
import { RepoDot } from './settingsParts';

/**
 * UnavailableSettings is the settings page for a repository that has no live store.
 *
 * Its one control is Archive. Archiving does not need the store — the server
 * flips the registration and records why — and it is the way out of this
 * state: archived, the repo can be purged, and its name and knowledge base no
 * longer block adding the same one again. Every OTHER button a settings page
 * carries would resolve through the store this repo does not have, so none is
 * offered.
 *
 * The three states want different moves, which is exactly why the server sends
 * the reason rather than a bare failure, and why this branches on it instead of
 * printing one apology for all three.
 */
export function UnavailableSettings({ repo, readOnly, onArchived }: {
  repo: RepoInfo; readOnly: boolean; onArchived: () => void;
}) {
  const state = repo.state ?? 'unavailable';
  const [confirming, setConfirming] = useState(false);
  const [note, setNote] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  // Every sentence here names something the product can actually do TODAY.
  const advice: Record<string, string> = {
    missing:
      'Its database file is not where the registry says it is. Put the file back — from a backup, or wherever it '
      + 'was moved to — and restart knomit; the registration is intact and will pick it up. If the file is gone '
      + 'for good, archive the registration below.',
    // A symlinked ontology does not normally land here: knomit archives such a
    // repo at startup, with the reason. It can still show here when a lens
    // reads or writes the repo (archiving a lens member is refused).
    unopenable:
      'The file is there but could not be opened — a corrupt database, one written by a newer build, or an '
      + 'ontology that is a symlink, which knomit refuses rather than follow. The detail above names the cause, '
      + 'and the server log for this startup carries it in full. Repairing or replacing the file and restarting '
      + 'knomit clears this; a refused repository does not sync, so a fix pushed to its remote does not reach '
      + 'this copy on its own. To start over, archive it below, purge it, and add the repository again.',
    conflict:
      'Another registered repository already holds this knowledge base. Two local copies would both write the same '
      + 'agent branch and overwrite each other on push, so this one is left closed. Archive this copy below to '
      + 'keep the other — or archive the OTHER copy and restart knomit to hand this registration its knowledge '
      + 'base back.',
  };
  const noteBytes = new TextEncoder().encode(note.trim()).length;
  const archive = async () => {
    setErr(''); setBusy(true);
    try { await api.archiveRepo(repo.name, note); onArchived(); }
    catch (e) { setErr(`archive failed: ${String(e)}`); }
    finally { setBusy(false); }
  };
  return (
    <div data-testid={`repo-unavailable-${repo.name}`} style={{ maxWidth: 560, paddingTop: 30 }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
        <RepoDot repo={repo.name} />
        <h3 style={{ margin: 0, fontSize: 17, fontWeight: 600 }}>{repo.name}</h3>
        <RepoStateChip repo={repo} />
      </div>
      <p style={{ fontSize: 12.5, color: '#888', lineHeight: 1.6, marginTop: 12 }}>
        This repository is registered, but knomit has no store open for it, so none of it can be read or written.
        It is listed here — rather than quietly dropped, which is what used to happen — precisely so that this is
        visible.
      </p>
      {/* The server's own words, when it sent any. It knows things this build
          cannot infer (which file, which other repo), so it is quoted rather
          than paraphrased. */}
      {repo.detail && (
        <p data-testid="repo-unavailable-detail" style={{
          fontSize: 12, color: '#c9c9c9', lineHeight: 1.6, marginTop: 12,
          fontFamily: 'var(--k-font-mono)', background: '#131313',
          border: '1px solid #262626', borderRadius: 5, padding: '9px 11px',
        }}>{repo.detail}</p>
      )}
      {advice[state] && (
        <p style={{ fontSize: 12.5, color: '#888', lineHeight: 1.6, marginTop: 12 }}>{advice[state]}</p>
      )}
      <div style={{ ...dangerBox, marginTop: 16 }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
          <span style={{ fontSize: 13, color: '#ddd' }}>Archive this registration</span>
          <div style={{ flex: 1 }} />
          {!confirming && (
            <button type="button" data-testid="repo-unavailable-archive" style={btn(readOnly || busy, 'danger')}
              disabled={readOnly || busy} onClick={() => { setNote(''); setConfirming(true); }}>
              Archive…
            </button>
          )}
        </div>
        <div style={{ fontSize: 11.5, color: '#777', marginTop: 6 }}>
          Nothing is deleted and the store is not opened: the registration moves into Archived with this state
          recorded as its reason. From there it can be purged, and the same repository added again.
        </div>
        {confirming && (
          <div style={confirmBox}>
            <div style={{ fontSize: 13, marginBottom: 8 }}>Why are you archiving it? (optional)</div>
            <input autoFocus data-testid="repo-unavailable-archive-note" style={confirmInput} value={note}
              placeholder="e.g. replaced by a fresh clone" onChange={e => setNote(e.target.value)} />
            {noteBytes > MAX_ARCHIVE_NOTE_BYTES && (
              <div style={{ fontSize: 11.5, color: '#f88', marginTop: 6 }}>
                At most {MAX_ARCHIVE_NOTE_BYTES} bytes are kept.
              </div>
            )}
            <div style={{ display: 'flex', gap: 8, marginTop: 10 }}>
              <button type="button" data-testid="repo-unavailable-archive-confirm"
                style={btn(busy || noteBytes > MAX_ARCHIVE_NOTE_BYTES, 'danger')}
                disabled={busy || noteBytes > MAX_ARCHIVE_NOTE_BYTES} onClick={archive}>Archive</button>
              <button type="button" style={btn(busy)} disabled={busy} onClick={() => setConfirming(false)}>Cancel</button>
            </div>
          </div>
        )}
        {err && <div data-testid="repo-unavailable-archive-error" style={{ fontSize: 12, color: '#f88', marginTop: 8 }}>{err}</div>}
      </div>
    </div>
  );
}
