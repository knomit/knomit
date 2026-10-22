import { describe, it, expect } from 'vitest';
import { urlAuthMismatch } from './originAuth';

// The rows TestValidateURLAuth_FleetPeersAreDecidedNotTyped in
// internal/edge/web/helpers_test.go shares with the wizard. A peer's https URL
// with no auth is allowed: the server decides it is a peer.
const rows: [string, string, string][] = [
  ['https://github.com/o/r.git', 'token', ''],
  ['https://h:8443/git/kb', '', ''],
  ['https://h:8443/git/kb', 'basic', ''],
  ['https://github.com/o/r.git', 'ssh', 'SSH auth cannot be used with HTTP/HTTPS URLs'],
  ['git@github.com:o/r.git', 'token', 'Token/basic auth cannot be used with SSH URLs'],
  ['ssh://git@github.com/o/r.git', 'basic', 'Token/basic auth cannot be used with SSH URLs'],
];

describe('urlAuthMismatch (the rows it shares with validateURLAuth)', () => {
  it.each(rows)('%s + %s', (url, method, want) => {
    expect(urlAuthMismatch(url, method)).toBe(want);
  });
});
