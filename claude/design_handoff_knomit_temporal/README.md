# Handoff: knomit · Temporal navigation + status footer (API-truthed)

## Overview
This bundle adds **temporal navigation** to knomit: every fact and edge
has a lifetime, and the UI exposes a single **time-anchor** control
that re-renders panels as-of any commit. It also restores the bottom
**console + status strip** as one unified footer that doubles as the
time-anchor read-out.

This README is **API-truthed** against the OpenAPI spec
(`Knomit API v2.0.0`, HAL/HATEOAS, RFC 9457 problem+json). Every
screen-level claim is mapped to a concrete `operationId`. Features
that need backend support that does not yet exist are flagged
**BLOCKED · BACKEND** with the missing endpoint named.

## About the design files
The files in this bundle are **HTML/JSX design references** —
prototypes, not production code. Implement the same UX in the real
knomit codebase using its existing component library, design tokens,
and HAL client. Treat the JSX as faithful pseudocode for layout, color,
and interaction.

## Fidelity
**High-fidelity.** Colors, type scale, and copy are intended to be
pixel-accurate. Tokens in `tokens.jsx` are extracted from the actual
knomit source — use them verbatim.

---

## Files in this bundle
| File | Purpose |
|---|---|
| `knomit-ux.html` | Top-level canvas. Composes every screen as artboards. |
| `tokens.jsx` | `THEME_A`/`THEME_B`/`THEME_L`, `TYPE_STYLES`, `OP_STYLES`, sample data. `SAMPLE_COMMITS` now uses the real `FileCounts` shape (`{ added, modified, deleted }`). |
| `chrome.jsx` | `<TopBar>`, `<Breadcrumb>`, `<FilterBar>`, `<TypeIcon>`, `<OpIcon>`, icon set `I.*`. |
| `screens.jsx` | `<Shell>`, `<StatusFooter>`, `<TreeList>` (scrubbed-aware), `<HistoryList>` (interactive scrubber), `<ChronoList>`, `<FactDetail>`, `<CommitHeader>`, `<FactEditor>`. |
| `temporal.jsx` | The six temporal artboards. Source of truth for scrubbed/diff visual treatment. |
| `design-canvas.jsx` | Canvas host — not relevant to production. |

---

## Backend contract assumptions

The UI is built against `Knomit API v2.0.0`. The relevant ops:

| What the UI needs | `operationId` | Path |
|---|---|---|
| Tree at HEAD | `listTopics`, `getTopic` | `/repos/{repo}/branches/{branch}/topics[/{path}]` |
| Tree at commit | `getTopicsAtCommit`, `getTopicAtCommit` | `/repos/{repo}/branches/{branch}/commits/{sha}/topics[/{path}]` — **returns 501** |
| Fact at HEAD | `getFact` | `/repos/{repo}/branches/{branch}/facts/{path}` |
| Fact at commit | `getFactAtCommit` | `/repos/{repo}/branches/{branch}/commits/{sha}/facts/{path}` |
| Edges at HEAD | `getFact` w/ `/incoming` or `/outgoing` suffix → `RefsCollection` |
| Edges at commit | `getFactAtCommit` w/ `/incoming` or `/outgoing` suffix → `RefsCollection` |
| Recent (chrono) | `listFacts` | `/repos/{repo}/branches/{branch}/facts` (`FactSummary` carries `committed_at`, `operation`) |
| History | `listCommits`, `getCommit` | `/repos/{repo}/branches/{branch}/commits[/{sha}]` |
| Branch HEAD | `getBranch` | `/repos/{repo}/branches/{branch}` (returns `head` SHA) |

Every `Ref` (in `RefsCollection`) carries:
- `commit` — pinning the edge to a specific source/target version.
- `deleted` (outgoing only) — true if the target is soft-deleted at the
  anchor; `_links.self` is omitted in that case.

`CommitSummary.files` is `{ added, modified, deleted }` integer counts.
`CommitDetail.files[]` is a list of `{ path, action, title }` per file.

**No diff endpoint exists.** A fact diff is computed client-side from
two `getFactAtCommit` calls.

