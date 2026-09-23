// URL/auth-method compatibility for the remote connect wizard. The two fleet
// refusals are the SAME words validateURLAuth (internal/web/helpers.go) returns,
// and originAuth.test.ts runs the same table as TestValidateURLAuth_FleetScheme,
// so the wizard and the server cannot disagree about a knomit+https origin.

export const KNOMIT_AUTH_WRONG = 'knomit+https origins authenticate with the instance certificate — use auth method cert';
export const CERT_URL_WRONG = 'cert auth is only valid with knomit+https:// URLs';

const KNOMIT_PREFIX = 'knomit+https://';

// Case-insensitive, as go-git (url.Parse) matches the scheme.
export function isKnomitURL(u: string): boolean {
  return u.slice(0, KNOMIT_PREFIX.length).toLowerCase() === KNOMIT_PREFIX;
}

export function isSSHURL(u: string): boolean {
  return u.startsWith('git@') || u.startsWith('ssh://');
}

// Never true for knomit+https: that scheme does not start with http.
export function isHTTPURL(u: string): boolean {
  return u.startsWith('http://') || u.startsWith('https://');
}

// The blocking mismatch for url + method, or '' when the pair is allowed.
export function urlAuthMismatch(u: string, method: string): string {
  const knomit = isKnomitURL(u);
  if (knomit && method !== 'cert' && method !== '') return KNOMIT_AUTH_WRONG;
  if (!knomit && method === 'cert') return CERT_URL_WRONG;
  if (isHTTPURL(u) && method === 'ssh') return 'SSH auth cannot be used with HTTP/HTTPS URLs';
  if (isSSHURL(u) && (method === 'token' || method === 'basic')) return 'Token/basic auth cannot be used with SSH URLs';
  return '';
}
