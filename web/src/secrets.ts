import type { SecretType, SecretView } from './api';
import { isHTTPURL, isSSHURL } from './originAuth';

// Stored-secret rules the UI applies before the server does, spelled as the
// server spells them (internal/corpus/repos/control/secrets.go RemoteHost and
// HostAllowed): a secret fits a remote when its type fits the scheme (token
// or username + password over http(s), an SSH key over SSH) AND its hosts
// list allows the remote's host. The server checks again when linking and at
// every git operation; this only keeps the picker honest.

export const SECRET_TYPE_LABEL: Record<SecretType, string> = {
  token: 'Token',
  basic: 'Username + password',
  ssh_key: 'SSH key',
};

/** remoteHost is the remote's host as the server reads it: lowercased, with
 *  its port when the URL names one; '' for a local path or an unparsable URL. */
export function remoteHost(url: string): string {
  const u = url.trim();
  if (u === '') return '';
  if (u.includes('://')) {
    const m = /^[a-z][a-z0-9+.-]*:\/\/(?:[^@/]*@)?([^/?#]+)/i.exec(u);
    if (!m || /^file:/i.test(u)) return '';
    return m[1].toLowerCase();
  }
  const colon = u.indexOf(':');
  if (colon <= 0 || u.slice(0, colon).includes('/')) return '';
  let host = u.slice(0, colon);
  const at = host.lastIndexOf('@');
  if (at >= 0) host = host.slice(at + 1);
  return host.toLowerCase();
}

/** hostAllowed: an entry matches the remote host exactly, or, when the entry
 *  names no port, the remote host's name with any port. */
export function hostAllowed(hosts: string[], host: string): boolean {
  if (host === '') return false;
  const name = host.replace(/:\d+$/, '');
  return hosts.some(h => h === host || (!h.includes(':') && h === name));
}

/** typesForURL: the secret types that can authenticate to url. */
export function typesForURL(url: string): SecretType[] {
  const u = url.trim();
  if (isSSHURL(u)) return ['ssh_key'];
  if (isHTTPURL(u)) return ['token', 'basic'];
  return [];
}

/** secretFits: s may be linked to a repo whose remote is url. */
export function secretFits(s: Pick<SecretView, 'type' | 'hosts'>, url: string): boolean {
  return typesForURL(url).includes(s.type) && hostAllowed(s.hosts, remoteHost(url));
}

/** The <input type="date"> value for an RFC 3339 expiry ('' for none). */
export function expiryDate(at: string | null | undefined): string {
  return at ? at.slice(0, 10) : '';
}

/** An <input type="date"> value as the RFC 3339 expiry the API takes (the
 *  start of that day, UTC), or null for an empty input. */
export function expiryFromDate(d: string): string | null {
  return d ? `${d}T00:00:00Z` : null;
}

/** parseHosts splits a typed hosts list on commas, spaces and newlines. */
export function parseHosts(text: string): string[] {
  return [...new Set(text.split(/[\s,]+/).map(h => h.trim().toLowerCase()).filter(Boolean))];
}

/** The credential method a linked secret's type implies for the transport
 *  checks (urlAuthMismatch). */
export function methodForSecretType(t: SecretType): string {
  return t === 'ssh_key' ? 'ssh' : t;
}
