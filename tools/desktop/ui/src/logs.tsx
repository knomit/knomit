import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { LogsApp } from './LogsApp.tsx'
import { connectLogStream } from './logStore.ts'
import './logs.css'

// BEFORE the render call, and deliberately: Go emits the file's backlog as soon
// as the window is created, while React's first render — and any effect inside
// it — is merely scheduled. Subscribing here happens during this module's
// evaluation, which the webview cannot interleave with the JavaScript Wails
// injects to deliver an event, so the backlog is buffered in the store instead
// of being dispatched to nobody. See logStore.ts.
//
// Never unsubscribed: the subscription's lifetime is the window's, and the
// window hides rather than closes precisely so this survives.
connectLogStream()

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <LogsApp />
  </StrictMode>,
)
