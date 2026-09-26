// Fixtures for the Fleet identity tests, typed as the wire interfaces so
// tsc holds them to the types; fleetWire.json holds them to Go.
import type { FleetIdentity } from './fleet.ts'

export const FP = 'a22ef991995221354898c11b64cfeaced731c99a9c1aa798fe7b89bb4599e036'
export const ROOT_A = '1111111111111111111111111111111111111111111111111111111111111111'
export const ROOT_B = '2222222222222222222222222222222222222222222222222222222222222222'

export const notEnrolled: FleetIdentity = {
  state: 'ready',
  dir: '/home/u/.knomit/pki',
  keyFingerprint: FP,
  enrolled: false,
  certificate: 'none',
  principal: '',
  san: '',
  serial: '',
  notAfter: '',
  rootFingerprint: '',
  crlNumber: '',
  crlNextUpdate: '',
  crlStale: false,
  tls: { booted: true, configured: '', listening: '', reason: '', configuredNow: '' },
}

export const enrolled: FleetIdentity = {
  ...notEnrolled,
  enrolled: true,
  certificate: 'ok',
  principal: `instance:${FP}@cert`,
  san: 'knomit://instance/laptop-a22ef991',
  serial: 'b09ad10ba3108b5d36091c85ea798e45',
  notAfter: '2026-12-24T22:49:01Z',
  rootFingerprint: ROOT_A,
  crlNumber: '3',
  crlNextUpdate: '2026-10-25T22:49:01Z',
  tls: { booted: true, configured: '0.0.0.0:19279', listening: '[::]:19279', reason: '', configuredNow: '0.0.0.0:19279' },
}
