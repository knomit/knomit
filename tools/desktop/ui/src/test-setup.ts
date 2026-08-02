import '@testing-library/jest-dom/vitest'
import { afterEach } from 'vitest'
import { cleanup } from '@testing-library/react'

// Same as web/src/test-setup.ts: @testing-library/react v16 only auto-registers
// afterEach(cleanup) when `afterEach` is a global, and this config does not set
// `globals: true`, so register it explicitly.
afterEach(() => {
  cleanup()
})
