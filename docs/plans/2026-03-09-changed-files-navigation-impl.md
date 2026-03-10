# Changed Files Navigation Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make the "Changed files" list in the historical directory view interactive — selectable with arrow keys, Enter follows the file into its own history at the relevant commit, with navStack for back navigation.

**Architecture:** Extend `RightSelectableItem` with a `"changed-file"` type carrying `changeStatus`. Build selectable items from `HistoricalData.changedFiles` in `RightPanel`. Pass `focused`/`selectedIndex` props to `HistoricalView`. Handle Enter on changed-file items in App.tsx by dispatching `FOLLOW_REF`. Remove the `!state.historyMode` guard on right-arrow focus.

**Tech Stack:** Bun, TypeScript, React (Ink), existing reducer pattern in `src/tui/state.ts`

---

### Task 1: Extend RightSelectableItem and Build Changed-File Items

**Files:**
- Modify: `src/tui/RightPanel.tsx`

**Step 1: Extend the interface**

In `RightSelectableItem` (line 12-17), add `"changed-file"` to the type union and add `changeStatus`:

```ts
export interface RightSelectableItem {
  type: "domain" | "entity" | "fact" | "ref" | "changed-file";
  label: string;
  path?: string;
  ref?: ParsedRef;
  changeStatus?: "added" | "modified" | "deleted";
}
```

**Step 2: Add buildChangedFileItems function**

After `buildFactSelectableItems` (line 59), add:

```ts
export function buildChangedFileItems(
  changedFiles: { added: string[]; modified: string[]; deleted: string[] } | undefined,
  historyTarget: string,
): RightSelectableItem[] {
  if (!changedFiles) return [];
  const items: RightSelectableItem[] = [];
  for (const f of changedFiles.added) {
    items.push({ type: "changed-file", label: f, path: `${historyTarget}/${f}`, changeStatus: "added" });
  }
  for (const f of changedFiles.modified) {
    items.push({ type: "changed-file", label: f, path: `${historyTarget}/${f}`, changeStatus: "modified" });
  }
  for (const f of changedFiles.deleted) {
    items.push({ type: "changed-file", label: f, path: `${historyTarget}/${f}`, changeStatus: "deleted" });
  }
  return items;
}
```

Note: `historyTarget` is the directory path (e.g. `know/people`) — file names from `changedFiles` are relative to it, so the full path is `historyTarget/filename`.

**Step 3: Wire changed-file items into RightPanel selectability**

In `RightPanel` function (line 91-98), change the selectableItems logic to include changed-file items when historical data with changedFiles is present:

```ts
let selectableItems: RightSelectableItem[] = [];
if (historical?.changedFiles) {
  selectableItems = buildChangedFileItems(historical.changedFiles, historical.entry ? /* need historyTarget */ "");
} else if (!historical) {
  if (mode === "summary") {
    selectableItems = buildSelectableItems(stats, summaryChildren);
  } else if (mode === "fact") {
    selectableItems = buildFactSelectableItems(factFrontmatter);
  }
}
```

Problem: `HistoricalView` doesn't know the `historyTarget` (the directory path). We need to pass it through. Add `historyTarget` to `RightPanelProps`:

```ts
interface RightPanelProps {
  // ... existing props
  historyTarget?: string;
}
```

Then use it:

```ts
if (historical?.changedFiles) {
  selectableItems = buildChangedFileItems(historical.changedFiles, historyTarget ?? "");
}
```

**Step 4: Run tests**

Run: `bun test src/tui/`
Expected: all pass (no changed-file tests yet, but existing tests must not break)

**Step 5: Commit**

```
git add src/tui/RightPanel.tsx
git commit -m "feat(tui): add changed-file selectable items in history view"
```

---

### Task 2: Pass Props and Render Selectable Changed Files

**Files:**
- Modify: `src/tui/RightPanel.tsx`
- Modify: `src/tui/App.tsx`

**Step 1: Pass historyTarget from App.tsx to RightPanel**

In App.tsx where `<RightPanel>` is rendered (line 380-393), add the `historyTarget` prop:

