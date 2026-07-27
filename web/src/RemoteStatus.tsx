import { useCallback, useEffect, useState } from 'react';
import { api, type OriginResponse } from './api';
import { GlobeIcon } from './icons';
import { btn, card, cardLabel, confirmBox, linkBtn } from './manageStyles';

/** RemoteState is the loaded remote for one repo. RepoDetail owns it (via
 *  useRemote) because the detail pane's ⋯ menu has to know whether a remote
 *  exists before it can offer Connect vs Reconnect/Disconnect — the card alone
 *  cannot answer that for the header. */
export interface RemoteState {
  origin: OriginResponse | null;
  loading: boolean;
  err: string;
  setErr: (m: string) => void;
  reload: () => void;
}

/** useRemote loads (and reloads) a repo's origin. `enabled` is false when the
 *  deployment hides remote config — the hook still runs (hooks cannot be
 *  conditional) but issues no request and reports "not connected". */
export function useRemote(repo: string, enabled = true): RemoteState {
  const [origin, setOrigin] = useState<OriginResponse | null>(null);
  const [loading, setLoading] = useState(enabled);
  const [err, setErr] = useState('');
  const [nonce, setNonce] = useState(0);

  useEffect(() => {
    if (!enabled) return;
    let cancelled = false;
    setLoading(true); setErr('');
    api.getOrigin(repo)
      .then(o => { if (!cancelled) setOrigin(o); })
      .catch(() => { if (!cancelled) setErr('could not load remote status'); })
      .finally(() => { if (!cancelled) setLoading(false); });
    return () => { cancelled = true; };
  }, [repo, enabled, nonce]);

  const reload = useCallback(() => setNonce(n => n + 1), []);
  // When disabled, report "not connected, not loading" by derivation rather
  // than by writing state from the effect — nothing was ever fetched.
  if (!enabled) return { origin: null, loading: false, err: '', setErr, reload };
  return { origin, loading, err, setErr, reload };
}

interface Props {
  repo: string;
  agentBranch: string;     // this machine's local agent branch (for the upstream warning)
  readOnly: boolean;
  state: RemoteState;
  onConnect: () => void;   // open the connect wizard
  onChanged: () => void;   // remote changed (e.g. upstream) — parent refresh
}

