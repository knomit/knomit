import { useState } from 'react';
import type { CSSProperties } from 'react';
import { api, MAX_ARCHIVE_NOTE_BYTES, type RepoInfo, type ArchivedRepo } from './api';
import { btn, confirmBox, confirmInput } from './manageStyles';
import { SettingsPage } from './SettingsPage';
import { warnCallout as callout, warnCalloutDot as calloutDot, warnDetail as detail } from './settingsStyles';
import type { Section } from './SettingsPage';

/**
 * UnavailableSettings is the settings page of a repository that has no live
 * store: a target pinned from the rail, with Settings as its only facet (the
 * top bar's label carries the state chip).
 *
 * Two sections and no more. "Store" says what is wrong, in the server's own
 * words, and what can be done about it; "Danger zone" holds Archive. Every
 * other section a repository page carries reads through the store this repo
 * does not have, and would render as a wall of 409s.
 *
 * Archive is the one action, because it is the one the server supports without
 * a store: the registration moves into Archived with this state recorded as its
 * reason, and from there it can be purged and the same repository added again.
 * "Check again", "Locate store…" and "Clone from remote" are not offered — the
 * server has no such operations, and a button that does nothing is worse than
 * none.
 *
 * The three states want different moves, which is why the server sends the
 * reason rather than a bare failure, and why the advice branches on it.
 */
export function UnavailableSettings({ repo, serverReadOnly, onArchived }: {
  repo: RepoInfo;
  /** The instance accepts no mutations: Archive is disabled. */
  serverReadOnly: boolean;
  /** Archived: the server's archived row, so the caller can pin it by id. */
  onArchived: (archived: ArchivedRepo) => void;
}) {
  const state = repo.state ?? 'unavailable';
  const [confirming, setConfirming] = useState(false);
  const [note, setNote] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const noteBytes = new TextEncoder().encode(note.trim()).length;
  const archive = async () => {
    setErr(''); setBusy(true);
    try { onArchived(await api.archiveRepo(repo.name, note)); }
    catch (e) { setErr(`archive failed: ${String(e)}`); }
    finally { setBusy(false); }
  };

  const sections: Section[] = [
    {
      id: 'store',
      title: 'Store',
      problem: true,
      body: (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
          <div style={lede}>Browse and Activity return once the store is reachable.</div>
          <div data-testid="repo-unavailable-callout" style={callout}>
            <div style={{ display: 'flex', alignItems: 'center', gap: 8, fontWeight: 600, fontSize: 12.5 }}>
              <span style={calloutDot} />
              {HEADLINE[state] ?? `Store ${state}`}
            </div>
            {/* The server's own words. It knows things this build cannot infer
                (which file, which other repo), so it is quoted, not
                paraphrased. */}
            {repo.detail && (
              <div data-testid="repo-unavailable-detail" style={detail}>{repo.detail}</div>
            )}
          </div>
          {ADVICE[state] && <div style={lede}>{ADVICE[state]}</div>}
        </div>
      ),
    },
    {
      id: 'danger',
      title: 'Danger zone',
      danger: true,
      body: (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
          <div style={lede}>
            Archiving needs no store and deletes nothing: the registration moves into Archived with this state
            recorded as its reason. From there it can be purged, and the same repository added again.
          </div>
          {!confirming ? (
            <div>
              <button type="button" data-testid="repo-unavailable-archive" style={btn(serverReadOnly || busy, 'danger')}
                disabled={serverReadOnly || busy} onClick={() => { setNote(''); setConfirming(true); }}>
                Archive…
              </button>
            </div>
          ) : (
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
          {err && <div data-testid="repo-unavailable-archive-error" style={{ fontSize: 12, color: '#f88' }}>{err}</div>}
        </div>
      ),
    },
  ];

  return (
    <div data-testid={`repo-unavailable-${repo.name}`} style={{ flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column' }}>
      <SettingsPage kind="repo" sections={sections} testid="unavailable-settings" />
    </div>
  );
}

const HEADLINE: Record<string, string> = {
  missing: 'Store missing',
  unopenable: 'Store cannot be opened',
  conflict: 'Store held by another repository',
};

// Every sentence here names something the product can actually do TODAY.
const ADVICE: Record<string, string> = {
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

const lede: CSSProperties = { fontSize: 12.5, color: '#888', lineHeight: 1.6 };

