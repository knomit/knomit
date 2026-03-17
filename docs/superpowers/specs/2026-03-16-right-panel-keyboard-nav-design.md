# Right-panel keyboard navigation & commit fact-switcher

**Date:** 2026-03-16
**Status:** Approved

---

## Overview

Two related improvements:

1. **Commit fact-switcher** — when a commit touches multiple viewable facts, replace the flat clickable list in the right panel with a collapsed dropdown at the top. Keyboard (`j`/`k`) cycles through the facts immediately (live load). The rendered fact appears below.
2. **Right-panel element focus** — when the right panel has focus, `j`/`k` moves a focus ring through all interactive elements (fact-switcher items, domain pills, entity pills, ref links). `Enter` activates the focused element. `←` returns focus to the left panel.

The fact layout itself is **not changed**.

---

## Focus model

### Acquiring focus

- When the user presses `→` while the left panel has focus **and the selected item is a fact** (not a directory), focus transfers to the right panel. The first focusable element in the current view is pre-selected (index 0). In search mode all results are facts, so `→` always transfers focus in search mode.
- When `→` is pressed on a directory (browse mode only), it retains its existing behaviour (`activateSelected()` — navigate into the directory). No focus transfer.
- When the user clicks anywhere in the right panel, the right panel acquires focus starting at index 0 (first focusable element). Click-to-exact-element is out of scope.

### Releasing focus

- `←` anywhere in the right panel returns focus to the left panel.
- No other key releases right-panel focus.

### Key ownership

`rightPanelFocused` is the authoritative gate. When it is `true`:

- `RightPanel`'s keydown handler consumes `j`/`k`/`↑`/`↓`/`Enter`.
- `LeftPanel`'s keydown handler **must guard** against `j`/`k`/`↑`/`↓`/`Enter` — it skips them when `rightPanelFocused === true`.
- `HistoryTimeline`'s keydown handler **must also guard** against `j`/`k`/`↑`/`↓`/`Enter` when `rightPanelFocused === true`.

Both `LeftPanel` and `HistoryTimeline` already receive `state` as a prop, so they can read `state.rightPanelFocused`.

### Navigation

- `j` / `↓` — move to next focusable element (wraps at bottom back to top).
- `k` / `↑` — move to previous focusable element (wraps at top back to bottom).
- `Enter` — activate focused element (search for entity/domain, open ref URL, load fact from switcher, toggle dropdown).

### Focusable elements (in visual / DOM order)

In **commit view** (multi-file):

1. Fact-switcher trigger (Enter toggles dropdown; j/k while dropdown is closed cycles facts)
2. Domain pills in the rendered fact (if a fact is loaded)
3. Entity pills
4. URL reference links (non-URL refs are skipped — they have no action)

In **fact view** (browsing/history):

1. Domain pills
2. Entity pills
3. URL reference links
4. "Find similar" button

In **summary view**: no focusable elements — `→` does nothing.

### Focus index

`rightPanelFocusIdx` is kept in `RightPanel` **local state** (not in the reducer), since it depends on the local async content list. `rightPanelFocused` (boolean) lives in `AppState`.

**Reset rule:** whenever content changes (new fact loaded, new commit selected, mode change), `rightPanelFocusIdx` resets to `0`.

### Focus indicator

A coloured `outline` ring is added to the currently focused element. The outline colour matches the element's existing accent:

- Entity pills: `rgba(136,170,255,0.55)` (blue)
- Domain pills: `rgba(119,204,153,0.55)` (green)
- URL ref links: `rgba(136,170,255,0.55)` (blue)
- Fact-switcher trigger: background highlight (`#2a2a3a`), same as the existing selected-item style

No size, spacing, or layout changes.

---

## Commit fact-switcher

### Trigger

Shown only when the number of **viewable** files (i.e. `files.filter(f => f.action !== 'deleted').length > 1`) is greater than one. When there is exactly one viewable file, the existing auto-load behaviour is preserved with no switcher shown.

### Collapsed state

A single bar rendered above the fact body:

```text
[ A ]  agents/memory-patterns          2 / 4  ▾
```

- Action badge (`A` / `M` / `D`) with existing colour coding (green/blue/red).
- Fact path, `.md` stripped.
- Position counter `n / total` where total = total viewable files.
- `▾` / `▴` chevron indicating open/closed.

### Expanded state (dropdown)

Appears below the trigger bar, listing all files (viewable and deleted):

- Each item: action badge + path.
- Currently loaded fact is highlighted (`background: #2a2a3a`).
- Deleted facts are shown greyed out (`opacity: 0.4`) and are not clickable.
- Clicking an item loads it and closes the dropdown.

### Keyboard behaviour

- `j`/`k` while the right panel has focus and the dropdown is **closed** cycles through the **viewable** facts (skipping deleted ones). Each step:
  1. Updates the collapsed bar (path, counter, badge).
  2. Immediately fetches and renders the fact below.
  3. Resets `rightPanelFocusIdx` to `0` (switcher trigger remains focused).
- `Enter` on the switcher trigger toggles the dropdown open/closed.
- `Enter` on a dropdown item loads that fact and closes the dropdown.
- When the dropdown is **open**, `j`/`k` moves the highlight within the dropdown list rather than cycling the fact content.

---

## State changes

### `AppState` (`state.ts`)

Add one boolean field:

```ts
rightPanelFocused: boolean  // default: false
```

New actions:

```ts
{ type: 'FOCUS_RIGHT_PANEL' }
{ type: 'BLUR_RIGHT_PANEL' }
```

`RIGHT_PANEL_NAV` is **not** in the reducer — navigation index lives in `RightPanel` local state.

### `RightPanel` local state

```ts
const [focusIdx, setFocusIdx] = useState(0);
```

Rebuilt whenever `fact`, `commitDetail`, or mode changes. Reset to `0` on content change.

---

## Files to change

| File | Change |
| --- | --- |
| `web/src/state.ts` | Add `rightPanelFocused: boolean` to `AppState`; add `FOCUS_RIGHT_PANEL` and `BLUR_RIGHT_PANEL` actions |
| `web/src/RightPanel.tsx` | Add `FactSwitcher` component; add focus ring logic; handle `→`/`←`/`j`/`k`/`Enter` when `rightPanelFocused` |
| `web/src/LeftPanel.tsx` | Guard `j`/`k`/`↑`/`↓`/`Enter` when `state.rightPanelFocused`; dispatch `FOCUS_RIGHT_PANEL` on `→` when selected item is a fact |
| `web/src/HistoryTimeline.tsx` | Guard `j`/`k`/`↑`/`↓`/`Enter` when `state.rightPanelFocused`; fix the `useEffect` keydown listener to include a proper dependency array (currently missing one), to avoid a stale-closure read of `rightPanelFocused` |

No backend changes required.

---

## Out of scope

- Summary view keyboard navigation (no interactive elements).
- History list view (`rightMode === 'history'`) — entries are not interactively navigable; left for a future iteration.
- Click-to-exact-element focus (clicks always start at index 0).
- Animated dropdown transitions.
- Non-URL refs are not focusable (they have no action).
