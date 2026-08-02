import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { SettingsApp } from './SettingsApp.tsx'
import '@fontsource-variable/space-grotesk/index.css'
import '@fontsource/jetbrains-mono/400.css'
import '@fontsource/jetbrains-mono/500.css'
import './index.css'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <SettingsApp />
  </StrictMode>,
)
