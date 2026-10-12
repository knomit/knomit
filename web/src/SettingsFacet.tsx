import { useState } from 'react';
import type { CSSProperties } from 'react';
import type { BrowseContext } from './state';
import { brokenLensMembers } from './api';
import type { Lens, RepoInfo, RepoCreateStatus, ArchivedRepo } from './api';
import type { PinnedTarget } from './facets';
import { RepoSettings } from './RepoSettings';
import { LensSettings } from './LensSettings';
import { ArchivedSettings } from './ArchivedSettings';
import { UnavailableSettings } from './UnavailableSettings';
import { PendingCreateSettings } from './PendingCreateSettings';

// SettingsFacet is a target's Settings page. For a normal target (the browse
// context) that is RepoSettings or LensSettings; for a pinned, settings-only
// target it is the page of what it is — ArchivedSettings, UnavailableSettings,
// PendingCreateSettings, or a broken lens's LensSettings headed by the mounts
// that have no store. The pages own their index column and scroll
// container; this adds only the error line their mutations report to.
//
// `serverReadOnly` is the instance gate and nothing else — never
// isReadOnly(state), which describes the BROWSED branch (a subscription, a
// history excursion) and belongs to RightPanel alone. A subscription's own
// lock is read by RepoSettings from its own details.
export function SettingsFacet({
  target, repos, lenses, archived, serverReadOnly, createJob, onCancelCreate, focus, onFocused,
  onChanged, onSelectLens, onEnterBranch, currentBranch, onBusyChange,
  onRestored, onPurged, onUnavailableArchived, onCreateClosed, onCreateRegistered,
}: {
  target: BrowseContext | PinnedTarget;
  repos: RepoInfo[];
  lenses: Lens[];
  archived: ArchivedRepo[];
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
  /** An archived repo was restored under `name`. */
  onRestored: (name: string) => void;
  /** An archived repo was purged. */
  onPurged: () => void;
  /** A repo with no store was archived; the archived row is the new target. */
  onUnavailableArchived: (archived: ArchivedRepo) => void;
  /** A create not yet registered was dismissed. */
  onCreateClosed: () => void;
  /** A create not yet registered now has its repository. */
  onCreateRegistered: (name: string) => void;
}) {
  const [err, setErr] = useState('');
  let page: React.ReactNode = null;
  switch (target.kind) {
    case 'repo':
      page = (
        <RepoSettings
          key={target.repo}
          name={target.repo}
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
          onRenamed={to => onChanged({ from: target.repo, to })}
          onSelectLens={onSelectLens}
          onEnterBranch={onEnterBranch}
          currentBranch={currentBranch}
          onError={setErr}
          onBusyChange={onBusyChange}
        />
      );
      break;
    case 'lens':
      page = (
        <LensSettings
          key={target.name}
          lens={lenses.find(l => l.name === target.name)}
          name={target.name}
          repos={repos}
          readOnly={serverReadOnly}
          onDeleted={() => onChanged()}
          onSaved={() => onChanged()}
          onRenamed={to => onChanged({ from: target.name, to })}
          onError={setErr}
        />
      );
      break;
    case 'brokenLens': {
      const lens = lenses.find(l => l.name === target.name);
      page = lens && (
        <LensSettings
          key={target.name}
          lens={lens}
          name={target.name}
          repos={repos}
          readOnly={serverReadOnly}
          broken={brokenLensMembers(lens, repos)}
          onDeleted={() => onChanged()}
          onSaved={() => onChanged()}
          onRenamed={to => onChanged({ from: target.name, to })}
          onError={setErr}
        />
      );
      break;
    }
    case 'archived': {
      const info = archived.find(a => a.id === target.id);
      page = info && (
        <ArchivedSettings
          key={info.id}
          info={info}
          serverReadOnly={serverReadOnly}
          activeNames={new Set(repos.map(r => r.name))}
          onRestored={onRestored}
          onPurged={onPurged}
          onError={setErr}
        />
      );
      break;
    }
    case 'unavailable': {
      const repo = repos.find(r => r.name === target.repo);
      page = repo && (
        <UnavailableSettings key={repo.name} repo={repo} serverReadOnly={serverReadOnly} onArchived={onUnavailableArchived} />
      );
      break;
    }
    case 'pending':
      page = (
        <PendingCreateSettings key={target.createId} createId={target.createId}
          onClose={onCreateClosed} onOpenRepo={onCreateRegistered} />
      );
      break;
  }
  return (
    <div data-testid="settings-facet" style={body}>
      {err && (
        <div data-testid="settings-error" style={errLine}>
          <span style={{ flex: 1 }}>{err}</span>
          <button type="button" aria-label="Dismiss" onClick={() => setErr('')} style={dismiss}>✕</button>
        </div>
      )}
      {page}
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
