# Knomit Web UI Specification

**Version**: 1.0
**Target**: Go rewrite of knomit (Git-backed knowledge base)
**Purpose**: Replace the terminal TUI with a Web UI served directly from the Go binary

---

## Overview

The Web UI replicates and extends the knomit TUI in a browser context. The TUI was keyboard-driven with a split-panel layout; the Web UI is mouse-and-keyboard driven with the same conceptual split-panel layout, but leverages browser affordances (URLs, back button, tabs) to replace the TUI's in-memory navigation stack.

All client state that the TUI kept in memory (currentPath, selectedFact, historyMode, navStack, etc.) is encoded in the URL so that navigation is bookmarkable and the browser back/forward buttons work natively.

---

## 1. Layout

### Overall Page Structure

The page is a full-viewport application shell divided into four horizontal regions stacked top to bottom:

```
+------------------------------------------------------------------+
| HEADER: logo | breadcrumb path            | sync status | hash  |
+------------------------------------------------------------------+
| LEFT PANEL (list)        | RIGHT PANEL (detail)                  |
|                          |                                       |
|                          |                                       |
|  [scrollable]            |  [scrollable]                         |
|                          |                                       |
+------------------------------------------------------------------+
| STATUS BAR: embeddings indicator | active operation label        |
+------------------------------------------------------------------+
```

The main area (left + right panels) fills all remaining vertical space between the header and status bar using CSS flexbox with `height: 100vh` minus the heights of header and status bar.

### Header (persistent)

Fixed at the top of the viewport. Contains (left to right):

- **Logo / title**: "knomit" wordmark or icon, links to root (`/`)
- **Breadcrumb**: full path from root, rendered as clickable segments (see Section 7)
- **Sync button**: manual sync trigger (see Section 8)
- **Sync status indicator**: idle / syncing spinner / last commit hash (see Section 8)

Height: approximately 48px. Does not scroll.

### Main Two-Panel Area

The left and right panels sit side by side in a flex row. A draggable divider between them lets the user resize the panels; default split is 35% left / 65% right. Panel widths are persisted in `localStorage`.

Both panels have independent vertical scroll. They do not scroll together.

### Status Bar (persistent)

Fixed at the bottom of the viewport. Contains (left to right):

- **Embeddings indicator**: shows whether semantic search (embeddings) is enabled. Text label such as "Embeddings: on" or "Embeddings: off". Styled subtly (muted color).
- **Active operation label**: when a background operation is in progress (sync, index rebuild), shows a short label such as "Rebuilding index…" with a small spinner icon. Hidden when idle.

Height: approximately 28px.

### Responsive Behavior

At viewport widths below 768px (mobile/narrow):

- The two panels stack vertically instead of side by side.
- By default the left panel is shown (browse/search list).
- When the user selects a fact or a world that has content to show, the right panel replaces the left panel in the view, and a "Back" affordance (arrow button or swipe gesture) returns to the left panel.
- The header breadcrumb truncates from the left, showing only the last two segments, with a "..." prefix that expands on tap.
- The status bar remains at the bottom on all viewport widths.

---

## 2. Left Panel — Browse Mode

Browse mode is active when no search query is present. The left panel shows the children of the currently selected path (`currentPath`).

### Directory Listing

Each entry in the list represents either a **world** (subdirectory) or a **fact** (markdown file).

Entry layout (single row):

```
[icon]  [name]                    [optional summary text]
```

- **Icon**: folder icon for worlds; document icon for facts. Icons are visually distinct enough to tell them apart at a glance without reading the name.
- **Name**: directory name (for worlds) or fact title derived from frontmatter (for facts). If the title is absent, fall back to the filename without extension.
- **Optional summary**: for facts, the first ~80 characters of the body. For worlds, a subdirectory count such as "3 worlds, 12 facts" if the API returns it. This is secondary text, displayed in a smaller, muted style.

Worlds and facts may optionally be grouped into two sections (Worlds first, then Facts) with a subtle divider and section label between them. Grouping is a display-level decision; the API returns a flat list and the client separates them by type.

### Selection and Interaction

