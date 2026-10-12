import type { SaveAck, ServerState, Settings } from './SettingsForm.tsx'

/** A server the app spawned, listening on a loopback HTTP port. */
export const spawnedServer: ServerState = {
  running: true,
  pid: 4242,
  attached: false,
  canRestart: true,
  canReload: true,
  version: '0.5.3.abc1234',
  desktopVersion: '0.5.3.abc1234',
  versionSkew: false,
  httpUrl: 'http://127.0.0.1:19278',
  httpReason: '',
  tlsListening: '',
  tlsReason: '',
  generation: 1,
  restartNeeded: [],
  reloadError: '',
}

/** A server started outside the app (knomit serve in a terminal). */
export const attachedServer: ServerState = {
  ...spawnedServer,
  pid: 777,
  attached: true,
  canRestart: false,
}

export const baseSettings: Settings = {
  port: '19278',
  logLevel: 'info',
  logFormat: 'console',
  startAtLogin: false,
  tlsAddr: '',
  httpScope: 'local',
  server: spawnedServer,
  effectivePort: 19278,
  configPath: '/home/u/.knomit/knomit.toml',
  logFilePath: '/home/u/Library/Logs/knomit/knomit-desktop.log',
  overriddenByEnv: [],
}

export function ack(over: Partial<SaveAck> = {}): SaveAck {
  return {
    outcome: 'applied',
    generation: 2,
    httpUrl: 'http://127.0.0.1:19278',
    httpReason: '',
    tlsListening: '',
    tlsReason: '',
    restartNeeded: [],
    error: '',
    ...over,
  }
}
