// Runtime API base. Served as a static file in the cloud build; the desktop
// (Wails) build never reads this file — configInjectingHandler in
// tools/desktop/app.go answers /config.js ahead of the static FS with an
// absolute http://127.0.0.1:<port> base and the __KNOMIT_DESKTOP__ flag.
//
// Derived from the document location rather than hardcoded to "", so one bundle
// serves both a same-origin deployment and one mounted under a path prefix by a
// prefix-stripping reverse proxy:
//
//   served at /                -> ""            -> /api/v1/repos
//   served at /proxy/19278/    -> "/proxy/19278" -> /proxy/19278/api/v1/repos
//
// `new URL('.', ...)` is the directory of the document, so it honours the <base>
// written by the trailing-slash guard in index.html. The trailing slash is
// stripped because every caller in web/src/api.ts appends a path that starts
// with one.
window.__KNOMIT_API_BASE__ =
  new URL('.', document.baseURI).pathname.replace(/\/$/, '');