- **Hover**: highlights the row with a light background tint.
- **Single click on a fact**: selects the fact (highlights the row) and loads the fact content in the right panel (fact view). Does not change `currentPath`.
- **Single click on a world**: selects the world (highlights the row) and loads the summary stats for that world in the right panel (summary view). Does not change `currentPath`. This matches the TUI behavior where pressing Enter on a world shows its summary before navigating into it.
- **Double click on a world** (or pressing Enter when a world row is focused): navigates into the world — sets `currentPath` to that world's path, updates the URL, and reloads the left panel list.
- **Keyboard navigation**: `j` / `k` or `ArrowDown` / `ArrowUp` move the highlight between rows when the left panel has focus. `Enter` activates the selected row (same as click/double-click per item type). Pressing `Enter` on a fact shows it; pressing `Enter` once on a world shows summary, pressing `Enter` again navigates into it (or `Enter` with modifier, see below).
  - Alternatively (simpler): `Enter` on a world navigates into it immediately, since the summary is also shown on single click. This avoids the two-press ambiguity.

### "Go Up" Affordance

When `currentPath` is deeper than the root (`know`), the first item in the list is a special "parent directory" row:

```
[up-arrow icon]  ..  (parent segment name)
```

Clicking this row navigates to the parent path. This replaces the TUI's `u` / `Backspace` keys. The breadcrumb (in the header) also provides the same navigation.

### Empty State

If the current path contains no children, show a centered empty-state message: "No items in this path."

---

## 3. Left Panel — Search Mode

### Search Input

A search input field is always visible at the top of the left panel, above the list. It is not hidden or revealed by a key — it is always present and accessible via mouse click or the `/` keyboard shortcut (which focuses the input).

Placeholder text: "Search facts…"

A small toggle or tab control adjacent to the input switches between two search modes:
- **Text** (default): full-text search against fact titles and bodies
- **Domain**: filter facts by domain tag

This replaces the TUI's `/` (text search) and `d` (domain search) entry keys.

An optional **minimum confidence** slider or numeric input may be exposed below the search input to let users filter by confidence score. Default: 0 (no minimum).

### Entering Search Mode

Typing in the search input activates search mode. The directory listing is replaced by the search results list. The current browse state (path, selected item) is saved so it can be restored when search is cleared.

The URL should reflect search state, e.g., `?q=foo&type=text`.

### Search Results List

Each result row:

```
[document icon]  [title]                    [score badge]
                 [path/to/fact]
```

- **Title**: fact title from frontmatter or filename.
- **Score badge**: numeric relevance score (0.00–1.00) rendered as a small pill badge. Color-coded: green for high scores (>0.75), yellow for medium (0.50–0.75), gray for low (<0.50).
- **Path**: relative path under `know/`, rendered in muted monospace text.

Clicking a result row shows the fact in the right panel (fact view). The row is highlighted on selection.

### Clearing Search

- Clicking an `×` button inside the search input field clears the query.
- Pressing `Escape` while the input is focused clears the query and blurs the input.
- Both actions restore the previous browse state (the saved path and selection from before search was entered). This is the equivalent of the TUI's `Escape` key behavior.

---

## 4. Right Panel — Summary View

Shown when the user selects a world (directory) from the left panel, or when no fact is selected (e.g., on initial load for a path).

### Content

**Heading**: name of the selected world or current path.

**Stats grid** (4 cards in a 2×2 or 4×1 layout):

| Card | Content |
|------|---------|
| Total facts | Integer count of all facts in this subtree |
| Avg confidence | Percentage or decimal (e.g., "0.82") |
| Worlds | Count of direct subdirectories |
| Sources | Count of unique sources referenced |

**Top Domains**: horizontal bar chart or tag cloud showing the most frequent domain tags in this subtree. Each domain is a clickable tag — clicking it activates domain search mode in the left panel with that domain pre-filled.

**Top Entities**: a list of the top N (e.g., 10) most frequently occurring entities in this subtree, displayed as a tag list or table with entity name and count.

If this is the root path and no stats are available yet, show a placeholder: "No facts indexed in this path."

---

## 5. Right Panel — Fact View

Shown when the user selects a fact from the left panel or from search results.

### Content Layout

**Title** (`<h1>`): fact title from frontmatter.

**Body**: fact body rendered as Markdown. Use a standard Markdown renderer. Code blocks use syntax highlighting. No unsafe HTML is rendered (sanitize output).

**Metadata section** (visually separated below the body, or in a collapsible sidebar):

