// URL/auth-method compatibility for the remote connect wizard, the rows it
// shares with validateURLAuth (internal/edge/web/helpers.go). Another knomit
// instance of this fleet is typed as a plain https URL: the server decides it
// is one and picks its transport, so the wizard offers nothing for it.

export function isSSHURL(u: string): boolean {
  return u.startsWith('git@') || u.startsWith('ssh://');
}

export function isHTTPURL(u: string): boolean {
  return u.startsWith('http://') || u.startsWith('https://');
}

// The blocking mismatch for url + method, or '' when the pair is allowed.
export function urlAuthMismatch(u: string, method: string): string {
  if (isHTTPURL(u) && method === 'ssh') return 'SSH auth cannot be used with HTTP/HTTPS URLs';
  if (isSSHURL(u) && (method === 'token' || method === 'basic')) return 'Token/basic auth cannot be used with SSH URLs';
  return '';
}