```tsx
<RightPanel
  // ... existing props
  historyTarget={state.historyTarget}
/>
```

**Step 2: Pass focused and selectedIndex to HistoricalView**

In `RightPanel`, pass `focused` and `selectedIndex` to `HistoricalView` (line 110):

```tsx
<HistoricalView
  data={historical}
  theme={theme}
  maxHeight={contentHeight}
  focused={focused}
  selectedIndex={selectedIndex}
/>
```

Update the `HistoricalView` function signature (line 128):

```ts
function HistoricalView({ data, theme, maxHeight, focused, selectedIndex }: {
  data: HistoricalData; theme: Theme; maxHeight: number;
  focused?: boolean; selectedIndex?: number;
}) {
```

**Step 3: Replace static changed-files rendering with selectable items**

Replace the changed files rendering block (lines 250-272) with:

```tsx
{changedFiles && (changedFiles.added.length > 0 || changedFiles.modified.length > 0 || changedFiles.deleted.length > 0) && (
  <Box flexDirection="column" marginTop={1}>
    <Text color={theme.dim}>{glyph.dashDivider} Changed files:</Text>
    {[
      ...changedFiles.added.map((f, i) => ({ f, i, status: "added" as const, color: theme.green, prefix: "+" })),
      ...changedFiles.modified.map((f, i) => ({ f, i: changedFiles.added.length + i, status: "modified" as const, color: theme.yellow, prefix: "~" })),
      ...changedFiles.deleted.map((f, i) => ({ f, i: changedFiles.added.length + changedFiles.modified.length + i, status: "deleted" as const, color: theme.red, prefix: "-" })),
    ].map(({ f, i: itemIdx, color, prefix }) => {
      const isActive = focused && selectedIndex === itemIdx;
      return (
        <Box key={f}>
          <Text color={isActive ? theme.yellow : theme.dim}>
            {isActive ? `${glyph.cursor} ` : "  "}
          </Text>
          <Text color={isActive ? theme.yellow : color} bold={!isActive}>
            {prefix}{" "}
          </Text>
          <Text color={isActive ? theme.yellow : color}>{f}</Text>
        </Box>
      );
    })}
  </Box>
)}
```

**Step 4: Run tests**

Run: `bun test src/tui/`
Expected: all pass

**Step 5: Commit**

```
git add src/tui/RightPanel.tsx src/tui/App.tsx
git commit -m "feat(tui): render selectable changed files in history view"
```

---

### Task 3: Enable Right-Panel Focus in History Mode

**Files:**
- Modify: `src/tui/App.tsx`

**Step 1: Remove the history mode guard on right-arrow**

In App.tsx (line 273-276), change the right-arrow handler to allow focus when items exist, even in history mode:

```ts
else if (key.rightArrow) {
  if (state.rightItemCount > 0) {
    dispatch({ type: "SET_FOCUS", zone: "right" });
  }
}
```

This removes the `!state.historyMode &&` guard. The `rightItemCount` check is sufficient — if no changed files exist, there are no items to focus.

**Step 2: Run tests**

Run: `bun test src/tui/`
Expected: all pass

**Step 3: Commit**

```
git add src/tui/App.tsx
git commit -m "feat(tui): allow right-panel focus during history mode"
```

---

### Task 4: Handle Enter on Changed-File Items

**Files:**
- Modify: `src/tui/App.tsx`

**Step 1: Add changed-file case in the right-panel Enter handler**

In App.tsx, in the right-panel `key.return` handler (after the ref case, ~line 237), add:

```ts
} else if (item.type === "changed-file" && item.path) {
  const commit = selectedHistoryEntry?.commit;
  if (commit) {
    const targetCommit = item.changeStatus === "deleted" ? `${commit}^` : commit;
    refCommitTarget.current = targetCommit;
    dispatch({
      type: "FOLLOW_REF",
      path: item.path,
      commit: targetCommit,
    });
  }
}
```

Note: `selectedHistoryEntry` is already computed at line 150 of App.tsx — it's the current history entry being viewed. For deleted files, we use `commit^` so git resolves the parent where the file last existed.

**Step 2: Run tests**

Run: `bun test src/tui/`
Expected: all pass