- **Domain**: tag(s), each rendered as a clickable pill. Clicking a domain pill activates domain search in the left panel.
- **Confidence**: rendered as a horizontal progress bar with a numeric label (e.g., "0.85 / 1.00"). The bar fill color transitions from red (low) to green (high).
- **Sources**: count of sources with a disclosure triangle. Expanding shows the list of source strings.
- **Entities**: list of entity strings, each a clickable link that triggers a text search for that entity.

**Refs section** (visually separated, below metadata):

Heading: "References"

Each ref is one list item. Ref rendering depends on scheme:

- `knomit:blob/<hash>/<path>` — rendered as an internal link with a document icon. The link text is the path (last segment or full relative path). Clicking this link navigates to the fact at that path and pushes the current state onto the nav stack, then shows that fact's history view at that specific commit. This matches TUI behavior where following a ref pushes the nav stack.
- `https://...` or `http://...` — rendered as an external link with an external-link icon. Opens in a new tab (`target="_blank" rel="noopener"`).
- Any other scheme — rendered as plain monospace text (not a link).

**History button**: a button labeled "View History" (or a clock icon + label) in the fact view header or metadata section. Clicking it switches the right panel to history view for this fact. This replaces the TUI's `H` key.

---

## 6. Right Panel — History View

Shown when the user activates history mode for a fact (via the "View History" button or a URL parameter).

### Commit List

**Header**: "History: [fact title or path]"

A "Back to Fact" button returns to fact view.

Each row in the commit list:

```
[short hash]  [relative date]  [commit message]
```

- **Short hash**: 7-character abbreviated commit hash, rendered in monospace.
- **Relative date**: human-readable relative time (e.g., "3 days ago"). On hover, show the full ISO date as a tooltip.
- **Commit message**: full first line of the commit message, truncated with ellipsis if too long.

Rows are listed newest-first.

### Viewing a Historical Version

Clicking a commit row loads the fact content as it existed at that commit. The historical content is displayed in an inline preview area below the commit list, or in a modal overlay. The preview is read-only and clearly labeled with the commit hash and date to avoid confusion with the current version.

The historical preview renders title, body, and metadata the same way as the current fact view (Section 5), but with a prominent banner: "Viewing snapshot at [hash] — [date]" and a muted visual treatment (e.g., slightly desaturated background or a banner stripe).

The historical preview does not show refs as clickable links (they are read-only context).

The "Back to Fact" button in the header dismisses the history view and returns to the current fact view.

---

## 7. Navigation and Breadcrumbs

### Breadcrumb

The breadcrumb in the header shows the full path from root to the current location:

```
know  /  projects  /  myapp
```

Each segment is a clickable link that navigates to that path level. The last segment (current level) is not a link (or is styled differently to indicate it is the current location).

On narrow viewports: truncate from the left, showing `… / parent / current`, with the full path visible on hover as a tooltip.

### URL Structure

The URL encodes enough state to fully restore the view on reload or when sharing a link. Suggested URL scheme:

| State | URL |
|-------|-----|
| Browsing a path | `/browse/know/projects/myapp` |
| Browsing path + fact selected | `/browse/know/projects/myapp?fact=know/projects/myapp/intro.md` |
| Browsing path + fact + history | `/browse/know/projects/myapp?fact=know/projects/myapp/intro.md&view=history` |
| Browsing path + fact + historical commit | `/browse/know/projects/myapp?fact=know/projects/myapp/intro.md&view=history&commit=abc1234` |
| Search | `/search?q=query&type=text` |
| Domain search | `/search?q=domain-name&type=domain` |

All navigation (breadcrumb clicks, list navigation, search) uses `pushState` so the browser back/forward buttons move through the navigation history.

### Back Navigation and Nav Stack

The browser's native back/forward buttons serve as the primary back navigation. No explicit "Back" button is needed in the main UI except on narrow viewports.

When following a `knomit:blob/...` ref in fact view, the new state is pushed to the browser history, allowing the user to press Back to return to the referencing fact. This is the equivalent of the TUI's `navStack`.

### Deep Linking

Any URL matching the scheme above can be loaded directly (e.g., pasted into a browser). The server must serve the same single-page app shell for all `/browse/...` and `/search` routes, with the Go server routing them to the same HTML response.

---

## 8. Sync

### Sync Button

A "Sync" button is always present in the header. It is represented by a circular arrows icon, optionally with a label "Sync" on wider viewports.