// RemoteCard is the read-only remote card in the Repo Manager detail pane. It
// never edits the remote inline except for the upstream branch (an edit of a
// value it already displays); connecting/changing goes through the wizard and
// Disconnect lives in RepoDetail's ⋯ menu alongside the other repo actions.
export function RemoteCard({ repo, agentBranch, readOnly, state, onConnect, onChanged }: Props) {
  const { origin, loading, err, setErr, reload } = state;
  const [busy, setBusy] = useState(false);
  const [editingUpstream, setEditingUpstream] = useState(false);
  const [branchChoices, setBranchChoices] = useState<string[]>([]);
  const [newUpstream, setNewUpstream] = useState('');

  // No reset-on-repo-switch effect is needed: RepoDetail is keyed by repo name,
  // so switching repos remounts this card and the editor starts closed.

  // upstream == this machine's agent branch is a degenerate config: pulls are
  // disabled (push-only) to avoid force-resetting unpushed facts. Warn + offer
  // a one-click switch to a real consensus branch.
  const upstreamIsAgent = !!origin && !!agentBranch && origin.branch === agentBranch;

  const openUpstreamEditor = async () => {
    setErr('');
    setNewUpstream('main');
    setEditingUpstream(true);
    try {
      const names = await api.listBranchNames(repo);
      // Offer real consensus candidates first; never offer the agent branch itself.
      setBranchChoices(names.filter(n => n !== agentBranch));
    } catch { setBranchChoices([]); }
  };

  const saveUpstream = async () => {
    if (!newUpstream) return;
    setErr(''); setBusy(true);
    try {
      await api.setOriginUpstream(repo, newUpstream);
      setEditingUpstream(false);
      reload();
      onChanged();
    } catch (e) { setErr(String(e)); }
    finally { setBusy(false); }
  };

  return (
    <div style={card}>
      <div style={{ ...cardLabel, display: 'flex', alignItems: 'center', gap: 6 }}>
        <GlobeIcon color="#555" size={11} /> Remote
      </div>

      {loading && <div style={muted}>Loading…</div>}

      {!loading && !origin && (
        <>
          <div style={muted}>Not connected to a remote — this is a local-only knowledge base.</div>
          <button type="button" data-testid="remote-connect" style={btn(readOnly, 'primary')} disabled={readOnly} onClick={onConnect}>
            Connect a remote…
          </button>
        </>
      )}

      {!loading && origin && (
        <>
          <div style={{ fontSize: 13, color: '#ddd', wordBreak: 'break-all' }}>{origin.url}</div>
          <div style={{ fontSize: 12, color: upstreamIsAgent ? '#e0a23a' : '#888', marginTop: 2, display: 'flex', alignItems: 'center', gap: 6 }}>
            {upstreamIsAgent && <span data-testid="upstream-warning-icon" title="Upstream is this agent branch" aria-label="warning">⚠</span>}
            <span>upstream branch: {origin.branch || 'main'}</span>
            {!readOnly && !editingUpstream && (
              <button type="button" data-testid="upstream-change" style={linkBtn} onClick={openUpstreamEditor}>change…</button>
            )}
          </div>

          {/* The degenerate-upstream warning is load-bearing (it explains why
              pulls silently stop) and therefore never lives behind a collapsed
              section — see kb/gotchas/repos/remote-sync/ed75e605.md. */}
          {upstreamIsAgent && !editingUpstream && (
            <div data-testid="upstream-warning" style={warnBox}>
              ⚠ The consensus (“main”) branch is set to this machine’s agent branch, so remote
              changes are <strong>not pulled</strong> — the repo is push-only to protect unpushed
              facts. Set a real consensus branch (e.g. <code>main</code>) to re-enable pulls.
            </div>
          )}

          {editingUpstream && (
            <div style={confirmBox}>
              <div style={{ fontSize: 13, marginBottom: 8 }}>Set the consensus (“main”) branch the remote syncs against:</div>
              <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                <input
                  data-testid="upstream-input"
                  list="upstream-branch-options"
                  value={newUpstream}
                  onChange={e => setNewUpstream(e.target.value)}
                  placeholder="main"
                  style={{ background: '#111', color: '#eee', border: '1px solid #333', borderRadius: 4, padding: '6px 8px', fontSize: 13, minWidth: 160 }}
                />
                <datalist id="upstream-branch-options">
                  {branchChoices.map(b => <option key={b} value={b} />)}
                </datalist>
                <button type="button" data-testid="upstream-save" style={btn(busy || !newUpstream, 'primary')} disabled={busy || !newUpstream} onClick={saveUpstream}>{busy ? 'Saving…' : 'Save'}</button>
                <button type="button" style={btn(busy)} disabled={busy} onClick={() => setEditingUpstream(false)}>Cancel</button>
              </div>
            </div>
          )}

          <SyncLine o={origin} />
          <PushLine o={origin} />
        </>
      )}

      {err && <div style={{ color: '#f88', fontSize: 13, marginTop: 8 }}>{err}</div>}
    </div>
  );
}

function SyncLine({ o }: { o: OriginResponse }) {
  let text = 'never synced yet';
  let color = '#888';
  // The backend persists status as "error" / "ok" (see updateRemoteStatus).
  if (o.last_status === 'error') {
    text = `✗ sync failed${o.last_error ? ' — ' + o.last_error : ''}`;
    color = '#f88';
  } else if (o.last_sync_at) {
    text = `✓ last sync ${new Date(o.last_sync_at).toLocaleString()}`;
    color = '#9c9';
  }
  return <div data-testid="sync-line" style={{ fontSize: 12, color, marginTop: 6 }}>{text}</div>;
}

function PushLine({ o }: { o: OriginResponse }) {
  // Push runs only when not pull-only; show a line whenever we have any push
  // outcome. A failed push (e.g. an expired/again-denied token) was previously
  // invisible — there was no push line at all.
  if (o.last_push_status === 'error') {
    return (
      <div data-testid="push-line" style={{ fontSize: 12, color: '#f88', marginTop: 4 }}>
        {`✗ push failed${o.last_push_error ? ' — ' + o.last_push_error : ''}`}
      </div>
    );
  }
  if (o.last_push_at) {
    return (
      <div data-testid="push-line" style={{ fontSize: 12, color: '#9c9', marginTop: 4 }}>
        {`✓ last push ${new Date(o.last_push_at).toLocaleString()}`}
      </div>
    );
  }
  return null;
}

const muted: React.CSSProperties = { fontSize: 13, color: '#888', marginBottom: 10 };
const warnBox: React.CSSProperties = { marginTop: 8, padding: 10, background: '#2a210e', border: '1px solid #5c4a1a', borderRadius: 6, fontSize: 12, color: '#e8c98a', lineHeight: 1.5 };
