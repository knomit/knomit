# knomit web UI

The React + TypeScript single-page app that the knomit server embeds and serves
at `/`. Browse and search facts, trigger synthesis, and watch tasks update in
real time over SSE.

The same bundle is reused by the desktop app (Wails) and the cloud build; it
reads `window.__KNOMIT_API_BASE__` at runtime to find the API, so it works both
same-origin and against a looknomitck server.

## Development

Build and dev commands are driven from the repo root via `make`:

```sh
make web      # production build → web/dist (embedded into the Go binary)
make dev      # Vite dev server with HMR (expects a knomit server running)
```

Tooling: [Vite](https://vite.dev) + [@vitejs/plugin-react](https://github.com/vitejs/vite-plugin-react/tree/main/packages/plugin-react),
TypeScript, and ESLint. Run `npm install` here first if you build directly with
`npm` instead of `make`.

See **[knomit.io/docs](https://knomit.io/docs)** for the product documentation.
