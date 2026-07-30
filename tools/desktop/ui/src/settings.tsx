import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { SettingsApp } from './SettingsApp.tsx'
import './settings.css'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <SettingsApp />
  </StrictMode>,
)
