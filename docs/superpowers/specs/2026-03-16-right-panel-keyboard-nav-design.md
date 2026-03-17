# Right-panel keyboard navigation & commit fact-switcher

**Date:** 2026-03-16
**Status:** Approved

---

## Overview

Two related improvements:

1. **Commit fact-switcher** — when a commit touches multiple facts, replace the flat clickable list in the right panel with a collapsed dropdown at the top. Keyboard (`j`/`k`) cycles through the facts immediately (live load). The rendered fact appears below.
2. **Right-panel element focus** — when the right panel has focus, `j`/`k` moves a focus ring through all interactive elements (fact-switcher items, entity pills, domain pills, ref links). `Enter` activates the focused element. `←` returns focus to the left panel.

The fact layout itself is **not changed**.

---

## Focus model

### Acquiring focus

- When the user presses `→` while the left panel has focus, the right panel acquires focus. The first focusable element in the current view is pre-selected.
- When the user clicks anywhere in the right panel, the right panel acquires focus at the clicked element (if focusable) or at the nearest focusable element.

### Releasing focus

- `←` anywhere in the right panel returns focus to the left panel.
- No other key releases right-panel focus.

### Navigation

- `j` / `↓` — move to next focusable element (wraps at bottom back to top).
- `k` / `↑` — move to previous focusable element (wraps at top back to bottom).
- `Enter` — activate focused element (search for entity/domain, open ref URL, load fact from switcher).

### Focusable elements (in visual order)

In **commit view** (multi-file):
1. Fact-switcher items (cycling also loads the fact immediately)
2. Entity pills in the rendered fact (if a fact is loaded)
3. Domain pills
4. Reference links

In **fact view** (browsing/history):
1. Entity pills
2. Domain pills
3. Reference links
4. "Find similar" button

In **summary view**: no focusable elements — `→` does nothing.

### Focus indicator

A coloured `outline` ring is added to the currently focused element. The outline colour matches the element's existing accent:

- Entity pills: `rgba(136,170,255,0.55)` (blue)
- Domain pills: `rgba(119,204,153,0.55)` (green)
- Ref links: `rgba(136,170,255,0.55)` (blue)
- Fact-switcher items: background highlight (`#2a2a3a`), same as the existing selected-item style

No size, spacing, or layout changes.

---

## Commit fact-switcher

### Trigger

Shown only when `commitDetail.files.length > 1`. When there is exactly one viewable file, the existing behaviour (auto-load, no switcher) is preserved.

### Collapsed state

A single bar rendered above the fact body:

```
[ A ]  agents/memory-patterns          2 / 4  ▾
```

- Action badge (`A` / `M` / `D`) with existing colour coding (green/blue/red).
- Fact path, `.md` stripped.
- Position counter `n / total` (counts all files, including deleted).
- `▾` / `▴` chevron indicating open/closed.

### Expanded state (dropdown)

Appears below the trigger bar, listing all files:

- Each item: action badge + path.
- Currently loaded fact is highlighted (`background: #2a2a3a`).
- Deleted facts are shown greyed out (`opacity: 0.4`) and are not clickable.
- Clicking an item loads it and closes the dropdown.

### Keyboard behaviour

- `j`/`k` while the right panel has focus cycles through the **viewable** facts (skipping deleted ones). Each step:
  1. Updates the collapsed bar (path, counter, badge).
  2. Immediately fetches and renders the fact below.
- The dropdown does not open on `j`/`k` — it stays collapsed during keyboard cycling.
- `Enter` on the switcher trigger toggles the dropdown open/closed.
- `Enter` on a dropdown item loads that fact and closes the dropdown.

---

## State changes

A new piece of state is needed in `AppState`:

```ts
rightPanelFocused: boolean   // whether the right panel has keyboard focus
rightPanelFocusIdx: number   // index into the flat focusable-element list (-1 = none)
```

New actions:

```ts
{ type: 'FOCUS_RIGHT_PANEL' }
{ type: 'BLUR_RIGHT_PANEL' }
{ type: 'RIGHT_PANEL_NAV'; delta: 1 | -1 }
```

The flat focusable-element list is computed inside `RightPanel` from the current content (facts, entities, domains, refs). The list is rebuilt whenever content changes.

---

## Files to change

| File | Change |
|---|---|
| `web/src/state.ts` | Add `rightPanelFocused`, `rightPanelFocusIdx` state + 3 new actions |
| `web/src/RightPanel.tsx` | Add `FactSwitcher` component; add focus ring logic to existing pills/refs; handle `→`/`←`/`j`/`k`/`Enter` when focused |
| `web/src/LeftPanel.tsx` | Dispatch `FOCUS_RIGHT_PANEL` on `→` keydown |
| `web/src/App.tsx` | Pass `dispatch` to left panel if not already wired for arrow key handling |

No backend changes required.

---

## Out of scope

- Summary view keyboard navigation (no interactive elements).
- History list view (the `rightMode === 'history'` panel) — its entries are not interactive in the same sense; left for a future iteration.
- Animated dropdown transitions.
