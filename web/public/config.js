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
// `new URL('.', ...)` is the DIRECTORY of the document, which is why index.html
// carries a guard that redirects a prefixed mount missing its trailing slash:
// without it this reads /proxy/19278 as the directory /proxy, and every API call
// goes to /proxy/api/v1/... — a wrong base, not a missing one, so nothing points
// at the cause. Do not remove that guard on the assumption that something else
// normalizes the directory; nothing does. (An earlier draft wrote a <base href>
// instead of redirecting, and this comment used to claim one exists. It does
// not.) The trailing slash is stripped because every caller in web/src/api.ts
// appends a path that starts with one.
window.__KNOMIT_API_BASE__ =
  new URL('.', document.baseURI).pathname.replace(/\/$/, '');
