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


// Forwarding the DEV server out of a code-server session.
//
// `npm run dev` on its own serves http://localhost:5173/ and needs none of
// this. Reaching that same dev server through code-server's port forwarder
// does, and three separate things have to line up. Point KNOMIT_DEV_BASE at the
// mount path (`npm run dev:proxy` sets the code-server default) and all three
// follow from it:
//
//  1. code-server dials a forwarded port on 0.0.0.0, not 127.0.0.1 — against
//     vite's default localhost bind it fails before it ever speaks HTTP, with
//     `connect ECONNREFUSED 0.0.0.0:5173`.
//  2. vite refuses any request whose Host header is not localhost, so the
//     session's own hostname comes back 403 "This host is not allowed".
//  3. `base: './'` below is BUILD-only — in dev vite serves at '/' and injects
//     ABSOLUTE `/@vite/client` and `/@react-refresh` script tags. Under a mount
//     those resolve against the origin root, reach code-server instead of vite,
//     and the page never boots. The dev base has to BE the mount path.
//
// Use code-server's `/absproxy/<port>/` route, not `/proxy/<port>/`. /proxy
// strips the prefix before forwarding, which puts vite back at '/' with no way
// to emit the prefix the browser needs for (3). /absproxy forwards the path
// intact — which is exactly what a `base` wants — at the cost of the dev API
// proxy rules having to carry, and then strip, the same prefix.
//
// Normalized to a leading slash and no trailing one, the shape public/config.js
// derives for the API base, so the two agree on what the mount is.
function devMount(): string {
  const raw = process.env.KNOMIT_DEV_BASE?.trim()
  if (!raw) return ''
  const trimmed = raw.replace(/\/+$/, '')
  return trimmed.startsWith('/') ? trimmed : `/${trimmed}`
}

const API_TARGET = 'http://localhost:19278'

// Dev proxy to the Go server. The keys carry the mount because vite matches
// them against the RAW request path, and under a mount the app asks for
// `<mount>/api/...`: public/config.js derives its API base from
// document.baseURI, so every call inherits the prefix. `rewrite` takes the
// prefix back off — the Go server knows nothing about the mount.
function devProxy(mount: string) {
  const rewrite = mount ? (path: string) => path.slice(mount.length) : undefined
  const plain = (path: string) =>
    [`${mount}${path}`, { target: API_TARGET, rewrite }] as const
  return Object.fromEntries([
    [
      `${mount}/api`,
      {
        target: API_TARGET,
        // Prevent Accept-Encoding: gzip on proxied requests — compression
        // buffers SSE event streams, breaking real-time updates.
        headers: { 'Accept-Encoding': 'identity' },
        rewrite,
      },
    ],
    plain('/mcp'),
    plain('/git'),
    plain('/docs'),
  ])
}

// https://vite.dev/config/
export default defineConfig(({ command }) => {
  // Build output stays mount-agnostic; only the dev server is ever pinned to a
  // path, so a stray KNOMIT_DEV_BASE in the environment cannot bake a mount
  // into `npm run build`.
  const mount = command === 'serve' ? devMount() : ''
  return {
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
    //
    // Dev cannot use './': vite resolves a relative base to '/' when it serves,
    // and injects its client at an absolute path. See devMount above.
    base: mount ? `${mount}/` : './',
    plugins: [react(), keepEmbedSentinel()],
    build: {
      rollupOptions: { output: { manualChunks: vendorChunk } },
    },
    server: {
      // Mounted only: bind every interface for (1), stand down the host check
      // for (2). A plain `npm run dev` keeps vite's defaults — localhost-only,
      // host check armed — so neither loosening is ever on by accident.
      // strictPort because the mount path names a port: silently landing on
      // 5174 would serve a base that points at nothing.
      ...(mount ? { host: true, allowedHosts: true, strictPort: true } : {}),
      proxy: devProxy(mount),
    },
    test: {
      environment: 'jsdom',
      setupFiles: ['./src/test-setup.ts'],
    },
  }
})
