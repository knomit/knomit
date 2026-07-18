import { useState } from 'react';
import { api, type RepoInfo, type LensRead } from './api';

// BranchData is the per-read-repo branch picker state: the existing branch
// names, the repo's agent branch (shown as the default), and load status.
interface BranchData { names: string[]; agent: string; loading: boolean; error: boolean }

// CreateLensForm composes a lens: a name, one write repo (facts land here),
// and any number of read repos (each optionally pinned to a branch). Mirrors
// CreateRepoForm's busy/error handling. The write repo is implicitly also a
// read mount on the server, so we neither force it into `reads` nor forbid it.
export function CreateLensForm({ repos, onDone, onError }: {
  repos: RepoInfo[];
  onDone: (name: string) => void;
  onError: (m: string) => void;
}) {
  const [name, setName] = useState('');
  const [write, setWrite] = useState(repos[0]?.name ?? '');
  // reads maps a toggled repo name → its branch pin. '' means "the repo's agent
  // branch, resolved at bind time" (see repos.LensRead) — the default. A
  // non-empty value is an explicit pin to an existing branch.
  const [reads, setReads] = useState<Record<string, string>>({});
  // branchData caches each toggled repo's selectable branches. We can't create
  // branches on the fly, so the picker only offers branches that already exist;
  // `agent` is that repo's agent branch, shown as the default choice.
  const [branchData, setBranchData] = useState<Record<string, BranchData>>({});
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');

  // loadBranches fetches the existing branches and agent branch for a repo the
  // moment it is toggled on, so the dropdown reflects only readable branches.
  const loadBranches = async (repo: string) => {
    setBranchData(prev => ({ ...prev, [repo]: { names: [], agent: '', loading: true, error: false } }));
    try {
      const [names, agent] = await Promise.all([api.listBranchNames(repo), api.getAgentBranch(repo)]);
      setBranchData(prev => ({ ...prev, [repo]: { names, agent, loading: false, error: false } }));
    } catch {
      setBranchData(prev => ({ ...prev, [repo]: { names: [], agent: '', loading: false, error: true } }));
    }
  };

  const toggleRead = (repo: string) => {
    setReads(prev => {
      const next = { ...prev };
      if (repo in next) delete next[repo];
      else next[repo] = '';
      return next;
    });
    // Fetch on toggle-on (once); the branch cache persists across re-toggles.
    if (!(repo in reads) && !branchData[repo]) void loadBranches(repo);
  };

  const setBranch = (repo: string, branch: string) => {
    setReads(prev => ({ ...prev, [repo]: branch }));
  };

  const submit = async () => {
    setErr(''); onError(''); setBusy(true);
    // Assemble reads: omit an empty branch so the server picks its default.
    const readList: LensRead[] = Object.entries(reads).map(([repo, branch]) =>
      branch.trim() ? { repo, branch: branch.trim() } : { repo });
    try {
      await api.createLens({ name, write, reads: readList });
      onDone(name);
    } catch (e) {
      setErr(String(e));
      onError(String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div>
      <h3 style={{ margin: '0 0 14px', fontSize: 16 }}>New lens</h3>

      <label style={label}>Name</label>
      <input data-testid="lens-name" style={input} placeholder="e.g. dev (a–z, 0–9, -, _)" value={name} disabled={busy}
        onChange={e => setName(e.target.value)} />

      <label style={label}>Write repo</label>
      <select data-testid="lens-write" style={input} value={write} disabled={busy}
        onChange={e => setWrite(e.target.value)}>
        {repos.map(r => <option key={r.name} value={r.name}>{r.name}</option>)}
      </select>
      <div style={hint}>New facts written through this lens land in the write repo.</div>

      <label style={label}>Read repos</label>
      <div style={hint}>Reads through this lens union facts from every selected repo.</div>
      <div style={{ marginTop: 6 }}>
        {repos.map(r => {
          const on = r.name in reads;
          return (
            <div key={r.name} style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 6 }}>
              <button type="button" data-testid={`lens-read-${r.name}`} style={toggle(on)} disabled={busy}
                onClick={() => toggleRead(r.name)}>{r.name}</button>
              {on && (() => {
                const bd = branchData[r.name];
                // Default option maps to '' (agent branch, resolved at bind
                // time). Every OTHER existing branch is offered as an explicit
                // pin — the agent branch itself is not repeated below the
                // default. We never allow typing a name that doesn't exist.
                const agentLabel = bd?.agent ? `Agent branch — ${bd.agent}` : 'Agent branch';
                const others = (bd?.names ?? []).filter(n => n !== bd?.agent);
                return (
                  <select data-testid={`lens-branch-${r.name}`} style={{ ...input, flex: 1, marginTop: 0 }}
                    value={reads[r.name]} disabled={busy || bd?.loading}
                    onChange={e => setBranch(r.name, e.target.value)}>
                    <option value="">{bd?.loading ? 'loading branches…' : `${agentLabel} (default)`}</option>
                    {others.map(n => <option key={n} value={n}>{n}</option>)}
                  </select>
                );
              })()}
            </div>
          );
        })}
      </div>

      {err && <div style={{ color: '#f88', fontSize: 13, marginTop: 8 }}>{err}</div>}

      <div style={{ display: 'flex', gap: 8, marginTop: 14 }}>
        <button type="button" data-testid="lens-create" style={btn(busy || !name || !write, 'primary')}
          disabled={busy || !name || !write} onClick={submit}>
          {busy ? 'Creating…' : 'Create lens'}
        </button>
      </div>
    </div>
  );
}

const label: React.CSSProperties = { fontSize: 12, color: '#888', marginBottom: 4, marginTop: 12, display: 'block' };
const hint: React.CSSProperties = { fontSize: 12, color: '#666', marginTop: 4 };
const input: React.CSSProperties = { width: '100%', boxSizing: 'border-box', background: '#111', border: '1px solid #333', color: '#eee', padding: '6px 8px', borderRadius: 4, fontSize: 13 };
const toggle = (active: boolean): React.CSSProperties => ({ minWidth: 90, background: active ? '#1d4ed8' : '#2a2a2a', color: '#eee', border: '1px solid #333', borderRadius: 4, padding: '6px 12px', fontSize: 13, cursor: 'pointer', textAlign: 'left' });
const btn = (disabled: boolean, variant: 'primary' | 'secondary' = 'secondary'): React.CSSProperties => ({ background: disabled ? '#222' : variant === 'primary' ? '#1d4ed8' : '#2a2a2a', color: disabled ? '#666' : '#eee', border: '1px solid #333', borderRadius: 4, padding: '6px 14px', fontSize: 13, cursor: disabled ? 'default' : 'pointer' });
