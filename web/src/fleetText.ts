import type { FleetEventKind, FleetMember } from './api';

// The fleet pages' words and the helpers that pick them. Kept apart from the
// components (react-refresh: a component module exports components only).
// Server messages are never reworded here: a refusal shows the server's text
// verbatim with its code beneath; these are the page's own strings.

/** A chip's tone, the prototype's palette: the fleet's teal for the master,
 *  green for active, blue for enrolled and for a record pending (a normal
 *  state that resolves itself: informational, never amber, which is kept for
 *  what fails, 3f1b3342), red for revoked, neutral otherwise. */
export type ChipTone = 'master' | 'member' | 'pending' | 'active' | 'revoked' | 'left' | 'enrolled' | 'green' | 'blue' | 'amber' | 'bad' | 'neutral';

export const CHIP_TONES: Record<ChipTone, { color: string; background: string; border: string }> = {
  master: { color: '#8ad6c8', background: '#121f1d', border: '1px solid #24453f' },
  member: { color: '#999', background: '#1a1a1a', border: '1px solid #2a2a2a' },
  neutral: { color: '#999', background: '#1a1a1a', border: '1px solid #2a2a2a' },
  left: { color: '#999', background: '#1a1a1a', border: '1px solid #2a2a2a' },
  pending: { color: '#8ab6d6', background: '#131d26', border: '1px solid #244056' },
  amber: { color: '#e2c07a', background: '#262013', border: '1px solid #4a3f22' },
  active: { color: '#7c9', background: '#1a231d', border: '1px solid #2a4a33' },
  green: { color: '#7c9', background: '#1a231d', border: '1px solid #2a4a33' },
  revoked: { color: '#e8a0a0', background: '#1f1414', border: '1px solid #4a2626' },
  bad: { color: '#e8a0a0', background: '#1f1414', border: '1px solid #4a2626' },
  enrolled: { color: '#8ab6d6', background: '#131d26', border: '1px solid #244056' },
  blue: { color: '#8ab6d6', background: '#131d26', border: '1px solid #244056' },
};

/** The members table's order: active, enrolled, left, revoked. */
export const STATE_ORDER: Record<FleetMember['state'], number> = { active: 0, enrolled: 1, left: 2, revoked: 3 };

/** The line under a member's state chip. */
export function stateSubline(m: FleetMember): string {
  if (m.state === 'left') return 'certificate valid until revoked';
  if (m.state === 'enrolled') return 'certificate on main; not joined yet';
  return '';
}

/** The toolbar's counts: `N active` always, the rest only when non-zero. */
export function memberCounts(ms: FleetMember[]): string {
  const n = (s: FleetMember['state']) => ms.filter(m => m.state === s).length;
  const refused = ms.filter(m => m.last_refusal).length;
  return [
    `${n('active')} active`,
    n('enrolled') ? `${n('enrolled')} enrolled` : '',
    n('left') ? `${n('left')} left` : '',
    n('revoked') ? `${n('revoked')} revoked` : '',
    refused ? `${refused} refused` : '',
  ].filter(Boolean).join(' · ');
}

/** os/arch · version, as the record has them; '' when it has neither. */
export function versionText(c?: Record<string, string>): string {
  if (!c) return '';
  const parts: string[] = [];
  if (c.os || c.arch) parts.push(`${c.os ?? '?'}/${c.arch ?? '?'}`);
  if (c.version) parts.push(c.version);
  return parts.join(' · ');
}

/** A commit hash as the tables show it. */
export const shortTip = (tip: string) => tip.slice(0, 7);

/** The Activity tab's names for the event kinds GET /fleet/activity returns.
 *  A root key import, a stand-down and a decommission leave no record, so
 *  they are not events and have no name here. */
export const EVENT_LABEL: Record<FleetEventKind, string> = {
  created: 'Fleet created', enrolled: 'Enrolled', joined: 'Joined', admitted: 'Admitted into main',
  refused: 'Branch refused', revoked: 'Revoked', left: 'Left', 'host-changed': 'Host changed',
};
export const EVENT_TONE: Record<FleetEventKind, ChipTone> = {
  created: 'green', enrolled: 'blue', joined: 'blue', admitted: 'green',
  refused: 'amber', revoked: 'bad', left: 'neutral', 'host-changed': 'master',
};

