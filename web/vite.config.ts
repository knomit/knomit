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

// The app shipped as ONE 548 kB chunk: React, the whole unified/micromark
// markdown stack, and every component in a single file that changes its hash
// whenever any line of app code does. Splitting the two vendor stacks out puts
// each chunk comfortably under rollup's 500 kB advisory and, more usefully,
// stops an edit to a component from invalidating 400 kB of dependencies that
// did not change.
//
// NOT lazy-loaded. Deferring react-markdown behind a dynamic import would trim
// the initial payload further, but the fact body is the first thing this app
// draws — the split would buy a flash of empty prose on open, and buy it
// against a bundle served from the Go binary's embedded FS over localhost,
// where the transfer was never the cost.
function vendorChunk(id: string): string | undefined {
  if (!id.includes('node_modules')) return undefined
  // react-dom pulls in scheduler; keeping them together avoids a third chunk
  // that is nothing but a shared runtime.
  if (/[\\/]node_modules[\\/](react|react-dom|scheduler)[\\/]/.test(id)) return 'react'
  return 'vendor'
}

// https://vite.dev/config/
export default defineConfig({
  // Relative asset URLs, so the bundle can be mounted under a path prefix.
  // A reverse proxy (code-server's /proxy/<port>/, an nginx `location`) strips
  // the prefix before forwarding, so the server still sees `/`-rooted paths and
  // needs no prefix awareness — but the HTML it returns did, because vite's
  // default `base: '/'` writes `/assets/index-<hash>.js` and the browser
  // resolves that against the ORIGIN root, which belongs to the proxy. Every
  // asset 404s and the page is blank.
  //
  // The usual objection to './' is deep links: a document at /a/b/ resolves
  // './assets/x.js' against /a/b/, not the mount root. It does not apply here
  // because the app has NO router — no react-router, no history.pushState — so
  // the document URL never changes after load and is always the mount root.
  // Adding a router later means revisiting this.
  base: './',
  plugins: [react(), keepEmbedSentinel()],
  build: {
    rollupOptions: { output: { manualChunks: vendorChunk } },
  },
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
