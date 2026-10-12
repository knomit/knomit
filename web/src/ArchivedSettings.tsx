import { useState } from 'react';
import { api, type ArchivedRepo } from './api';
import { formatBytes } from './utils';
import { ArchiveIcon } from './icons';
import { btn, confirmBox, confirmInput } from './manageStyles';
import { SettingsPage } from './SettingsPage';
import type { Section } from './SettingsPage';
import { detailHead } from './settingsStyles';

// ArchivedSettings is every archived repo on ONE page, with the contents rail as
// its index. An archived repo carries three lines — when it was archived, what
// its origin was, and two buttons — so a rail entry each would have been a
// click that buys almost nothing, and a detail pane per repo would have been
// mostly empty. Restoring one is also usually a comparison ("which of these two
// was it?"), which a list answers and a pane cannot.
export function ArchivedSettings({ archived, readOnly, activeNames, onRestored, onPurged, onError }: {
  archived: ArchivedRepo[]; readOnly: boolean; activeNames: Set<string>;
  onRestored: (name: string) => void; onPurged: () => void; onError: (m: string) => void;
}) {
  const sections: Section[] = archived.map(info => ({
    id: `archived-${info.id}`,
    title: info.name,
    hint: `archived ${info.reason?.source === 'system' ? 'automatically ' : ''}${new Date(info.archivedAt).toLocaleString()}`,
    body: (
      <ArchivedDetail
        key={info.id}
        info={info}
        readOnly={readOnly}
        activeNames={activeNames}
        onRestored={onRestored}
        onPurged={onPurged}
        onError={onError}
      />
    ),
  }));

  return (
    <div>
      <div style={detailHead}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, minWidth: 0 }}>
          <span style={archIconBox}><ArchiveIcon color="#a08c6a" size={16} /></span>
          <div style={{ minWidth: 0 }}>
            <h3 style={{ margin: 0, fontSize: 16 }}>Archived</h3>
            <div style={{ fontSize: 12, color: '#777', marginTop: 1 }}>
              {archived.length} repositor{archived.length === 1 ? 'y' : 'ies'} · restorable, nothing is deleted
            </div>
          </div>
        </div>
        {/* No Browse: an archived repo is not a surface you can read. */}
      </div>
      <SettingsPage sections={sections} testid="archived-settings" />
    </div>
  );
}

function ArchivedDetail({ info, readOnly, activeNames, onRestored, onPurged, onError }: {
  info: ArchivedRepo; readOnly: boolean; activeNames: Set<string>;
  onRestored: (name: string) => void; onPurged: () => void; onError: (m: string) => void;
}) {
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState<'restore' | 'purge' | null>(null);
  const [renameTo, setRenameTo] = useState('');
  const [purgeText, setPurgeText] = useState('');

  const nameTaken = activeNames.has(info.name);
  const size = formatBytes(info.sizeBytes);

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

  return (
    <div data-testid={`archived-body-${info.id}`}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
        <span style={{ fontSize: 12.5, color: '#8a8a8a' }}>
          origin <span style={{ fontFamily: 'var(--k-font-mono)', fontSize: 11.5, color: info.origin ? '#9aa' : '#666' }}>{info.origin || 'none'}</span>
        </span>
        {/* What purging this would give back. An archived database keeps its
            full size on disk under a filename derived from its uid, so there is
            no directory the user could have looked in to work this out — which
            is why the server sends it and why it belongs beside the Purge
            button rather than on a page nobody opens. */}
        {size && (
          <span data-testid={`archived-size-${info.id}`} style={{ fontSize: 12.5, color: '#8a8a8a' }}>
            on disk <span style={{ fontFamily: 'var(--k-font-mono)', fontSize: 11.5, color: '#9aa' }}>{size}</span>
          </span>
        )}
        <div style={{ flex: 1 }} />
        {confirming === null && (
          <div style={{ display: 'flex', gap: 8 }}>
            <button type="button" data-testid={`archived-restore-${info.id}`} style={btn(readOnly || busy)} disabled={readOnly || busy} onClick={beginRestore}>Restore</button>
            <button type="button" data-testid={`archived-purge-${info.id}`} style={btn(readOnly || busy, 'danger')} disabled={readOnly || busy} onClick={() => { setPurgeText(''); setConfirming('purge'); }}>Purge…</button>
          </div>
        )}
      </div>

      <ArchiveReasonLine info={info} />

      {confirming === 'restore' && (
        <div style={confirmBox}>
          <div style={{ fontSize: 13, marginBottom: 8 }}>“{info.name}” is already active. Restore under a new name:</div>
          <input autoFocus data-testid={`restore-name-input-${info.id}`} style={confirmInput} value={renameTo} placeholder="new repo name"
            onChange={e => setRenameTo(e.target.value)} />
          <div style={{ display: 'flex', gap: 8, marginTop: 10 }}>
            <button type="button" data-testid={`restore-confirm-${info.id}`} style={btn(busy || !renameTo, 'primary')} disabled={busy || !renameTo} onClick={() => doRestore(renameTo)}>Restore</button>
            <button type="button" style={btn(busy)} disabled={busy} onClick={() => setConfirming(null)}>Cancel</button>
          </div>
        </div>
      )}

      {confirming === 'purge' && (
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
      )}
    </div>
  );
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
    <div data-testid={`archived-reason-${info.id}`} style={{ marginTop: 10, fontSize: 12.5, lineHeight: 1.6 }}>
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

// archIconBox mirrors lensIconBox / repoIconBox in the warm archive tint.
const archIconBox: React.CSSProperties = {
  width: 30, height: 30, borderRadius: 7, flexShrink: 0,
  background: '#211d18', border: '1px solid #453a2a',
  display: 'flex', alignItems: 'center', justifyContent: 'center',
};
