// Runtime API base. Served as a static file in the cloud build (UI and API are
// same-origin, so the base is empty and API URLs stay relative). The desktop
// (Wails) build overrides /config.js dynamically with the looknomitck API URL.
window.__KNOMIT_API_BASE__ = "";
