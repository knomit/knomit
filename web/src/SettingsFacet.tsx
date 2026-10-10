import { useState } from 'react';
import type { CSSProperties } from 'react';
import type { BrowseContext } from './state';
import type { Lens, RepoInfo, RepoCreateStatus } from './api';
import { RepoSettings } from './RepoSettings';
import { LensSettings } from './LensSettings';

// SettingsFacet is a normal target's Settings page: RepoSettings for a repo,
// LensSettings for a lens, both rendered directly (no Manage around them).
// The pages own their index column and scroll container; this adds only the
// error line their mutations report to.
//
// `serverReadOnly` is the instance gate and nothing else — never
// isReadOnly(state), which describes the BROWSED branch (a subscription, a
// history excursion) and belongs to RightPanel alone. A subscription's own
// lock is read by RepoSettings from its own details.
export function SettingsFacet({
  context, repos, lenses, serverReadOnly, createJob, onCancelCreate, focus, onFocused,
  onChanged, onSelectLens, onEnterBranch, currentBranch, onBusyChange,
}: {
  context: BrowseContext;
  repos: RepoInfo[];
  lenses: Lens[];
  serverReadOnly: boolean;
  /** The create still working on the repo, if any (its creating mode). */
  createJob: RepoCreateStatus | null;
  onCancelCreate: () => void;
  /** One-shot block to land on. */
  focus: string | null;
  onFocused: () => void;
  /** A settings mutation landed; a rename carries both names. */
  onChanged: (renamed?: { from: string; to: string }) => void;
  onSelectLens: (name: string) => void;
  onEnterBranch: (branch: string) => void;
  currentBranch: string;
  /** The connect commit lock, relayed to the view. */
  onBusyChange: (busy: boolean) => void;
}) {
  const [err, setErr] = useState('');
  return (
    <div data-testid="settings-facet" style={body}>
      {err && (
        <div data-testid="settings-error" style={errLine}>
          <span style={{ flex: 1 }}>{err}</span>
          <button type="button" aria-label="Dismiss" onClick={() => setErr('')} style={dismiss}>✕</button>
        </div>
      )}
      {context.kind === 'repo' ? (
        <RepoSettings
          key={context.repo}
          name={context.repo}
          lenses={lenses}
          focus={focus ?? undefined}
          onFocused={onFocused}
          canArchive={!serverReadOnly}
          serverReadOnly={serverReadOnly}
          hideRemoteConfig={serverReadOnly}
          createJob={createJob}
          onCancelCreate={onCancelCreate}
          onArchived={() => onChanged()}
          onChanged={() => onChanged()}
          onRenamed={to => onChanged({ from: context.repo, to })}
          onSelectLens={onSelectLens}
          onEnterBranch={onEnterBranch}
          currentBranch={currentBranch}
          onError={setErr}
          onBusyChange={onBusyChange}
        />
      ) : (
        <LensSettings
          key={context.name}
          lens={lenses.find(l => l.name === context.name)}
          name={context.name}
          repos={repos}
          readOnly={serverReadOnly}
          onDeleted={() => onChanged()}
          onSaved={() => onChanged()}
          onRenamed={to => onChanged({ from: context.name, to })}
          onError={setErr}
        />
      )}
    </div>
  );
}

const body: CSSProperties = { flex: '1 1 0', minHeight: 0, display: 'flex', flexDirection: 'column', overflow: 'hidden' };
const errLine: CSSProperties = {
  display: 'flex', alignItems: 'center', gap: 10, flexShrink: 0,
  background: '#1f1414', borderBottom: '1px solid #4a2626', color: '#e8a0a0',
  fontSize: 12, padding: '6px 14px',
};
const dismiss: CSSProperties = { background: 'none', border: 'none', color: '#b98080', cursor: 'pointer', fontSize: 13 };