Clicking the button:
1. Disables the button and shows a spinner in place of or beside the icon.
2. Issues `POST /api/sync` to the backend.
3. On success: updates the commit hash display in the header, shows a brief success notification ("Synced to abc1234f"), and refreshes the current view's data.
4. On conflict error: shows a dismissible error notification that lists the conflicting files. The user must resolve conflicts manually (outside the UI).
5. On network or server error: shows a brief error notification.

The spinner/loading state persists until the server responds. There is no timeout on the client side (sync may take time on large repos or slow connections).

This replaces the TUI's `r` key.

### Commit Hash Display

The last synced commit hash is displayed in the header as monospace text (7 characters). On hover, show the full hash as a tooltip. Clicking the hash could open the source repository URL if one is configured (optional enhancement; out of scope for initial version).

### "Last Synced" Timestamp

Below or beside the commit hash, show a relative timestamp of when the last sync completed (e.g., "synced 4 min ago"). This is derived from client-side state (stored in memory or `sessionStorage` when the sync response arrives). It updates in the background as time passes.

### Conflict Handling

If `POST /api/sync` returns a conflict error, the notification should clearly list the affected file paths and suggest the user resolve them via git CLI. No in-browser merge UI is provided.

---

## 9. Command Palette

The command palette replaces TUI command mode (`:` prefix). It is a modal overlay.

### Opening the Palette

- Keyboard shortcut: `Cmd+K` (macOS) / `Ctrl+K` (Windows/Linux)
- A "Commands" button in the header (optional, for discoverability)

### Palette UI

A centered modal with a text input at the top and a filtered list of commands below. Typing in the input filters the command list by fuzzy match on command names.

Available commands:

| Command | Description |
|---------|-------------|
| Rebuild index | Triggers `POST /api/index/rebuild`. Rebuilds the full-text and embedding search index from scratch. Shows progress in the status bar. Equivalent to TUI `:rebuild`. |
| Go to path | Opens a path input sub-prompt. The user types a path (e.g., `know/projects/myapp`) and presses Enter to navigate there. |
| Search by domain | Opens the left panel search input pre-configured in domain mode. |

Navigation within the palette: `ArrowUp` / `ArrowDown` to move between commands, `Enter` to execute the focused command, `Escape` to dismiss.

After executing a command that has side effects (rebuild, sync), the palette closes and the status bar shows progress.

---

## 10. REST API

The following endpoints are required from the Go backend. All endpoints use JSON responses. The base path for all API routes is `/api`.

---

### `GET /api/browse`

Returns the children of a given path.

**Query parameters**:
- `path` (required): path relative to repo root, e.g., `know/projects/myapp`

**Response**:

```json
{
  "path": "know/projects/myapp",
  "children": [
    {
      "name": "subworld",
      "type": "world",
      "path": "know/projects/myapp/subworld",
      "childCount": 3,
      "factCount": 7
    },
    {
      "name": "intro",
      "type": "fact",
      "path": "know/projects/myapp/intro.md",
      "title": "Introduction to myapp",
      "summary": "First 80 chars of body…"
    }
  ],
  "manifest": {
    "inherits": ["know/shared"],
    "description": "My application knowledge"
  }
}
```

Worlds appear before facts in the returned array (server-side ordering). Maps conceptually to the TUI's explore tool.

---

### `GET /api/fact`

Returns the full content and metadata for a single fact file.

**Query parameters**:
- `path` (required): path to the fact file, e.g., `know/projects/myapp/intro.md`

**Response**:

```json
{
  "path": "know/projects/myapp/intro.md",
  "title": "Introduction to myapp",
  "body": "Markdown body text…",
  "frontmatter": {
    "domain": "engineering",
    "confidence": 0.85,
    "sources": ["https://example.com/docs"],
    "entities": ["myapp", "Go"],
    "refs": ["knomit:blob/abc1234/know/other/fact.md", "https://example.com"]
  }
}
```

---

### `GET /api/fact/history`

Returns the git commit log for a single fact file.

**Query parameters**:
- `path` (required): path to the fact file

**Response**:

```json
{
  "path": "know/projects/myapp/intro.md",
  "commits": [
    {
      "hash": "abc1234f",
      "shortHash": "abc1234",
      "date": "2026-03-10T14:22:00Z",
      "message": "update: clarify architecture notes"
    }
  ]
}
```