**No edge counts at commit-summary level.** `CommitSummary` only counts
files. Edge deltas, if needed, must be reconstructed by diffing two
`RefsCollection` snapshots.

---

## Design tokens (verbatim from `THEME_A`)

```
bg          #0b0b0d
surface     #111114
surface2    #16161b
surface3    #1c1c22
line        #1f1f26
lineStrong  #2a2a33
text        #e6e6ea
textDim     #a0a0a8
textMute    #5a5a65
textFaint   #3a3a42
accent      #7c9        primary accent (LIVE green)
accentBlue  #8af        commit hash, modified (~) marker
accentPink  #f8a
accentPeach #fa8
amber       #e5a23c     ★ scrubbed/diff signal
```

`#e5a23c` is **the** "you are reading from history" signal: SCRUBBED/DIFF
status pill, breadcrumb rule, scrub dot in history rail, range tint, and
all `+`/`−` glyphs in the scrubbed tree.

Type: `'Inter', system-ui, sans-serif` for sans;
`'JetBrains Mono', ui-monospace, monospace` for mono.

---

## Per-component changes

### 1 · `<StatusFooter>` *(new — replaces `<ConsoleStrip>` in `<Shell>`)*

**File:** `screens.jsx`. Height 26 px, `flexShrink: 0`,
`borderTop: 1px solid t.line`, `background: t.bg`.

Single horizontal strip:
1. Time-anchor pill: dot + `LIVE` (green `#7c9`, glow) / `SCRUBBED`
   (amber `#e5a23c`) / `DIFF` (amber).
2. `│` separator in `t.line`.
3. Console roll-up: `Console`, info count, `N err` if errors,
   `[op] task.msg` if running.
4. Spacer.
5. Keyboard hints in mono 10px: `t scrub  h HEAD  / search  ⌘K palette`,
   each key glyph in a 1-px-bordered chip.
6. Trailing `<I.ChevUp>` to suggest the strip is expandable.

Props:
```ts
type AsOf =
  | { mode: 'live' }
  | { mode: 'scrubbed'; commit: string }
  | { mode: 'diff'; from: string; to: string };

type StatusFooterProps = {
  asOf?: AsOf | null;
  info?: number;
  errors?: number;
  task?: { op: string; msg: string } | null;
};
```

`<Shell>` accepts `asOf` and forwards it. Default is `{ mode: 'live' }`.

### 2 · `<TreeList>` updates *(scrubbed-aware glyphs)*

**File:** `screens.jsx`. New prop: `scrubbed?: boolean`.

When `scrubbed === true`:
- Header: `16 items @ <commit> · 3 dirs`, right-aligned amber line
  `N differ from HEAD`.
- Each row carries one of three states (`present` / `future` /
  `deleted`); `future` shows `+` glyph, `deleted` shows `−` glyph and
  strikethrough; both at opacity 0.45.

**BLOCKED · BACKEND.** This view requires `getTopicsAtCommit`, which
currently returns `501 Not Implemented`. The UI is built; it cannot
populate real data until the backend lands either:
- `getTopicsAtCommit` returning a `TopicsCollection` with per-node
  `state: 'present' | 'future' | 'deleted'`, **or**
- per-node lifecycle fields (`created_at_commit`, `retracted_at_commit`)
  on `TopicNode` so the client can derive state.

Until then the artboard renders sample data with a banner that says so.

**Removed:** the per-row "last-touched" sparkline that the prior README
described. There is no per-node last-touched commit in `TopicNode`.
(`getActivity?path=` returns `last_commit` per path but is too
expensive to call once per row.)

### 3 · `<HistoryList>` updates *(timeline = scrubber)*

**File:** `screens.jsx`. New props:
```ts
type HistoryListProps = {
  selected?: number;
  scrubAt?: number;     // index of the row currently anchored as as-of
  rangeFrom?: number;
  rangeTo?: number;
};
```

Row chrome unchanged except:
- The dot at `scrubAt` is larger (12 px), amber, with `box-shadow: 0 0
  0 3px t.bg, 0 0 8px #e5a23c`.
