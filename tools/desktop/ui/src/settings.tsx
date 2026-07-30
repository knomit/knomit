import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { SettingsApp } from './SettingsApp.tsx'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <SettingsApp />
  </StrictMode>,
)
