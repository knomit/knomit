import { useState } from 'react';
import type { CSSProperties } from 'react';
import { api, type ArchivedRepo } from './api';
import { formatBytes } from './utils';
import { btn, confirmBox, confirmInput } from './manageStyles';
import { SettingsPage } from './SettingsPage';
import type { Section } from './SettingsPage';

// ArchivedSettings is ONE archived repo's page: a target pinned from the rail's
// Archived group, whose only facet is Settings (the top bar's label carries the
// `archived` chip).
//
// It shows what ArchivedRepo carries and nothing it does not: when it was
// archived, its origin, its size on disk and why it was archived — then
// Restore, and Purge in the danger zone. The mock's store path, last commit and
// fact count are not in the API, and an archived repo's description is not
// readable (its store is closed), so those are omitted rather than invented.
export function ArchivedSettings({ info, serverReadOnly, activeNames, onRestored, onPurged, onError }: {
  info: ArchivedRepo;
  /** The instance accepts no mutations: Restore and Purge are disabled. */
  serverReadOnly: boolean;
  /** Names of the active repos: restoring under a taken name asks for a new one. */
  activeNames: Set<string>;
  onRestored: (name: string) => void;
  onPurged: () => void;
  onError: (m: string) => void;
}) {
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState<'restore' | 'purge' | null>(null);
  const [renameTo, setRenameTo] = useState('');
  const [purgeText, setPurgeText] = useState('');

  const nameTaken = activeNames.has(info.name);
  const size = formatBytes(info.sizeBytes);
  const off = serverReadOnly || busy;

  const beginRestore = () => {
    if (nameTaken) { setRenameTo(''); setConfirming('restore'); return; }
    void doRestore('');
  };
  const doRestore = async (newName: string) => {
    onError(''); setBusy(true);
    try {
      const res = await api.restoreRepo(info.id, newName);
      onRestored(res.name);
    } catch (e) { onError(`restore failed: ${String(e)}`); }
    finally { setBusy(false); setConfirming(null); }
  };
  const doPurge = async () => {
    onError(''); setBusy(true);
    try { await api.purgeRepo(info.id); onPurged(); }
    catch (e) { onError(`purge failed: ${String(e)}`); }
    finally { setBusy(false); setConfirming(null); }
  };

  const archivedOn = new Date(info.archivedAt).toLocaleString();
  const sections: Section[] = [
    {
      id: 'archived',
      title: 'Archived',
      body: (
        <div data-testid={`archived-body-${info.id}`} style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
          <div style={lede}>
            Archived {info.reason?.source === 'system' ? 'automatically ' : ''}{archivedOn}. The store is kept on disk but
            not indexed, synced, or offered to agents.
          </div>
          <dl style={fields}>
            <dt style={fieldKey}>Origin</dt>
            <dd style={{ ...fieldVal, color: info.origin ? '#bbb' : '#666' }}>{info.origin || 'none'}</dd>
            {/* What purging this would give back. An archived database keeps
                its full size on disk under a filename derived from its uid, so
                there is no directory the user could have looked in to work this
                out — which is why the server sends it. */}
            {size && <>
              <dt style={fieldKey}>On disk</dt>
              <dd data-testid={`archived-size-${info.id}`} style={fieldVal}>{size}</dd>
            </>}
          </dl>
          <ArchiveReasonLine info={info} />
          {confirming === 'restore' ? (
            <div style={confirmBox}>
              <div style={{ fontSize: 13, marginBottom: 8 }}>“{info.name}” is already active. Restore under a new name:</div>
              <input autoFocus data-testid={`restore-name-input-${info.id}`} style={confirmInput} value={renameTo} placeholder="new repo name"
                onChange={e => setRenameTo(e.target.value)} />
              <div style={{ display: 'flex', gap: 8, marginTop: 10 }}>
                <button type="button" data-testid={`restore-confirm-${info.id}`} style={btn(busy || !renameTo, 'primary')} disabled={busy || !renameTo} onClick={() => doRestore(renameTo)}>Restore</button>
                <button type="button" style={btn(busy)} disabled={busy} onClick={() => setConfirming(null)}>Cancel</button>
              </div>
            </div>
          ) : (
            <div>
              <button type="button" data-testid={`archived-restore-${info.id}`} style={btn(off, 'primary')} disabled={off} onClick={beginRestore}>Restore</button>
            </div>
          )}
        </div>
      ),
    },
    {
      id: 'danger',
      title: 'Danger zone',
      danger: true,
      body: (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
          <div style={lede}>Purging deletes the store from disk. The remote, if any, is untouched.</div>
          {confirming === 'purge' ? (
            <div style={confirmBox}>
              <div style={{ fontSize: 13, marginBottom: 8, color: '#f88' }}>
                This permanently deletes the archived repo and its history. Type <b>{info.name}</b> to confirm:
              </div>
              <input autoFocus data-testid={`purge-confirm-input-${info.id}`} style={confirmInput} value={purgeText} placeholder={info.name}
                onChange={e => setPurgeText(e.target.value)} />
              <div style={{ display: 'flex', gap: 8, marginTop: 10 }}>
                <button type="button" data-testid={`purge-confirm-${info.id}`} style={btn(busy || purgeText !== info.name, 'danger')} disabled={busy || purgeText !== info.name} onClick={doPurge}>Confirm purge</button>
                <button type="button" style={btn(busy)} disabled={busy} onClick={() => setConfirming(null)}>Cancel</button>
              </div>
            </div>
          ) : (
            <div>
              <button type="button" data-testid={`archived-purge-${info.id}`} style={btn(off, 'danger')} disabled={off}
                onClick={() => { setPurgeText(''); setConfirming('purge'); }}>Purge {info.name}…</button>
            </div>
          )}
        </div>
      ),
    },
  ];

  return <SettingsPage kind="repo" sections={sections} testid="archived-settings" />;
}

