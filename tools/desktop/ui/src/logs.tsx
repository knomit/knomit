import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { LogsApp } from './LogsApp.tsx'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <LogsApp />
  </StrictMode>,
)
