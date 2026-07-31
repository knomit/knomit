import { useEffect, useState } from 'react'
import { Call, Window } from '@wailsio/runtime'
import { SettingsForm, type Settings } from './SettingsForm.tsx'
import './App.css'

// Bound-method names are packagePath.TypeName.MethodName, which Wails builds in
// pkg/application/bindings.go. NativeService lives in package main, so the
// package path is literally "main". There is NO binding codegen in this repo —
// Call.ByName reaches the service directly, proven by the Task 3 spike — so
// these strings are the only thing tying the UI to the Go method set, and a
// typo in one fails at runtime with nothing to catch it at build time.
// SettingsApp.test.tsx pins them.
const GET = 'main.NativeService.GetSettings'
const SAVE = 'main.NativeService.SaveSettings'
const RESTART = 'main.NativeService.RestartApp'
const REVEAL = 'main.NativeService.RevealLogFile'

// Kept separate from settings.tsx (which only mounts it) so it can be rendered
// in a test without a real #root element.
export function SettingsApp() {
  const [initial, setInitial] = useState<Settings | null>(null)
  const [loadError, setLoadError] = useState('')

  useEffect(() => {
    Call.ByName(GET)
      .then((s: Settings) => setInitial(s))
      .catch((e: unknown) => setLoadError(e instanceof Error ? e.message : String(e)))
  }, [])

  if (loadError) {
    return (
      <div className="settings">
        <h1>Settings</h1>
        <p role="alert" className="error">
          Could not load settings: {loadError}
        </p>
      </div>
    )
  }
  if (!initial) {
    return (
      <div className="settings">
        <h1>Settings</h1>
        <p>Loading…</p>
      </div>
    )
  }

  return (
    <SettingsForm
      initial={initial}
      onSave={(s) => Call.ByName(SAVE, s)}
      // Handed straight through, unawaited by design: RestartApp's promise
      // never settles on success, because the process is gone before the
      // transport can answer. SettingsForm.restart() carries the full
      // reasoning — read it before changing anything here.
      onRestart={() => Call.ByName(RESTART)}
      onRevealLog={() => Call.ByName(REVEAL)}
      // Cancel discards by closing: SettingsForm holds every edit in local
      // state and writes nothing until Save, so destroying the window IS the
      // discard. ShowSettings builds a fresh one on the next open (see
      // windows.go), which is what makes reopening show what is on disk rather
      // than the abandoned edits.
      onCancel={() => void Window.Close()}
    />
  )
}