**Step 3: Commit**

```
git add src/tui/App.tsx
git commit -m "feat(tui): navigate from changed files to file history"
```

---

### Task 5: Reducer Tests for Changed-File Navigation

**Files:**
- Modify: `src/tui/state.test.ts`

**Step 1: Add tests for FOLLOW_REF from history context**

These tests verify that navigating from a directory history view pushes the right state and that NAV_BACK restores it. Add to the existing `describe("ref navigation", ...)` block:

```ts
it("FOLLOW_REF from history mode preserves history state", () => {
  let s = {
    ...initialState,
    historyMode: true,
    historyTarget: "know/people",
    historySelectedIndex: 2,
    rightPanelMode: "history" as const,
    currentFact: null,
  };
  s = reducer(s, { type: "FOLLOW_REF", path: "know/people/alice/likes-rock.md", commit: "abc1234" });

  expect(s.navStack).toHaveLength(1);
  expect(s.navStack[0].historyMode).toBe(true);
  expect(s.navStack[0].historyTarget).toBe("know/people");
  expect(s.navStack[0].historySelectedIndex).toBe(2);
  expect(s.navStack[0].rightPanelMode).toBe("history");
  expect(s.historyTarget).toBe("know/people/alice/likes-rock.md");
  expect(s.currentFact).toBe("know/people/alice/likes-rock.md");
});

it("NAV_BACK to history mode restores directory history", () => {
  let s = {
    ...initialState,
    historyMode: true,
    historyTarget: "know/people",
    historySelectedIndex: 2,
    rightPanelMode: "history" as const,
  };
  s = reducer(s, { type: "FOLLOW_REF", path: "know/people/alice/likes-rock.md", commit: "abc1234" });
  s = reducer(s, { type: "NAV_BACK" });

  expect(s.navStack).toHaveLength(0);
  expect(s.historyMode).toBe(true);
  expect(s.historyTarget).toBe("know/people");
  expect(s.historySelectedIndex).toBe(2);
  expect(s.rightPanelMode).toBe("history");
});
```

**Step 2: Run tests**

Run: `bun test src/tui/state.test.ts`
Expected: all pass (existing + 2 new)

**Step 3: Commit**

```
git add src/tui/state.test.ts
git commit -m "test(tui): reducer tests for changed-file navigation from history"
```

---

### Task 6: Unit Tests for buildChangedFileItems

**Files:**
- Modify: `src/tui/RightPanel.test.ts` (create if needed — check if it exists, otherwise add to `src/tui/state.test.ts`)

**Step 1: Write tests for buildChangedFileItems**

```ts
import { describe, it, expect } from "bun:test";
import { buildChangedFileItems } from "./RightPanel";

describe("buildChangedFileItems", () => {
  it("builds items in order: added, modified, deleted", () => {
    const items = buildChangedFileItems(
      { added: ["a.md"], modified: ["b.md"], deleted: ["c.md"] },
      "know/people",
    );
    expect(items).toEqual([
      { type: "changed-file", label: "a.md", path: "know/people/a.md", changeStatus: "added" },
      { type: "changed-file", label: "b.md", path: "know/people/b.md", changeStatus: "modified" },
      { type: "changed-file", label: "c.md", path: "know/people/c.md", changeStatus: "deleted" },
    ]);
  });

  it("returns empty array for undefined", () => {
    expect(buildChangedFileItems(undefined, "worlds")).toEqual([]);
  });

  it("returns empty array when no changes", () => {
    expect(buildChangedFileItems({ added: [], modified: [], deleted: [] }, "worlds")).toEqual([]);
  });
});
```

**Step 2: Run tests**

Run: `bun test src/tui/`
Expected: all pass

**Step 3: Commit**

```
git add src/tui/RightPanel.test.ts
git commit -m "test(tui): unit tests for buildChangedFileItems"
```

---

### Task 7: Final Verification

**Step 1: Run all TUI tests**

Run: `bun test src/tui/`
Expected: all pass

**Step 2: Final commit if any cleanup was needed**

```
git add -A
git commit -m "feat(tui): changed-file navigation complete"
```
