import { useCallback, useEffect, useState } from 'react';
import { api, type OAuthApproval, type OAuthGrant, type OAuthPending } from './api';

// The Authorizations section's non-component parts: the server's default
// ceiling, the redirect host, list keys, the data hook and the approval note.

// Requests expire after 10 minutes and someone is usually waiting in a
// browser, so this polls far faster than Sessions' 30 s.
const POLL_MS = 5_000;

// defaultScopes is the server's own default ceiling (3a ruling D13):
// requested ∩ {read, write}, else read. Clients ask for everything the server
// advertises — Claude Code does — so pre-checking "what was asked" would make
// the careless click the widest grant.
export function defaultScopes(requested: string[]): string[] {
  const d = ['read', 'write'].filter(s => requested.includes(s));
  return d.length > 0 ? d : ['read'];
}

export function redirectHost(uri: string): string {
  try {
    return new URL(uri).host || uri;
  } catch {
    return uri;
  }
}

/** A list id: requests and approvals share one selection. */
export const pendingKey = (id: string) => `p:${id}`;
export const grantKey = (id: string) => `g:${id}`;

// ─── Data ────────────────────────────────────────────────────────────────────

export interface AuthData {
  pending: OAuthPending[] | null;
  /** null: the server lists no approvals (no issuer, or not read yet). */
  grants: OAuthGrant[] | null;
  err: string;
  reload: () => void;
}

/** useAuthData polls the queue (and reads the approvals) while the section
 *  is on screen, and reports the queue's length for the Access tab's badge. */
export function useAuthData(onCount?: (n: number | null) => void, enabled = true): AuthData {
  const [pending, setPending] = useState<OAuthPending[] | null>(null);
  const [grants, setGrants] = useState<OAuthGrant[] | null>(null);
  const [err, setErr] = useState('');
  const reload = useCallback(() => {
    api.listOAuthPending()
      .then(list => {
        setErr('');
        setPending(list ?? []);
        // null: no issuer here any more (404); the caller drops the section.
        onCount?.(list === null ? null : list.length);
      })
      .catch(e => setErr(e instanceof Error ? e.message : String(e)));
    // A failed read keeps what was listed: the queue's error already shows,
    // and a flicker of the Approved group would move the selection.
    api.listOAuthGrants().then(setGrants).catch(() => undefined);
  }, [onCount]);
  useEffect(() => {
    if (!enabled) return;
    reload();
    const t = setInterval(reload, POLL_MS);
    return () => clearInterval(t);
  }, [reload, enabled]);
  return { pending, grants, err, reload };
}

/** The approval note: what the reader should know after Approve. */
export function approvalNotice(a: OAuthApproval, p: OAuthPending): string {
  const who = p.client_name || p.client_id;
  return a.grantsUnchanged
    ? `Approved ${who}. host:${a.subject}@token had been granted before, so its grants were left as they are and may cap this token below the scopes you checked. Widen them with: knomit grants add host:${a.subject}@token <perm>`
    : `Approved ${who}. It signs in as host:${a.subject}@token.`;
}
