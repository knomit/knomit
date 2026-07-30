import { existsSync, readFileSync, writeFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { defineConfig, type Plugin } from 'vitest/config'
import react from '@vitejs/plugin-react'

const SENTINEL = resolve(__dirname, 'dist/.gitkeep')

const SENTINEL_FALLBACK = `# Keeps tools/desktop/ui/dist/ present so \`//go:embed all:dist\` compiles on a
# fresh checkout. Real assets come from \`make desktop-ui\`.
`

// Same reasoning as web/vite.config.ts: dist/ is gitignored except for this
// sentinel, which //go:embed needs in order to compile before anyone has run a
// frontend build. vite's emptyOutDir would delete it, leaving a deleted tracked
// file and a red `go build -tags desktop ./tools/desktop/...`. Emptying is still
// what we want (stale hashed assets would otherwise pile up in the embedded FS),
// so snapshot the sentinel at config-evaluation time — before vite empties
// outDir — and write it back once the bundle is closed.
function keepEmbedSentinel(): Plugin {
  const contents = existsSync(SENTINEL)
    ? readFileSync(SENTINEL, 'utf8')
    : SENTINEL_FALLBACK
  return {
    name: 'knomit-desktop-keep-embed-sentinel',
    apply: 'build',
    closeBundle() {
      writeFileSync(SENTINEL, contents)
    },
  }
}

export default defineConfig({
  plugins: [react(), keepEmbedSentinel()],
  // Assets are served from wails://localhost/desktop/, not from the origin
  // root, so every generated URL must be relative to the document. An absolute
  // "/assets/..." would resolve to the shared knowledge UI's tree and 404 into
  // its index.html SPA fallback.
  base: './',
  build: {
    rollupOptions: {
      // Two entry documents, one per window. Neither is named index.html on
      // purpose: the desktop tree is only ever reached by explicit path, and an
      // index.html here would invite the "just open /desktop/" habit.
      input: {
        settings: resolve(__dirname, 'settings.html'),
        logs: resolve(__dirname, 'logs.html'),
      },
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test-setup.ts'],
  },
})