- Rows in `[min(rangeFrom, rangeTo), max(...)]` get
  `rgba(229, 162, 60, 0.04)` background; rail segments tinted amber.
- HEAD (index 0) gains a `HEAD` chip in `t.accent`.
- Scrub row gains an `AT` chip in amber.
- **Right-side count chip is now real `FileCounts`:**
  `+A` (green `accent`) `~M` (blue `accentBlue`) `−D` (red `#f88`),
  pulled directly from `CommitSummary.files`. **No edge counts** —
  the API does not provide them at the commit-summary level.

In production: clicking a row → `setAsOf({ mode: 'scrubbed', commit: row.commit })`.
⌥+click on a second row → `setAsOf({ mode: 'diff', from: prev.commit, to: row.commit })`.

### 4 · `<HistoryScreen>` (composition)

**File:** `knomit-ux.html`. Accepts `asOf` prop, forwards to `<Shell>`
and to `<HistoryList>`. The artboard demonstrates the scrubbed visual
state with `asOf={{ mode: 'scrubbed', commit: 'b812d40' }}` and
`<HistoryList scrubAt={2} rangeFrom={4} rangeTo={2}>`.

### 5 · `<SplitScreen>` (composition)

**File:** `knomit-ux.html`. Forwards `asOf` to its `<Shell>`. Tree and
Recent artboards leave `asOf` unset → render in `live` mode.

---

## Behavior contract for production wiring

One global `AsOf` value, lifted into your existing global UI state.

> **2026-05-07 — implementation rename:** the gesture is implemented as
> `⌥+click`, not `⌥+drag`. The legend, code, and spec all use `⌥+click`.
> The single-click semantics make the affordance simpler to hit on
> trackpads and easier to undo (plain click in diff mode demotes to
> scrubbed at the clicked row, exiting the range).

**Mutators:**
- Click any row in `HistoryList` → `setAsOf({ mode: 'scrubbed', commit: row.commit })`.
- ⌥+click second row → `setAsOf({ mode: 'diff', from, to })`.
- `t` → open scrub picker.
- `h` → `setAsOf({ mode: 'live' })`.
- `⌘[` / `⌘]` → walk current fact through versions (see "Open questions").
- Filter chip `at:<commit>` → behaves like scrubbed.
- Filter chip `vs:<from>..<to>` (formerly `between:`) → **fact-scoped
  diff**, not a global filter. See "Filter scope" below.

**Read points (each panel includes the anchor in its query):**
- Tree at HEAD → `listTopics` / `getTopic`.
- Tree at scrubbed commit → `getTopicsAtCommit` / `getTopicAtCommit`
  (**501 today**).
- Fact body → `getFactAtCommit` (use HEAD's SHA for `live` mode, or
  the scrubbed SHA otherwise). For `diff` mode, two calls
  (FROM and TO) and diff client-side.
- Edges → `getFactAtCommit` with `/incoming` or `/outgoing` suffix.
  The returned `Ref` entries already carry `commit` and `deleted` —
  use those verbatim for "since cN" chips and ghost-rendering.
- History list → `listCommits` (paginates with `after` cursor; supports
  `from`/`before` for windowing).

**Read-only enforcement.** When `mode !== 'live'`, disable: PUT
(`updateFact`), POST (`createFact`), DELETE (`deleteFact`), and any UI
that would call them. The mutation endpoints accept no commit
parameter — they only ever write to HEAD.

**Breadcrumb rule.** When `mode !== 'live'`, render a 1-px amber rule
under the breadcrumb. Mirrored from the temporal artboards.

---

## Filter scope: `at:` vs `vs:` (was `between:`)

The earlier mock had a `between:` filter chip on the global filter bar.
That doesn't map to anything in the API: `listFacts` accepts no commit
parameter. The truthful split:

- **`at:<commit>`** is a *global* time-anchor. Every read panel
  re-issues with the new SHA. Equivalent to `setAsOf({ mode: 'scrubbed' })`.