/** When, as the Activity table reads it: month, day and the minute. */
export function when(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
}

/** The one-master sentence, verbatim in Export, Import and Stand down. */
export const ONE_MASTER = 'A fleet has one master. Copy the key to the new master and remove it from the old one; knomit does not check that you did.';

/** bundleHost is the host of the fleet URL a bundle's header names, for the
 *  `Couldn't reach <host>` and refused headlines. It reads the header the
 *  server checked (join:check refuses a bundle without one before it can
 *  answer unreachable or bundle_refused); the bundle itself is never shown. */
export function bundleHost(bundle: string): string {
  const m = /^url: *(\S+)/m.exec(bundle);
  if (!m) return '';
  const scp = /^[^@/]+@([^:]+):/.exec(m[1]);
  if (scp) return scp[1];
  const u = /^[a-z][a-z0-9+.-]*:\/\/(?:[^@/]+@)?([^/]+)/i.exec(m[1]);
  return u ? u[1] : '';
}

/** What the Bundle step's card says for a refused join:check, by code. The
 *  server's text is shown beneath, verbatim. */
export function checkRefusalCard(code: string, host: string): { headline: string; hint?: string } {
  const resend = 'Send the master this instance’s agent id and public key (Start) and ask for a new bundle.';
  switch (code) {
    case 'agent_mismatch': return { headline: 'This bundle is for another instance', hint: resend };
    case 'key_mismatch': return { headline: 'The bundle was issued for a different key', hint: resend };
    case 'bundle_invalid': return { headline: 'That is not a fleet bundle' };
    case 'unreachable': return { headline: host ? `Couldn't reach ${host}` : "Couldn't reach the fleet" };
    case 'bundle_refused': return { headline: host ? `The fleet at ${host} refused this bundle` : 'The fleet refused this bundle', hint: 'Ask the fleet’s master for a new bundle.' };
    default: return { headline: 'The check was refused' };
  }
}

/** The note under a refused join on Review, by code. */
export function joinRefusalNote(code: string): string {
  switch (code) {
    case 'key_mismatch': return 'Nothing was kept. Send the master this instance’s public key (Start) and ask for a new bundle.';
    case 'root_differs': return 'Nothing was kept. The bundle is for another fleet: ask the master for one issued by this fleet’s root.';
    case 'join_refused': return 'Nothing was kept. You can try again once the master’s next round has published the certificate.';
    case 'bundle_refused': return 'Nothing was kept. Ask the fleet’s master for a new bundle.';
    default: return 'Nothing was kept.';
  }
}

/** readTextFile reads a file the reader chose (a bundle, a root key file). */
export function readTextFile(f: File): Promise<string> {
  if (typeof f.text === 'function') return f.text();
  return new Promise((resolve, reject) => {
    const r = new FileReader();
    r.onload = () => resolve(String(r.result ?? ''));
    r.onerror = () => reject(new Error(`Could not read ${f.name}.`));
    r.readAsText(f);
  });
}

/** intervalText is a sync interval as the page says it: "5 min", "90 s". */
export function intervalText(seconds: number): string {
  if (seconds >= 60 && seconds % 60 === 0) return `${seconds / 60} min`;
  return `${seconds} s`;
}

/** listenerNotRunning is what the page says when the fleet listener is not
 *  up (GET /fleet host.listening false): members cannot reach this instance
 *  until it starts, and it starts only when knomit does with a certificate
 *  installed (a create, a join or a host-here installs one). */
export function listenerNotRunning(configured: boolean, reason?: string): string {
  if (!configured) return 'Members reach this instance over the fleet listener, and it is off. Set the Fleet port in Settings → Network; saving applies it.';
  if (reason === 'addr_in_use') return 'Members reach this instance over the fleet listener, and its port is held by another program. Free the port or choose another Fleet port in Settings → Network.';
  return 'Members reach this instance over the fleet listener, and it starts only when knomit starts with this instance\'s certificate installed. Restart knomit (or run `knomit reload` for a server you started yourself) to start it; until then no member can join or push.';
}