// ArchiveReasonLine says why an archived repo is in the archive: by the user
// (with their note), by knomit itself (with the concrete why — for a
// symlinked ontology the same text a restore will refuse with), or, for one
// archived before reasons were recorded, a neutral "No reason recorded".
// The server composes the wording (reason.summary); this only lays it out.
function ArchiveReasonLine({ info }: { info: ArchivedRepo }) {
  const why = info.reason;
  if (why === undefined) return null; // an older server: say nothing rather than guess
  const text = why === null ? 'No reason recorded' : why.summary;
  return (
    <div data-testid={`archived-reason-${info.id}`} style={{ fontSize: 12.5, lineHeight: 1.6 }}>
      <span style={{ color: why === null ? '#6a6a6a' : '#b8b8b8' }}>{text}</span>
      {why && why.condition && why.condition !== why.reason && (
        <div data-testid={`archived-condition-${info.id}`} style={{ fontSize: 11.5, color: '#777', fontFamily: 'var(--k-font-mono)' }}>
          state when archived: {why.condition}
        </div>
      )}
      {why?.source === 'system' && why.condition && (
        <div style={{ fontSize: 11.5, color: '#777' }}>
          Restoring re-runs the check that refused it, so it is refused again until the cause is fixed;
          purge it and add the repository again to start over.
        </div>
      )}
    </div>
  );
}

const lede: CSSProperties = { fontSize: 12, color: '#777', lineHeight: 1.6 };
const fields: CSSProperties = {
  display: 'grid', gridTemplateColumns: 'max-content 1fr', columnGap: 24, rowGap: 4, margin: 0, fontSize: 12.5,
};
const fieldKey: CSSProperties = { color: '#888', margin: 0 };
const fieldVal: CSSProperties = { margin: 0, fontFamily: 'var(--k-font-mono)', fontSize: 11.5, color: '#bbb', overflowWrap: 'anywhere' };
