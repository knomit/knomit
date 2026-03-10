# Navigable Changed Files in History View

**Goal:** Make the "Changed files" list in the historical view interactive — selectable with arrow keys, navigable with Enter, pushing onto the navStack so Backspace returns to the directory history.

## Selectable Items

Add `"changed-file"` to `RightSelectableItem`:

```ts
{ type: "changed-file", label: string, path: string, changeStatus: "added" | "modified" | "deleted" }
```

Built from `historical.changedFiles` in render order: added, modified, deleted. Cursor highlight matches ref items (yellow, `glyph.cursor`).

## Navigation Behavior

Enter on a changed file dispatches `FOLLOW_REF`:
- **Added/Modified**: `path` = full file path, `commit` = current history entry's commit
- **Deleted**: `path` = full file path, `commit` = current history entry's commit + `^` (parent)

Git resolves `commit^` natively — no extra parent resolution needed.

`refCommitTarget` syncs the history timeline to the target commit. Backspace pops navStack back to the directory history view.

## Scope

- Extend `RightSelectableItem` with `changeStatus` field
- Build selectable items from `changedFiles` in `HistoricalView`
- Handle Enter on `changed-file` items in App.tsx keyboard handler
- Right panel focus (→) enabled during history mode when items exist
- Tests for the new item type and navigation flow
