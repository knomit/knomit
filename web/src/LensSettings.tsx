import { useEffect, useRef, useState } from 'react';
import { api, repoAvailable, MAX_LENS_DESCRIPTION_BYTES, type RepoInfo, type Lens, type LensReadRef } from './api';
import { RepoStateChip } from './RepoStateChip';
import { RepoIndexChip } from './RepoIndexChip';
import { LENS } from './utils';
import { PencilIcon } from './icons';
import { btn, card, cardIconBtn, cardLabel, confirmBox, writeCard } from './manageStyles';
import { SettingsPage } from './SettingsPage';
import type { Section } from './SettingsPage';
import { dangerBox, renameInput, writeReadTag, readTag, warnCallout, warnCalloutDot, warnDetail } from './settingsStyles';
import { RepoDot, BranchChip, CheckMark, ConnectBody } from './settingsParts';
import { useDescriptionEditor, DescriptionActions, DescriptionBody } from './DescriptionEditor';

export function LensSettings({ lens: initial, name, repos, readOnly, broken = [], onDeleted, onSaved, onRenamed, onError }: {
  lens?: Lens; name: string; repos: RepoInfo[]; readOnly: boolean;
  /** Mounts with no store (brokenLensMembers). Non-empty only on the page of a
   *  pinned broken lens: a Mounts block heads the page, naming them and the
   *  way out, and the rest of the page is the lens's ordinary settings. */
  broken?: string[];
  onDeleted: () => void; onSaved: () => void;
  // Fired with the NEW name once the server confirms the rename — same
  // contract as RepoSettings's onRenamed. The pane is keyed on `name` by its
  // caller, so this is how the parent learns to stop addressing it by the old
  // one.
  onRenamed: (newName: string) => void;
  onError: (m: string) => void;
}) {
  const [lens, setLens] = useState<Lens | undefined>(initial);
  // Owned here for the same reason as a repo's descEditing: the Note block's
  // controls are in its heading, its editor in its body.
  const [noteEditing, setNoteEditing] = useState(false);
  const noteEditor = useDescriptionEditor({
    markdown: lens?.description ?? '',
    maxBytes: MAX_LENS_DESCRIPTION_BYTES,
    editing: noteEditing,
    onEditing: setNoteEditing,
    onSave: async md => {
      const updated = await api.updateLens(name, { description: md });
      setLens(updated);
      onSaved();
    },
  });
  const [writeBranch, setWriteBranch] = useState('');
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState(false);
  // Rename draft + its typed confirmation. Local to the danger zone; cleared
  // when the pane switches lenses by the same effect that resets editReads.
  const [renameTo, setRenameTo] = useState('');
  const [renameConfirm, setRenameConfirm] = useState('');
  const [renaming, setRenaming] = useState(false);
  // Edit mode: reads maps a mounted read repo → its branch pin ('' = default).
  // The write repo is never a key (it is read implicitly). null = not editing.
  const [editReads, setEditReads] = useState<Record<string, string> | null>(null);
  const [branchNames, setBranchNames] = useState<Record<string, string[]>>({});
  // agentBranches caches each read repo's OWN agent branch, so the per-row
  // dropdown can offer it as the "(default)" option and filter it out of the
  // explicit-pin list — never the write repo's agent branch (a different repo).
  const [agentBranches, setAgentBranches] = useState<Record<string, string>>({});
  const editRef = useRef<HTMLDivElement>(null);

  // Opening the editor reveals content below the mounts card; scroll it in so
  // Save/Cancel are reachable without hunting for them.
  useEffect(() => {
    if (!editReads) return;
    const id = requestAnimationFrame(() => editRef.current?.scrollIntoView({ block: 'nearest', behavior: 'smooth' }));
    return () => cancelAnimationFrame(id);
  }, [editReads]);

  useEffect(() => {
    setLens(initial);
    setEditReads(null);
    setRenameTo(''); setRenameConfirm('');
    // Always refresh the full detail — the list view can omit the description,
    // and getLens returns the canonical reads set.
    let cancelled = false;
    api.getLens(name).then(l => { if (!cancelled) setLens(l); }).catch(() => {});
    return () => { cancelled = true; };
  }, [name, initial]);

  // The write repo's NAME — what this screen labels rows with and what the
  // branch endpoints take. Membership itself is uid-keyed; `save` below is the
  // only place that needs that spelling.
  const write = lens?.write.name ?? '';
  const reads = lens?.reads ?? [];
  // Name → registry uid for every repo this screen can mount, taken from the
  // same listing the rows are drawn from.
  const uidByName = new Map(repos.filter(r => r.uid).map(r => [r.name, r.uid as string]));

  // The write repo's default branch drives the write-target chip and the
  // write row's fallback label when no explicit pin is set.
  useEffect(() => {
    if (!write) return;
    let cancelled = false;
    api.getAgentBranch(write).then(b => { if (!cancelled) setWriteBranch(b); }).catch(() => {});
    return () => { cancelled = true; };
  }, [write]);

  const del = async () => {
    onError(''); setBusy(true);
    try { await api.deleteLens(name); onDeleted(); }
    catch (e) { onError(`delete failed: ${String(e)}`); }
    finally { setBusy(false); setConfirming(false); }
  };

  // Mirrors RepoSettings's rename: the server's detail for a 409 (name taken by
  // another lens, or by a repo — they share one namespace) already tells the
  // user the one true thing to do, so it is surfaced verbatim like every
  // sibling mutation in this file rather than rewritten into invented copy.
  const rename = async () => {
    onError(''); setRenaming(true);
    try {
      const updated = await api.renameLens(name, renameTo);
      setRenameTo(''); setRenameConfirm('');
      // The pane is addressed by name, so it must follow the lens to its new
      // one — leaving it on the old name would show a 404 on the next read.
      onRenamed(updated.name);
    } catch (e) {
      onError(`rename failed: ${String(e)}`);
    } finally {
      setRenaming(false);
    }
  };

  // ── edit mode ──
  // loadBranchData fetches a read repo's selectable branch names AND its own
  // agent branch (the default pin) together, cached so each is fetched once.
  const loadBranchData = (repo: string) => {
    if (branchNames[repo]) return;
    api.listBranchNames(repo).then(names => setBranchNames(prev => ({ ...prev, [repo]: names }))).catch(() => {});
    api.getAgentBranch(repo).then(b => setAgentBranches(prev => ({ ...prev, [repo]: b }))).catch(() => {});
  };
  const beginEdit = () => {
    // Seed from the current reads, dropping the write repo (read implicitly),
    // and preload each mounted repo's branch data for its dropdown.
    const seed: Record<string, string> = {};
    for (const r of reads) if (r.name !== write) { seed[r.name] = r.branch ?? ''; loadBranchData(r.name); }
    setEditReads(seed);
  };
  const toggleRead = (repo: string) => {
    setEditReads(prev => {
      if (!prev) return prev;
      const next = { ...prev };
      if (repo in next) delete next[repo];
      else { next[repo] = ''; loadBranchData(repo); }
      return next;
    });
  };
  const setBranch = (repo: string, branch: string) => {
    setEditReads(prev => (prev ? { ...prev, [repo]: branch } : prev));
  };
  const save = async () => {
    if (!editReads) return;
    onError(''); setBusy(true);
    // Rows are kept by name; the wire takes uids. A repo the listing gave no
    // uid for cannot be identified to the server, and a 400 naming a uid the
    // reader never saw explains nothing — say which repo instead.
    const missing = Object.keys(editReads).filter(repo => repo !== write && !uidByName.get(repo));
    if (missing.length > 0) {
      onError(`save failed: cannot identify ${missing.join(', ')} — reload and try again.`);
      setBusy(false);
      return;
    }
    const readList: LensReadRef[] = Object.entries(editReads)
      .filter(([repo]) => repo !== write)
      .map(([repo, branch]) => {
        const uid = uidByName.get(repo) as string;
        return branch.trim() ? { uid, branch: branch.trim() } : { uid };
      });
    try {
      const updated = await api.updateLens(name, { reads: readList });
      setLens(updated);
      setEditReads(null);
      onSaved();
    } catch (e) { onError(`save failed: ${String(e)}`); }
    finally { setBusy(false); }
  };

  const sections: Section[] = [];

  if (broken.length > 0) {
    const writeBroken = broken.includes(write);
    sections.push({
      id: 'mounts-missing',
      title: 'Mounts',
      problem: true,
      body: (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
          <div style={lede}>Browse and Activity return once every mount has a store.</div>
          <div data-testid="lens-broken-callout" style={warnCallout}>
            <div style={{ display: 'flex', alignItems: 'center', gap: 8, fontWeight: 600, fontSize: 12.5 }}>
              <span style={warnCalloutDot} />
              {broken.length === 1 ? 'A mount has no store' : `${broken.length} mounts have no store`}
            </div>
            {broken.map(b => {
              const r = repos.find(x => x.name === b);
              return (
                <div key={b} data-testid={`lens-broken-mount-${b}`} style={warnDetail}>
                  {b}{r?.state ? ` — ${r.state}` : ''}{r?.detail ? `: ${r.detail}` : ''}
                </div>
              );
            })}
          </div>
          <div style={lede}>
            {writeBroken
              ? `${write} is the write target, which a lens cannot change: put its store back and restart knomit, or delete this lens below.`
              : 'Every read under this lens fails until then. Remove the mount under Read mounts (the pencil), or put its store back and restart knomit. A repository a lens mounts cannot be archived, so this page is the way out.'}
          </div>
        </div>
      ),
    });
  }

  if (lens?.description || !readOnly) {
    sections.push({
      id: 'note',
      title: 'Note',
      hint: `saved with the lens · up to ${Math.round(MAX_LENS_DESCRIPTION_BYTES / 1024)} KiB`,
      action: readOnly ? undefined : <DescriptionActions editor={noteEditor} label="Edit note" />,
      body: <DescriptionBody editor={noteEditor} readOnly={readOnly} />,
    });
  }

  // Write target — the one repo new facts land in. Same green card as a repo's
  // Agent branch: both answer "where do new facts go".
  sections.push({
    id: 'write-target',
    title: 'Write target',
    hint: 'every fact written through this lens lands here',
    body: (
      <div style={writeCard}>
        <div data-testid="lens-detail-write" style={{ display: 'flex', alignItems: 'center', gap: 8, fontSize: 13, flexWrap: 'wrap' }}>
          <RepoDot repo={write} />
          <b style={{ color: '#eee' }}>{write || '…'}</b>
          <BranchChip branch={reads.find(r => r.name === write)?.branch || writeBranch || 'agent branch'} />
        </div>
        <div style={{ fontSize: 11.5, color: '#777', marginTop: 6 }}>
          Fixed when the lens was created — a lens that changed where it writes would strand its own history.
        </div>
      </div>
    ),
  });

  const readsLocked = readOnly || busy || !!editReads;

  sections.push({
    id: 'read-mounts',
    title: 'Read mounts',
    hint: 'the union an agent sees, resolved top → bottom',
    tail: <span style={{ fontSize: 10.5, color: '#6a6a6a' }}>{reads.length}</span>,
    action: (
      <button type="button" className="k-bare" data-testid="lens-edit"
        title="Edit read mounts" aria-label="Edit read mounts"
        style={cardIconBtn(readsLocked)} disabled={readsLocked} onClick={beginEdit}>
        <PencilIcon color={readsLocked ? '#555' : '#888'} size={13} />
      </button>
    ),
    body: (
      <>
        {/* The union, in server order. The write repo shows here (tagged
            write · read), never as a separate line.

            Rows are NUMBERED because resolution order is load-bearing — the
            union resolves top to bottom and the first mount holding a path
            wins — and nothing in this UI said so before. Every row carries its
            branch, not only the pinned ones, so "follows the agent branch" and
            "pinned to a fixed branch" read as a difference rather than as an
            absence. */}
        <div style={card}>
          {reads.length === 0 && <div style={{ color: '#555', fontSize: 13 }}>None</div>}
          {reads.map((r, i) => {
            const isWrite = r.name === write;
            const pinned = !!r.branch && !isWrite;
            return (
              // Keyed by uid: a rename relabels the row rather than remounting
              // it, and two mounts can never share a uid.
              <div key={r.uid} data-testid={`lens-detail-read-${r.name}`}
                style={{ display: 'flex', alignItems: 'center', gap: 9, padding: '8px 2px', borderBottom: i === reads.length - 1 ? 'none' : '1px solid #242424' }}>
                <span style={mountOrdinal}>{i + 1}</span>
                <RepoDot repo={r.name} />
                <span style={{ fontSize: 13, color: '#eee', minWidth: 70 }}>{r.name}</span>
                <BranchChip branch={r.branch || (isWrite ? writeBranch : '') || 'agent branch'} />
                <div style={{ flex: 1 }} />
                {isWrite
                  ? <span style={writeReadTag}>write · read</span>
                  : <span style={readTag}>{pinned ? 'read · pinned' : 'read'}</span>}
              </div>
            );
          })}
        </div>
        {reads.some(r => r.branch && r.name !== write) && (
          <div style={{ fontSize: 11.5, color: '#777', marginTop: 8 }}>
            A pinned mount reads a fixed branch and never follows that repo's agent branch — the only way a lens shows stale content on purpose.
          </div>
        )}
        {/* Edit mode expands directly under the mounts it edits, inside their
            block, so the card you are changing stays in view above the editor.
            It is pulled into view so its Save/Cancel are never left below the fold. */}
        {editReads && (
          <div ref={editRef} style={{ ...card, borderColor: LENS.border }}>
            <div style={cardLabel}>Edit read mounts</div>
            {write && (
              <div style={editRow(true)}>
                <span style={editCheckbox(true)}><CheckMark color={LENS.text} /></span>
                <RepoDot repo={write} />
                <span style={{ fontSize: 13, color: '#eee', minWidth: 76 }}>{write}</span>
                <span style={writeReadTag}>write · always read</span>
              </div>
            )}
            {repos.filter(r => r.name !== write).map(r => {
              const on = r.name in editReads;
              const others = (branchNames[r.name] ?? []).filter(n => n !== agentBranches[r.name]);
              // A repo with no live store cannot be MOUNTED — the server answers
              // 422 `repo not found: "<ksuid>"`, naming a uid the reader has
              // never seen — so its checkbox is disabled while it is off.
              //
              // But it is still RENDERED, and still toggleable while it is ON,
              // and that asymmetry is the whole point. beginEdit seeds editReads
              // from the lens's current reads by name and save re-sends the WHOLE
              // set, so filtering a broken repo out of this list would hide the
              // one row that has to be unchecked: the mount that is breaking the
              // lens. Every save would then re-send it and 422 forever, with no
              // control on screen capable of repairing it.
              const mountable = repoAvailable(r);
              const toggleable = mountable || on;
              return (
                <div key={r.name} style={{ ...editRow(on), opacity: toggleable ? 1 : 0.6 }}>
                  <button type="button" data-testid={`lens-read-${r.name}`} style={editCheckbox(on)}
                    disabled={busy || !toggleable}
                    title={toggleable ? undefined : r.detail || `${r.name} has no store (${r.state}) and cannot be mounted.`}
                    onClick={() => toggleRead(r.name)} aria-label={r.name} aria-pressed={on}>
                    {on && <CheckMark color={LENS.text} />}
                  </button>
                  <RepoDot repo={r.name} />
                  <span style={{ fontSize: 13, color: on ? '#eee' : '#aaa', minWidth: 76 }}>{r.name}</span>
                  {!mountable && <RepoStateChip repo={r} />}
                  {mountable && <RepoIndexChip repo={r} />}
                  <div style={{ flex: 1 }} />
                  {on && (
                    <select data-testid={`lens-branch-${r.name}`}
                      style={{ background: '#111', border: '1px solid #333', color: '#eee', padding: '5px 7px', borderRadius: 4, fontSize: 12, maxWidth: 200 }}
                      value={editReads[r.name]} disabled={busy} onChange={e => setBranch(r.name, e.target.value)}>
                      <option value="">agent branch (default)</option>
                      {others.map(n => <option key={n} value={n}>{n}</option>)}
                    </select>
                  )}
                </div>
              );
            })}
            <div style={{ display: 'flex', gap: 8, marginTop: 12 }}>
              <button type="button" data-testid="lens-edit-save" style={btn(busy, 'primary')} disabled={busy} onClick={save}>
                {busy ? 'Saving…' : 'Save mounts'}
              </button>
              <button type="button" data-testid="lens-edit-cancel" style={btn(busy)} disabled={busy} onClick={() => setEditReads(null)}>Cancel</button>
            </div>
          </div>
        )}
      </>
    ),
  });

  sections.push({
    id: 'agent-access',
    title: 'Agent access',
    hint: 'how a client connects to this lens',
    body: <ConnectBody kind="lens" name={name} />,
  });

  sections.push({
    id: 'danger',
    title: 'Danger zone',
    danger: true,
    body: (
      <div style={dangerBox}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
          <span style={{ fontSize: 13, color: '#ddd' }}>Delete this lens</span>
          <div style={{ flex: 1 }} />
          <button type="button" data-testid="lens-delete" style={btn(readOnly || busy, 'danger')} disabled={readOnly || busy} onClick={() => setConfirming(true)}>
            Delete
          </button>
        </div>
        <div style={{ fontSize: 11.5, color: '#777', marginTop: 6 }}>
          The repositories it reads are not affected — only the lens that groups them.
        </div>
        {confirming && (
          <div style={confirmBox}>
            <div style={{ fontSize: 13, marginBottom: 8, color: '#f88' }}>Delete lens “{name}”? The underlying repos are not affected.</div>
            <div style={{ display: 'flex', gap: 8 }}>
              <button type="button" data-testid="lens-delete-confirm" style={btn(busy, 'danger')} disabled={busy} onClick={del}>Confirm delete</button>
              <button type="button" style={btn(busy)} disabled={busy} onClick={() => setConfirming(false)}>Cancel</button>
            </div>
          </div>
        )}
        <div style={{ borderTop: '1px solid #3a2020', marginTop: 14, paddingTop: 14 }}>
          <div style={{ fontSize: 13, color: '#ddd', marginBottom: 4 }}>Rename this lens</div>
          {/* Differs from the repo warning: a lens has no history or mounts of
              its OWN to lose, and its identity (uid) is stable across the
              rename — the MCP binding pin is lens:<uid>, never the name (RFC
              §7.3) — so the one real consequence is the endpoint URL. */}
          <div data-testid="lens-rename-warning" style={{ fontSize: 11.5, color: '#777', marginBottom: 10 }}>
            Agent MCP endpoint URLs contain this lens's name and will stop resolving
            until each agent is reconfigured. The lens keeps its identity, so its
            mounts, and any in-flight agent queries against it, are unaffected.
          </div>
          <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', alignItems: 'center' }}>
            <input
              data-testid="lens-rename-input"
              value={renameTo}
              onChange={e => setRenameTo(e.target.value)}
              placeholder="new-name"
              disabled={readOnly || renaming}
              style={renameInput}
            />
            <input
              data-testid="lens-rename-confirm"
              value={renameConfirm}
              onChange={e => setRenameConfirm(e.target.value)}
              placeholder={`type "${name}" to confirm`}
              disabled={readOnly || renaming}
              style={renameInput}
            />
            <button
              type="button"
              data-testid="lens-rename-submit"
              style={btn(readOnly || renaming || renameConfirm !== name || !renameTo || renameTo === name, 'danger')}
              disabled={readOnly || renaming || renameConfirm !== name || !renameTo || renameTo === name}
              onClick={rename}
            >
              {renaming ? 'Renaming…' : 'Rename'}
            </button>
          </div>
        </div>
      </div>
    ),
  });

  return <SettingsPage kind="lens" sections={sections} testid="lens-settings" />;
}

const mountOrdinal: React.CSSProperties = {
  width: 12, flexShrink: 0, fontFamily: 'var(--k-font-mono)', fontSize: 10, color: '#4a4a4a',
};

// editRow / editCheckbox mirror CreateLensForm's row/checkbox (LENS tokens).
const editRow = (on: boolean): React.CSSProperties => ({
  display: 'flex', alignItems: 'center', gap: 10, padding: '8px 10px', borderRadius: 6, marginBottom: 4,
  background: on ? LENS.soft : '#0f0f0f', border: '1px solid ' + (on ? LENS.border : '#242424'),
});
const editCheckbox = (on: boolean): React.CSSProperties => ({
  width: 16, height: 16, borderRadius: 4, flexShrink: 0, padding: 0,
  display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
  background: on ? LENS.accent : 'transparent', border: '1.5px solid ' + (on ? LENS.accent : '#444'),
  cursor: 'pointer',
});

const lede: React.CSSProperties = { fontSize: 12.5, color: '#888', lineHeight: 1.6 };