Commits are returned newest-first.

---

### `GET /api/fact/at`

Returns the content of a fact file as it existed at a specific git commit.

**Query parameters**:
- `path` (required): path to the fact file
- `commit` (required): full or abbreviated commit hash

**Response**: same shape as `GET /api/fact` response, plus an additional field:

```json
{
  "commit": "abc1234f",
  "commitDate": "2026-03-10T14:22:00Z",
  ...same fields as /api/fact...
}
```

If the file did not exist at the given commit, returns HTTP 404.

---

### `GET /api/search`

Returns search results for a text or domain query.

**Query parameters**:
- `q` (required): search query string
- `type` (optional): `text` (default) or `domain`
- `min_confidence` (optional): float 0–1, default 0
- `limit` (optional): integer, default 20

**Response**:

```json
{
  "query": "go concurrency",
  "type": "text",
  "results": [
    {
      "path": "know/projects/myapp/goroutines.md",
      "title": "Goroutines and channels",
      "body": "Snippet of matching text…",
      "score": 0.91
    }
  ]
}
```

---

### `GET /api/stats`

Returns aggregate statistics for a path (used by the summary view).

**Query parameters**:
- `path` (required): path, e.g., `know/projects`

**Response**:

```json
{
  "path": "know/projects",
  "totalFacts": 42,
  "avgConfidence": 0.78,
  "worldCount": 5,
  "topDomains": [
    { "name": "engineering", "count": 18 },
    { "name": "architecture", "count": 9 }
  ],
  "topEntities": [
    { "name": "Go", "count": 23 },
    { "name": "PostgreSQL", "count": 11 }
  ]
}
```

---

### `POST /api/sync`

Triggers a pull and merge from `origin/main`. No request body required.

**Response on success** (HTTP 200):

```json
{
  "status": "ok",
  "commit": "def5678a",
  "message": "Synced to def5678a"
}
```

**Response on conflict** (HTTP 409):

```json
{
  "status": "conflict",
  "conflictingFiles": ["know/projects/myapp/intro.md"]
}
```

**Response on error** (HTTP 500):

```json
{
  "status": "error",
  "error": "git pull failed: <reason>"
}
```

---

### `POST /api/index/rebuild`

Triggers a full rebuild of the search index. This is a potentially long-running operation; the server may respond immediately with an accepted status and process in the background, or block until complete depending on implementation.

**Response** (HTTP 202 Accepted):

```json
{
  "status": "rebuilding"
}
```

The client polls `GET /api/status` or listens on the SSE stream to know when the rebuild completes.

---

### `GET /api/status`

Returns current server status. Used for polling and on initial page load.

**Response**:

```json
{
  "lastCommit": "def5678a",
  "embeddingsEnabled": true,
  "indexStats": {
    "factCount": 237,
    "lastRebuildAt": "2026-03-10T12:00:00Z",
    "rebuilding": false
  }
}
```

---

### `GET /api/events` (SSE stream)

See Section 11.

---

## 11. Real-time Updates

### Recommendation: Server-Sent Events (SSE)

**Recommendation**: use SSE (`GET /api/events`) rather than polling.

**Reasoning**:

Polling `GET /api/status` every N seconds is simpler to implement but has drawbacks: either the poll interval is short (high server load, wasted requests) or it is long (stale UI after a sync). The knomit backend is a single-binary server, making SSE trivial to implement — Go's `net/http` supports it natively with no external dependencies. SSE is a one-way push from server to client over a persistent HTTP connection, which fits this use case exactly (the server pushes commit hash changes; the client does not need to send real-time data).

WebSockets are overkill here since communication is server-to-client only.

### SSE Event Stream

Endpoint: `GET /api/events`

Content-Type: `text/event-stream`

The server keeps this connection open and pushes events as they occur. The client reconnects automatically if the connection drops (SSE reconnect is built into the browser `EventSource` API).

**Event types**:

| Event name | Payload | Triggered when |
|------------|---------|----------------|
| `sync_complete` | `{ "commit": "abc1234" }` | A sync finishes successfully |
| `sync_error` | `{ "error": "...", "conflictingFiles": [...] }` | A sync fails or conflicts |
| `index_rebuilt` | `{ "factCount": 237 }` | Index rebuild completes |
| `status` | full `GET /api/status` response | Periodic heartbeat (every 30s) |

