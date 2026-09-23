import { describe, it, expect } from 'vitest';
import { urlAuthMismatch, isKnomitURL, isHTTPURL, KNOMIT_AUTH_WRONG, CERT_URL_WRONG } from './originAuth';

// The same table as TestValidateURLAuth_FleetScheme in
// internal/web/helpers_test.go, row for row. Change one, change both.
const rows: [string, string, string][] = [
  ['https://github.com/o/r.git', 'token', ''],
  ['https://github.com/o/r.git', 'cert', CERT_URL_WRONG],
  ['knomit+https://h:8443/git/kb', 'cert', ''],
  ['knomit+https://h:8443/git/kb', '', ''],
  ['KNOMIT+HTTPS://h:8443/git/kb', 'cert', ''],
  ['knomit+https://h:8443/git/kb', 'token', KNOMIT_AUTH_WRONG],
  ['knomit+https://h:8443/git/kb', 'basic', KNOMIT_AUTH_WRONG],
  ['knomit+https://h:8443/git/kb', 'none', KNOMIT_AUTH_WRONG],
  ['KNOMIT+HTTPS://h:8443/git/kb', 'token', KNOMIT_AUTH_WRONG],
  ['git@github.com:o/r.git', 'cert', CERT_URL_WRONG],
  ['ssh://git@github.com/o/r.git', 'cert', CERT_URL_WRONG],
  ['/srv/kb', 'cert', CERT_URL_WRONG],
  ['knomit+https://h:8443/git/kb', 'ssh', KNOMIT_AUTH_WRONG],
];

describe('urlAuthMismatch (mirrors validateURLAuth)', () => {
  it.each(rows)('%s + %s', (url, method, want) => {
    expect(urlAuthMismatch(url, method)).toBe(want);
  });

  it('uses the Go wording verbatim', () => {
    expect(KNOMIT_AUTH_WRONG).toBe('knomit+https origins authenticate with the instance certificate — use auth method cert');
    expect(CERT_URL_WRONG).toBe('cert auth is only valid with knomit+https:// URLs');
  });

  it('never classifies knomit+https as HTTP', () => {
    expect(isKnomitURL('knomit+https://h/git/kb')).toBe(true);
    expect(isHTTPURL('knomit+https://h/git/kb')).toBe(false);
  });
});
