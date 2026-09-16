import type { CSSProperties } from 'react';
import { useRepoCreates, runningCreates } from './useRepoCreates';

// CreateIndicator is the global "something is being created" light.
//
// A detached create is invisible from every view but the one that started it,
// and the one that started it is a wizard the user closes. The reported
// incident is a subscribe that ran for minutes with nothing anywhere on screen
// saying so — the user went to Logs, came back, and concluded it had vanished.
//
// It is an INDICATOR, not a lock: the create owns itself, nothing here can
// cancel it, and no part of the app is blocked while it runs. Clicking it goes
// to the list where the pending rows are.
//
// It renders nothing at zero. A permanent "0 creates" would be the top bar
// spending its scarcest space on the answer "nothing is happening".
export function CreateIndicator({ onOpen }: { onOpen?: () => void }) {
  const creates = useRepoCreates();
  const running = runningCreates(creates);
  if (running === 0) return null;

  const label = running === 1 ? '1 create running' : `${running} creates running`;
  return (
    <button
      type="button"
      className="k-bare"
      data-testid="create-indicator"
      data-running={running}
      title={`${label} — open the list`}
      aria-label={label}
      onClick={onOpen}
      style={{ ...btn, cursor: onOpen ? 'pointer' : 'default' }}
    >
      <span aria-hidden="true" style={dot} />
      <span>{running}</span>
    </button>
  );
}

const btn: CSSProperties = {
  display: 'inline-flex', alignItems: 'center', gap: 5,
  padding: '2px 7px', borderRadius: 3,
  fontSize: 11, fontFamily: 'var(--k-font-mono)',
  color: '#8ab6d6', background: '#131d26', border: '1px solid #244056',
};
const dot: CSSProperties = {
  width: 6, height: 6, borderRadius: '50%', background: '#5f9ec4', flexShrink: 0,
};