**Client behavior on receiving `sync_complete`**:
1. Update the commit hash display in the header.
2. Update the "last synced" timestamp.
3. If the currently viewed path or fact was modified in the sync, reload its data. The server may include an optional `changedPaths` array in the `sync_complete` event to allow targeted refreshes; if absent, the client reloads the current view unconditionally.

**Client behavior on receiving `sync_error`**:
Show the conflict notification (same as the `POST /api/sync` conflict response in Section 8).

**Client behavior on receiving `index_rebuilt`**:
Update the status bar to remove the "Rebuilding index…" indicator. Optionally show a brief "Index ready" notification.

### Fallback

If SSE is not supported or the connection cannot be established, fall back to polling `GET /api/status` every 30 seconds.

---

## 12. Tech Stack Recommendation

### Recommendation: React + Vite, embedded in the Go binary via `embed.FS`

**Decision**: React + Vite (Option A).

**Reasoning**:

The Web UI has substantial client-side state that must remain consistent across interactions:

- Current browse path and selected item
- Search mode toggle and saved pre-search navigation state
- History mode for the right panel
- Navigation stack for following refs
- Panel focus (which panel is "active" for keyboard navigation)
- Pending/loading states per panel independently

Managing this state in HTMX requires storing it in DOM attributes and coordinating via HTML swap patterns. This becomes fragile at the complexity level of this UI, where two panels update independently, keyboard focus affects behavior, and navigation history must be tracked. The HTMX approach would likely require significant custom JavaScript anyway, negating the simplicity argument.

React's component model and hooks (or a lightweight state manager like Zustand) map cleanly to the TUI's state machine described in the overview. Each piece of TUI state (currentPath, rightPanelMode, searchActive, etc.) becomes a piece of React state. URL synchronization is straightforward with React Router.

**Build artifact embedding**: the Vite build produces static assets (`dist/`) that are embedded into the Go binary using `//go:embed dist`. The Go server serves them under `/` and proxies all non-API routes to `index.html` for client-side routing. No CDN or external asset hosting is needed.

**Dependency surface**: React 19, React Router 7, a Markdown renderer (e.g., `react-markdown` with `remark-gfm`), and a small charting library for the top-domains bar chart (e.g., Recharts or a pure-CSS approach). All dependencies are bundled at build time; the embedded binary has no runtime npm dependencies.

**Development workflow**: `vite dev` proxies `/api/...` requests to the running Go server. Frontend developers do not need to understand Go; they just run the Go binary in one terminal and Vite in another.

### Why not HTMX

HTMX would suit the UI if interactions were primarily "click a link, swap a section with server-rendered HTML." But the following requirements push beyond what HTMX handles cleanly:

- Independent keyboard navigation in two panels with focus tracking
- Client-side nav stack for ref-following (push/pop without server round-trips)
- Search input that restores prior state on clear
- Draggable panel divider with persisted widths
- SSE-driven partial refreshes of specific panels

These are all solvable in HTMX but would each require non-trivial custom JavaScript extensions, ultimately producing a codebase that is neither simple HTMX nor clean React.

---

## Appendix: TUI → Web UI Interaction Mapping

| TUI key/interaction | Web UI equivalent |
|---------------------|-------------------|
| `j` / `k` or arrow keys | Arrow keys when panel has focus; mouse hover |
| `Enter` on fact | Single click |
| `Enter` on world | Double click (or single click to preview, Enter/double to navigate) |
| `u` / `Backspace` | Click `..` first row, or click parent breadcrumb segment |
| `/` | Focus the search input (always visible) |
| `d` | Switch search toggle to "Domain" mode, focus input |
| `Escape` | Clear search input; restores prior browse state |
| `H` | "View History" button in fact view |
| `Tab` / `Shift+Tab` | Click to focus a panel; tab key moves between focusable elements in the panel |
| `r` | "Sync" button in header |
| `:` | `Cmd+K` / `Ctrl+K` command palette |
| `:rebuild` | Command palette → "Rebuild index" |
| `:quit` / `:exit` | Close the browser tab (no equivalent; server runs as daemon) |
| Selecting history entry | Click commit row in history list |
| Following a ref | Click the ref link in the refs section |
| Nav stack (push/pop) | Browser history (`pushState` / back button) |
