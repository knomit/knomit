// Placeholder for the live log viewer. Kept separate from logs.tsx (which only
// mounts it) so it can be rendered in a test without a real #root element.
export function LogsApp() {
  return <h1>Logs</h1>
}