- **`vs:<from>..<to>`** (renamed from `between:`) is **fact-scoped**.
  It only makes sense on a single fact's diff view, executed as two
  `getFactAtCommit` calls.

The `TimeFilterScreen` artboard now reflects this: the picker dropdown
labels `between:` as "fact-scoped (diff one fact)" and the bottom-of-page
query block shows the two `getFactAtCommit` calls explicitly. Production
should rename to `vs:` to avoid implying it can scope a list query.

---

## Reconstructing things the API doesn't provide

| Need | Approach | Cost |
|---|---|---|
| Per-fact version list | Walk `listCommits` and call `getFactAtCommit` per row, keeping ones whose body changed. Or filter `listCommits` by `from`/`before` then call `getCommit` and inspect `files[].path`. | O(N) requests per fact |
| Per-edge lifetime rail (Explain view) | Call `getFactAtCommit/(incoming|outgoing)` at multiple anchors, diff client-side. | O(K) requests per render |
| Edge delta per commit | Diff `RefsCollection` at `c` vs `c^` for the relevant fact. | 2 requests per commit row |
| Tree at-of-commit | **No client-side workaround that's correct.** Approximating by walking commits and applying `CommitDetail.files[]` changes is fragile (depends on ordering, doesn't see directory creates). Wait for `getTopicsAtCommit`. |

For an MVP, only the first row is worth doing on the client. The
others should wait for backend support.

---

## Suggested implementation order

1. **`AsOf` type + global store entry.** Default `{ mode: 'live' }`.
2. **`StatusFooter`.** Pure presentational; reads anchor from store.
3. **`HistoryList` interactive.** Click to set anchor; visual chrome
   from `screens.jsx`. Backed entirely by `listCommits` (already exists).
4. **`getFactAtCommit` plumbing.** Wire fact-detail panels to fetch via
   commit-anchored URL when `mode !== 'live'`. The HAL `_links.self`
   on a fact at commit is already commit-anchored — follow the link.
5. **Read-only guards.** Disable mutation buttons when `mode !== 'live'`.
6. **Filter chip `at:`.** Sets the global anchor; nothing else changes.
7. **Keyboard shortcuts** `t`, `h`, `⌘[`, `⌘]`.
8. **Edge inspector + Explain edges (current state).** Use `commit` and
   `deleted` fields on `Ref`. No backend changes needed.
9. **Fact diff view (`vs:` chip).** Two `getFactAtCommit` calls,
   client-side diff. Edge diff column = compare two `RefsCollection`s
   by path.
10. (Stretch) **Time anchor picker (`t` shortcut).** Dropdown over the
    history rail, same data source.
11. (BLOCKED) **Scrubbed tree.** Wait for `getTopicsAtCommit` (today
    `501`). Or build with the fragile `CommitDetail.files[]` walk and
    accept the caveats.

---

## Open questions

- **What does `⌘[` walk through?** The footer copy says "walk
  versions," meaning the previous version of the *currently selected
  fact*. Implementing this requires the per-fact version list above
  (walk `listCommits` calling `getFactAtCommit`). Cache aggressively.
- **`vs:` mode without a selected fact.** If the user enters
  `vs:c4f1..c9a7` without picking a fact, the diff has no subject. UX
  options: (a) show a roll-up of `CommitDetail.files[]` paths in the
  range and let the user pick one to diff, or (b) prompt explicitly.
- **HAL navigation.** Wherever possible, follow `_links.self`,
  `_links.incoming`, `_links.outgoing`, `_links.history` rather than
  build URLs by hand. The fact-at-commit response's `_links.self` is
  already commit-anchored; reuse it.
- **Origin/agent branch indicator.** `Branch.is_agent_branch` exists.
  The current top-bar shows `agent/main` literally; in production read
  this from the `getBranch` response.

---

## Assets
No new images, icons, or fonts. Glyphs are inline SVG (`I.ChevUp`,
`I.Folder`, `TypeIcon`, `OpIcon`) from `chrome.jsx`. Fonts are existing
knomit fonts (Inter, JetBrains Mono).
