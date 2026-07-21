import { existsSync, readFileSync, writeFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { defineConfig, type Plugin } from 'vitest/config'
import react from '@vitejs/plugin-react'

const SENTINEL = resolve(__dirname, 'dist/.gitkeep')

const SENTINEL_FALLBACK = `# Keeps the web/dist/ directory present so \`//go:embed dist\` compiles on a
# fresh checkout. Real assets are produced by \`npm run build\` (see Makefile
# \`web\` target) and are intentionally not tracked.
`

// web/dist/.gitkeep is committed so `//go:embed all:dist` in web/embed.go
// compiles on a fresh checkout (CI builds Go directly, without `make web`).
// vite's default emptyOutDir wipes dist/ — including the sentinel — leaving a
// deleted tracked file and a red `go test ./web/`. Emptying is still what we
// want (stale hashed assets would otherwise accumulate into the embedded FS),
// so instead of disabling it we snapshot the sentinel before the build and
// write it back afterwards. Reading the existing file keeps the two copies from
// drifting; the fallback covers a build that runs when it is already missing.
function keepEmbedSentinel(): Plugin {
  // Snapshot at config-evaluation time: the config module is loaded before vite
  // empties outDir, whereas buildStart is not guaranteed to run before it.
  const contents = existsSync(SENTINEL)
    ? readFileSync(SENTINEL, 'utf8')
    : SENTINEL_FALLBACK
  return {
    name: 'knomit-keep-embed-sentinel',
    apply: 'build',
    closeBundle() {
      writeFileSync(SENTINEL, contents)
    },
  }
}

// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), keepEmbedSentinel()],
  server: {
    proxy: {
      '/api': {
        target: 'http://localhost:19278',
        // Prevent Accept-Encoding: gzip on proxied requests — compression
        // buffers SSE event streams, breaking real-time updates.
        headers: { 'Accept-Encoding': 'identity' },
      },
      '/mcp': 'http://localhost:19278',
      '/git': 'http://localhost:19278',
      '/docs': 'http://localhost:19278',
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test-setup.ts'],
  },
})
