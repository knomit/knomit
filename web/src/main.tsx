import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import '@fontsource-variable/space-grotesk/index.css'
import '@fontsource/jetbrains-mono/400.css'
import '@fontsource/jetbrains-mono/500.css'
import './index.css'
import App from './App.tsx'
import { installExternalLinkHandler } from './externalLinks'

// Desktop only, and a no-op everywhere else: the Wails webview silently drops
// target="_blank", so without this every external link in the app is inert.
// Installed on the document rather than per-link because the links are
// generated (autolinks in fact bodies, the References rail). See externalLinks.ts.
installExternalLinkHandler()

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
