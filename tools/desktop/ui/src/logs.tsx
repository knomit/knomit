import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { LogsApp } from './LogsApp.tsx'
import { connectLogStream } from './logStore.ts'
import '@fontsource-variable/space-grotesk/index.css'
import '@fontsource/jetbrains-mono/400.css'
import '@fontsource/jetbrains-mono/500.css'
import './index.css'

// BEFORE the render call, and deliberately: Go emits the file's backlog as soon
// as the window is created, while React's first render — and any effect inside
// it — is merely scheduled. Subscribing here happens during this module's
// evaluation, which the webview cannot interleave with the JavaScript Wails
// injects to deliver an event, so the backlog is buffered in the store instead
// of being dispatched to nobody. See logStore.ts.
//
// The guarantee rests on this module and everything above it being ONE
// run-to-completion evaluation: no top-level `await` and no dynamic `import()`
// before this line, in this file or in what it imports, and a bundle that stays
// a single chunk. Any of those yields to the event loop, which lets the
// injected dispatch run first and reopens the race. logs.test.tsx pins the
// order; it cannot pin the chunking.
//
// Never unsubscribed: the subscription's lifetime is the window's, and the
// window hides rather than closes precisely so this survives.
connectLogStream()

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <LogsApp />
  </StrictMode>,
)
