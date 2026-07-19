import { useEffect, useState } from 'react';
import { api, type RepoInfo, type Lens, type LensRead } from './api';
import { LENS, repoHue } from './utils';
import { LayersIcon, GitBranchIcon } from './icons';

// BranchData is the per-repo branch picker state: the existing branch names,
// the repo's agent branch (shown as the default), and load status.
interface BranchData { names: string[]; agent: string; loading: boolean; error: boolean }

// A small inline checkmark for the filled checkbox — icons.tsx has no plain
// check glyph, and this is the only place that needs one.
const Check = ({ color }: { color: string }) => (
  <svg width={11} height={11} viewBox="0 0 24 24" fill="none" stroke={color} strokeWidth="3" strokeLinecap="round" strokeLinejoin="round">
    <polyline points="20 6 9 17 4 12" />
  </svg>
);

// Dot is the deterministic per-repo hue swatch shown beside each repo name.
const Dot = ({ repo }: { repo: string }) => (
  <span style={{ width: 7, height: 7, borderRadius: '50%', background: repoHue(repo), flexShrink: 0 }} />
);

// CreateLensForm composes a lens: a name, one write repo (facts land here), and
// any number of read repos (each optionally pinned to a branch). The write repo
// is implicitly also a read mount on the server, so it renders as a pinned,
// always-on first row (locked to its agent branch) — never toggled into `reads`.
export function CreateLensForm({ repos, lenses = [], onDone, onError, onCancel }: {
  repos: RepoInfo[];
  lenses?: Lens[];
  onDone: (name: string) => void;
  onError: (m: string) => void;
  onCancel?: () => void;
}) {
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [write, setWrite] = useState(repos[0]?.name ?? '');
  // reads maps a toggled read repo → its branch pin. '' means "the repo's agent
  // branch, resolved at bind time" (the default); a non-empty value is an
  // explicit pin. The write repo is never a key here (it's always read).
  const [reads, setReads] = useState<Record<string, string>>({});
  // branchData caches each repo's selectable branches (the picker can't create
  // branches, only offer existing ones). `agent` is the default choice.
  const [branchData, setBranchData] = useState<Record<string, BranchData>>({});
  const [search, setSearch] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');

  // loadBranches fetches the existing branches and agent branch for a repo, so
  // the dropdown (or the write row's locked label) reflects only real branches.
  const loadBranches = async (repo: string) => {
    if (!repo) return;
    setBranchData(prev => ({ ...prev, [repo]: { names: [], agent: '', loading: true, error: false } }));
    try {
      const [names, agent] = await Promise.all([api.listBranchNames(repo), api.getAgentBranch(repo)]);
      setBranchData(prev => ({ ...prev, [repo]: { names, agent, loading: false, error: false } }));
    } catch {
      setBranchData(prev => ({ ...prev, [repo]: { names: [], agent: '', loading: false, error: true } }));
    }
  };

  // The write repo's branch is shown locked to its agent branch, so load it
  // whenever the write target changes (once — the cache persists).
  useEffect(() => {
    if (write && !branchData[write]) void loadBranches(write);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [write]);

  const changeWrite = (next: string) => {
    setWrite(next);
    // A repo that becomes the write target is read implicitly — drop any
    // explicit read pin so it isn't double-counted or sent in `reads`.
    setReads(prev => {
      if (!(next in prev)) return prev;
      const n = { ...prev }; delete n[next]; return n;
    });
  };

  const toggleRead = (repo: string) => {
    setReads(prev => {
      const next = { ...prev };
      if (repo in next) delete next[repo];
      else next[repo] = '';
      return next;
    });
    if (!(repo in reads) && !branchData[repo]) void loadBranches(repo);
  };

  const setBranch = (repo: string, branch: string) => {
    setReads(prev => ({ ...prev, [repo]: branch }));
  };

  // The toggleable read repos are every repo except the write target. When a
  // search filter is active, "select all" is scoped to the *visible* rows
  // (select-all-visible) — it never mounts repos the user filtered out of view,
  // nor clears hidden selections.
  const others = repos.filter(r => r.name !== write);
  const filteredOthers = search.trim()
    ? others.filter(r => r.name.toLowerCase().includes(search.trim().toLowerCase()))
    : others;
  const allOn = filteredOthers.length > 0 && filteredOthers.every(r => r.name in reads);

  const toggleAll = () => {
    if (allOn) {
      setReads(prev => {
        const n = { ...prev };
        for (const r of filteredOthers) delete n[r.name];
        return n;
      });
      return;
    }
    setReads(prev => {
      const n = { ...prev };
      for (const r of filteredOthers) if (!(r.name in n)) n[r.name] = '';
      return n;
    });
    for (const r of filteredOthers) if (!branchData[r.name]) void loadBranches(r.name);
  };

  // ── live name validation ──
  const nameTrim = name.trim();
  const repoNames = new Set(repos.map(r => r.name));
  const lensNames = new Set(lenses.map(l => l.name));
  const patternOk = /^[a-z0-9_-]+$/.test(nameTrim);
  let nameError = '';
  if (nameTrim === '') nameError = '';
  else if (!patternOk) nameError = 'Use only a–z, 0–9, - and _';
  else if (repoNames.has(nameTrim)) nameError = `A repo named "${nameTrim}" already exists`;
  else if (lensNames.has(nameTrim)) nameError = `A lens named "${nameTrim}" already exists`;
  const nameValid = nameTrim !== '' && nameError === '';

  const submit = async () => {
    setErr(''); onError(''); setBusy(true);
    // Assemble reads: omit an empty branch so the server picks its default;
    // defensively skip the write repo (it's read implicitly).
    const readList: LensRead[] = Object.entries(reads)
      .filter(([repo]) => repo !== write)
      .map(([repo, branch]) => branch.trim() ? { repo, branch: branch.trim() } : { repo });
    const body: { name: string; write: string; reads: LensRead[]; description?: string } =
      { name: nameTrim, write, reads: readList };
    const desc = description.trim();
    if (desc) body.description = desc;
    try {
      await api.createLens(body);
      onDone(nameTrim);
    } catch (e) {
      setErr(String(e));
      onError(String(e));
    } finally {
      setBusy(false);
    }
  };

  const selectedCount = 1 + others.filter(r => r.name in reads).length;
  const readEntries = others.filter(r => r.name in reads);

  return (
    <div>
      <h3 style={{ margin: '0 0 4px', fontSize: 16 }}>New lens</h3>
      <div style={{ fontSize: 12.5, color: '#777', lineHeight: 1.5, marginBottom: 6 }}>
        A lens unions several repos for <b style={{ color: '#bbb' }}>reading</b> and points writes at one of them.
      </div>

      <label style={label}>Name</label>
      <div style={{ position: 'relative' }}>
        <span style={{ position: 'absolute', left: 8, top: '50%', transform: 'translateY(-50%)', pointerEvents: 'none', display: 'flex' }}>
          <LayersIcon color={LENS.accent} size={13} />
        </span>
        <input data-testid="lens-name" style={{ ...input, paddingLeft: 28 }} placeholder="e.g. dev (a–z, 0–9, -, _)"
          value={name} disabled={busy} onChange={e => setName(e.target.value)} />
      </div>
      <div data-testid="lens-name-status" style={{ ...hint, color: nameError ? '#f88' : nameValid ? '#7c9' : '#666', display: 'flex', alignItems: 'center', gap: 5 }}>
        {nameError
          ? nameError
          : nameValid
            ? <><Check color="#7c9" /> available · a–z, 0–9, -, _</>
            : 'a–z, 0–9, - and _'}
      </div>

      <label style={label}>Description <span style={{ color: '#555' }}>(optional)</span></label>
      <textarea data-testid="lens-description" style={{ ...input, minHeight: 44, resize: 'vertical', fontFamily: 'inherit' }}
        placeholder="What is this lens for?" value={description} disabled={busy}
        onChange={e => setDescription(e.target.value)} />

      <label style={label}>Write target</label>
      <select data-testid="lens-write" style={input} value={write} disabled={busy}
        onChange={e => changeWrite(e.target.value)}>
        {repos.map(r => <option key={r.name} value={r.name}>{r.name}</option>)}
      </select>
      <div style={hint}>Every new fact written through this lens lands on <b style={{ color: '#bbb' }}>{write || '…'}</b>'s agent branch.</div>

      <div style={{ display: 'flex', alignItems: 'baseline', justifyContent: 'space-between', marginTop: 16 }}>
        <label style={{ ...label, marginTop: 0 }}>Read mounts</label>
        <span style={{ fontSize: 11, color: '#777' }}>
          {selectedCount} of {repos.length} · <span data-testid="lens-select-all" style={{ color: LENS.accent, cursor: 'pointer' }} onClick={toggleAll}>select all</span>
        </span>
      </div>

      {repos.length > 8 && (
        <input data-testid="lens-read-search" style={{ ...input, marginTop: 4 }} placeholder="Filter repos…"
          value={search} disabled={busy} onChange={e => setSearch(e.target.value)} />
      )}

      <div style={{ marginTop: 6 }}>
        {/* Pinned write-as-read row: always on, locked to the agent branch. */}
        {write && (
          <div style={row(true)}>
            <button type="button" data-testid={`lens-read-${write}`} style={checkbox(true)} disabled aria-label={`${write} (write repo, always read)`}>
              <Check color={LENS.text} />
            </button>
            <Dot repo={write} />
            <span style={{ fontSize: 13, color: '#eee', minWidth: 76 }}>{write}</span>
            <span style={writeBadge}>WRITE · always read</span>
            <div style={{ flex: 1 }} />
            <span style={{ display: 'flex', alignItems: 'center', gap: 5, fontSize: 12, color: '#8af' }}>
              <GitBranchIcon color="#8af" size={12} />
              <span style={{ fontFamily: 'var(--k-font-mono)', fontSize: 11, color: '#8af' }}>
                {branchData[write]?.agent || (branchData[write]?.loading ? '…' : 'agent branch')}
              </span>
            </span>
          </div>
        )}

        {/* Toggleable read rows. */}
        {filteredOthers.map(r => {
          const on = r.name in reads;
          const bd = branchData[r.name];
          const agentLabel = bd?.agent ? `Agent branch — ${bd.agent}` : 'Agent branch';
          const otherBranches = (bd?.names ?? []).filter(n => n !== bd?.agent);
          return (
            <div key={r.name} style={row(on)}>
              <button type="button" data-testid={`lens-read-${r.name}`} style={checkbox(on)} disabled={busy}
                onClick={() => toggleRead(r.name)} aria-label={r.name} aria-pressed={on}>
                {on && <Check color={LENS.text} />}
              </button>
              <Dot repo={r.name} />
              <span style={{ fontSize: 13, color: on ? '#eee' : '#aaa', minWidth: 76 }}>{r.name}</span>
              <div style={{ flex: 1 }} />
              {on && (
                <select data-testid={`lens-branch-${r.name}`} style={{ ...input, width: 'auto', flexShrink: 0, marginTop: 0, maxWidth: 200 }}
                  value={reads[r.name]} disabled={busy || bd?.loading}
                  onChange={e => setBranch(r.name, e.target.value)}>
                  <option value="">{bd?.loading ? 'loading branches…' : `${agentLabel} (default)`}</option>
                  {otherBranches.map(n => <option key={n} value={n}>{n}</option>)}
                </select>
              )}
            </div>
          );
        })}
      </div>

      {/* Preview: the resolved read union + write target, repo names hue-colored. */}
      <div data-testid="lens-preview" style={previewBox}>
        <div style={{ color: '#888', textTransform: 'uppercase', fontSize: 10, letterSpacing: 1, marginBottom: 2 }}>Preview</div>
        <div>
          Reads union <b style={{ color: repoHue(write) }}>{write || '…'}</b>
          {readEntries.map(r => (
            <span key={r.name}>
              {' + '}
              <b style={{ color: repoHue(r.name) }}>{r.name}</b>
              {reads[r.name] && <span style={{ fontFamily: 'var(--k-font-mono)', color: '#888' }}>@{reads[r.name]}</span>}
            </span>
          ))}
          . Writes → <b style={{ color: repoHue(write) }}>{write || '…'}</b>.
        </div>
      </div>

      {err && <div style={{ color: '#f88', fontSize: 13, marginTop: 8 }}>{err}</div>}

      <div style={{ display: 'flex', gap: 8, marginTop: 16 }}>
        <button type="button" data-testid="lens-create" style={btn(busy || !nameValid || !write, 'primary')}
          disabled={busy || !nameValid || !write} onClick={submit}>
          {busy ? 'Creating…' : 'Create lens'}
        </button>
        <button type="button" data-testid="lens-cancel" style={btn(busy)} disabled={busy} onClick={() => onCancel?.()}>
          Cancel
        </button>
      </div>
    </div>
  );
}

const label: React.CSSProperties = { fontSize: 12, color: '#888', marginBottom: 4, marginTop: 12, display: 'block' };
const hint: React.CSSProperties = { fontSize: 12, color: '#666', marginTop: 4 };
const input: React.CSSProperties = { width: '100%', boxSizing: 'border-box', background: '#111', border: '1px solid #333', color: '#eee', padding: '6px 8px', borderRadius: 4, fontSize: 13 };
const writeBadge: React.CSSProperties = { fontSize: 10, color: '#7c9', background: '#1a2e1a', border: '1px solid #2a4a2a', padding: '1px 6px', borderRadius: 3, letterSpacing: '0.03em' };
const previewBox: React.CSSProperties = { marginTop: 12, padding: '9px 11px', background: '#0f0f0f', border: '1px solid #242424', borderRadius: 6, fontSize: 12, color: '#aaa', lineHeight: 1.6 };

// row: one read-mount row — filled when on, deep/inert when off.
const row = (on: boolean): React.CSSProperties => ({
  display: 'flex', alignItems: 'center', gap: 10, padding: '8px 10px', borderRadius: 6, marginBottom: 4,
  background: on ? LENS.soft : '#0f0f0f', border: '1px solid ' + (on ? LENS.border : '#242424'),
});
// checkbox: 16×16 box, filled with the lens accent (and a light check) when on.
const checkbox = (on: boolean): React.CSSProperties => ({
  width: 16, height: 16, borderRadius: 4, flexShrink: 0, padding: 0,
  display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
  background: on ? LENS.accent : 'transparent', border: '1.5px solid ' + (on ? LENS.accent : '#444'),
  cursor: 'pointer',
});
const btn = (disabled: boolean, variant: 'primary' | 'secondary' = 'secondary'): React.CSSProperties => ({ background: disabled ? '#222' : variant === 'primary' ? '#1d4ed8' : '#2a2a2a', color: disabled ? '#666' : '#eee', border: '1px solid #333', borderRadius: 4, padding: '6px 14px', fontSize: 13, cursor: disabled ? 'default' : 'pointer' });
