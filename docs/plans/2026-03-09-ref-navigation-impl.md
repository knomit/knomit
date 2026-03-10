# TUI Ref Navigation Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Enable navigating knomit: refs from the fact view to open referenced facts at their exact commit, with history timeline synced.

**Architecture:** Add `parseKnomitRef()` parser, extend the reducer with `navStack`/`FOLLOW_REF`/`NAV_BACK` actions, make refs selectable in FactView, and wire up keyboard + effect handlers in App.tsx.

**Tech Stack:** Bun, TypeScript, React (Ink), existing reducer pattern in `src/tui/state.ts`

---

### Task 1: Ref Parser

**Files:**
- Create: `src/tui/refs.ts`
- Create: `src/tui/refs.test.ts`

**Step 1: Write the failing tests**

```ts
// src/tui/refs.test.ts
import { describe, it, expect } from "bun:test";
import { parseKnomitRef } from "./refs";

describe("parseKnomitRef", () => {
  it("parses local ref", () => {
    expect(parseKnomitRef("knomit:blob/abc1234/worlds/people/alice/likes-rock.md"))
      .toEqual({ path: "worlds/people/alice/likes-rock.md", commit: "abc1234" });
  });

  it("parses external ref", () => {
    expect(parseKnomitRef("knomit://github.com/org/repo/blob/def5678/src/file.ts"))
      .toEqual({ path: "src/file.ts", commit: "def5678", external: true });
  });

  it("returns null for plain URLs", () => {
    expect(parseKnomitRef("https://example.com")).toBeNull();
  });

  it("returns null for non-URI strings", () => {
    expect(parseKnomitRef("issue #42")).toBeNull();
  });

  it("handles paths with multiple slashes", () => {
    expect(parseKnomitRef("knomit:blob/aaa1111/worlds/a/b/c/d.md"))
      .toEqual({ path: "worlds/a/b/c/d.md", commit: "aaa1111" });
  });
});
```

**Step 2: Run test to verify it fails**

Run: `bun test ./tui/refs.test.ts`
Expected: FAIL — module not found

**Step 3: Write minimal implementation**

```ts
// src/tui/refs.ts
export interface ParsedRef {
  path: string;
  commit: string;
  external?: true;
}

export function parseKnomitRef(ref: string): ParsedRef | null {
  // Local: knomit:blob/<commit>/<path>
  const localMatch = ref.match(/^knomit:blob\/([^/]+)\/(.+)$/);
  if (localMatch) {
    return { path: localMatch[2], commit: localMatch[1] };
  }

  // External: knomit://<host>/<owner>/<repo>/blob/<commit>/<path>
  const extMatch = ref.match(/^knomit:\/\/[^/]+\/[^/]+\/[^/]+\/blob\/([^/]+)\/(.+)$/);
  if (extMatch) {
    return { path: extMatch[2], commit: extMatch[1], external: true };
  }

  return null;
}
```

**Step 4: Run test to verify it passes**

Run: `bun test ./tui/refs.test.ts`
Expected: 5 pass, 0 fail

**Step 5: Commit**

```
git add src/tui/refs.ts src/tui/refs.test.ts
git commit -m "feat(tui): add parseKnomitRef parser"
```

---

### Task 2: NavEntry Type and navStack in State

**Files:**
- Modify: `src/tui/state.ts`

**Step 1: Add NavEntry interface and navStack to AppState**

After the `SavedNavState` interface (~line 16), add:

```ts
export interface NavEntry {
  currentPath: string;
  currentFact: string | null;
  selectedIndex: number;
  breadcrumbSelected: boolean;
  statsPath: string;
  rightPanelMode: "summary" | "fact" | "history";
  historyMode: boolean;
  historyTarget: string;
  historySelectedIndex: number;
  focusZone: "left" | "right";
  rightSelectedIndex: number;
  // Preserve search context if active when following ref
  searchActive: boolean;
  searchResults: SearchResultItem[];
  searchType: "text" | "domain";
  savedNavState: SavedNavState | null;
}
```

Add `navStack: NavEntry[]` to `AppState` interface and `initialState`:

```ts
// In AppState interface, add:
navStack: NavEntry[];

// In initialState, add:
navStack: [],
```

**Step 2: Add new action types**

Extend the `Action` union:

```ts
| { type: "FOLLOW_REF"; path: string; commit: string }
| { type: "NAV_BACK" }
```

**Step 3: Implement reducer cases**

Add to the `reducer` switch:

```ts
case "FOLLOW_REF": {
  const entry: NavEntry = {
    currentPath: state.currentPath,
    currentFact: state.currentFact,
    selectedIndex: state.selectedIndex,
    breadcrumbSelected: state.breadcrumbSelected,
    statsPath: state.statsPath,
    rightPanelMode: state.rightPanelMode,
    historyMode: state.historyMode,
    historyTarget: state.historyTarget,
    historySelectedIndex: state.historySelectedIndex,
    focusZone: state.focusZone as "left" | "right",
    rightSelectedIndex: state.rightSelectedIndex,
    searchActive: state.searchActive,
    searchResults: state.searchResults,
    searchType: state.searchType,
    savedNavState: state.savedNavState,
  };
  return {
    ...state,
    navStack: [...state.navStack, entry],
    historyMode: true,
    historyTarget: action.path,
    historySelectedIndex: 0,
    historyEntries: [],
    currentFact: action.path,
    rightPanelMode: "history",
    focusZone: "left",
    rightSelectedIndex: 0,
  };
}

case "NAV_BACK": {
  if (state.navStack.length === 0) return state;
  const stack = [...state.navStack];
  const prev = stack.pop()!;
  return {
    ...state,
    navStack: stack,
    currentPath: prev.currentPath,
    currentFact: prev.currentFact,
    selectedIndex: prev.selectedIndex,
    breadcrumbSelected: prev.breadcrumbSelected,
    statsPath: prev.statsPath,
    rightPanelMode: prev.rightPanelMode,
    historyMode: prev.historyMode,
    historyTarget: prev.historyTarget,
    historySelectedIndex: prev.historySelectedIndex,
    focusZone: prev.focusZone,
    rightSelectedIndex: prev.rightSelectedIndex,
    searchActive: prev.searchActive,
    searchResults: prev.searchResults,
    searchType: prev.searchType,
    savedNavState: prev.savedNavState,
    historyEntries: [],
  };
}
```

**Step 4: Run existing tests to verify nothing broke**

Run: `bun test ./tui/state.test.ts`
Expected: all existing tests pass

**Step 5: Commit**

```
git add src/tui/state.ts
git commit -m "feat(tui): add navStack, FOLLOW_REF, NAV_BACK to state"
```

---

### Task 3: Reducer Tests for Nav Stack

**Files:**
- Modify: `src/tui/state.test.ts`

**Step 1: Write tests for FOLLOW_REF and NAV_BACK**

Add a new describe block:

```ts
describe("ref navigation", () => {
  it("FOLLOW_REF pushes state and enters history mode", () => {
    let s = { ...initialState, currentFact: "worlds/distilled/overview.md", rightPanelMode: "fact" as const };
    s = reducer(s, { type: "FOLLOW_REF", path: "worlds/people/alice/likes-rock.md", commit: "abc1234" });

    expect(s.navStack).toHaveLength(1);
    expect(s.navStack[0].currentFact).toBe("worlds/distilled/overview.md");
    expect(s.navStack[0].rightPanelMode).toBe("fact");
    expect(s.historyMode).toBe(true);
    expect(s.historyTarget).toBe("worlds/people/alice/likes-rock.md");
    expect(s.currentFact).toBe("worlds/people/alice/likes-rock.md");
    expect(s.rightPanelMode).toBe("history");
  });

  it("NAV_BACK restores previous state", () => {
    let s = { ...initialState, currentFact: "worlds/distilled/overview.md", rightPanelMode: "fact" as const };
    s = reducer(s, { type: "FOLLOW_REF", path: "worlds/people/alice/likes-rock.md", commit: "abc1234" });
    s = reducer(s, { type: "NAV_BACK" });

    expect(s.navStack).toHaveLength(0);
    expect(s.currentFact).toBe("worlds/distilled/overview.md");
    expect(s.rightPanelMode).toBe("fact");
    expect(s.historyMode).toBe(false);
  });

  it("NAV_BACK on empty stack is no-op", () => {
    const s = reducer(initialState, { type: "NAV_BACK" });
    expect(s).toBe(initialState);
  });

  it("deep stack: follow 3 refs, back 3 times", () => {
    let s = { ...initialState, currentFact: "worlds/a.md", rightPanelMode: "fact" as const };
    s = reducer(s, { type: "FOLLOW_REF", path: "worlds/b.md", commit: "aaa" });
    s = reducer(s, { type: "FOLLOW_REF", path: "worlds/c.md", commit: "bbb" });
    s = reducer(s, { type: "FOLLOW_REF", path: "worlds/d.md", commit: "ccc" });

    expect(s.navStack).toHaveLength(3);
    expect(s.currentFact).toBe("worlds/d.md");

    s = reducer(s, { type: "NAV_BACK" });
    expect(s.currentFact).toBe("worlds/c.md");
    expect(s.navStack).toHaveLength(2);

    s = reducer(s, { type: "NAV_BACK" });
    expect(s.currentFact).toBe("worlds/b.md");
    expect(s.navStack).toHaveLength(1);

    s = reducer(s, { type: "NAV_BACK" });
    expect(s.currentFact).toBe("worlds/a.md");
    expect(s.navStack).toHaveLength(0);
    expect(s.historyMode).toBe(false);
  });

  it("FOLLOW_REF during search preserves search state", () => {
    let s = {
      ...initialState,
      searchActive: true,
      searchResults: [{ file: "worlds/x.md", title: "X", body: "", score: 1 }],
      currentFact: "worlds/x.md",
      rightPanelMode: "fact" as const,
    };
    s = reducer(s, { type: "FOLLOW_REF", path: "worlds/y.md", commit: "aaa" });

    expect(s.navStack[0].searchActive).toBe(true);
    expect(s.navStack[0].searchResults).toHaveLength(1);

    s = reducer(s, { type: "NAV_BACK" });
    expect(s.searchActive).toBe(true);
    expect(s.searchResults).toHaveLength(1);
  });
});
```

**Step 2: Run tests**

Run: `bun test ./tui/state.test.ts`
Expected: all pass (new + existing)

**Step 3: Commit**

```
git add src/tui/state.test.ts
git commit -m "test(tui): reducer tests for FOLLOW_REF and NAV_BACK"
```

---

### Task 4: Selectable Refs in RightPanel

**Files:**
- Modify: `src/tui/RightPanel.tsx`

**Step 1: Import ParsedRef and extend RightSelectableItem**

At top of file, add import:

```ts
import { parseKnomitRef, type ParsedRef } from "./refs.js";
```

Update `RightSelectableItem` to include refs:

```ts
export interface RightSelectableItem {
  type: "domain" | "entity" | "fact" | "ref";
  label: string;
  path?: string;
  ref?: ParsedRef;
}
```

**Step 2: Update buildFactSelectableItems to include refs**

```ts
export function buildFactSelectableItems(
  frontmatter: Frontmatter | undefined,
): RightSelectableItem[] {
  const items: RightSelectableItem[] = [];
  if (!frontmatter) return items;
  for (const d of frontmatter.domain) {
    items.push({ type: "domain", label: d });
  }
  for (const e of frontmatter.entities) {
    items.push({ type: "entity", label: e });
  }
  for (const r of frontmatter.refs) {
    const parsed = parseKnomitRef(r);
    items.push({ type: "ref", label: r, ref: parsed ?? undefined });
  }
  return items;
}
```

**Step 3: Render refs in FactView**

Replace the static refs rendering block (lines 505-516) with selectable refs. The refs section in `FactView` currently renders non-interactively. Replace it with:

```tsx
{frontmatter && frontmatter.refs.length > 0 && (
  <Box flexDirection="column" marginTop={1}>
    <Text color={theme.dim}>{glyph.dashDivider.repeat(30)}</Text>
    <Text color={theme.primary} bold>{glyph.bullet} References</Text>
    {frontmatter.refs.map((r, ri) => {
      const parsed = parseKnomitRef(r);
      const itemIdx = domainCount + (frontmatter.entities?.length ?? 0) + ri;
      const isActive = focused && selectedIndex === itemIdx;
      const isExternal = parsed?.external;
      return (
        <Box key={r}>
          <Text color={isActive ? theme.yellow : theme.dim}>
            {isActive ? `  ${glyph.cursor} ` : "    "}
          </Text>
          <Text color={isActive ? theme.yellow : isExternal ? theme.dim : theme.secondary}>
            {glyph.arrow} {r}
          </Text>
          {isExternal && <Text color={theme.dim}> (external)</Text>}
        </Box>
      );
    })}
  </Box>
)}
```

**Step 4: Run tests**

Run: `bun test ./tui/`
Expected: all pass

**Step 5: Commit**

```
git add src/tui/RightPanel.tsx
git commit -m "feat(tui): make refs selectable in fact view"
```

---

### Task 5: Keyboard Handling in App.tsx

**Files:**
- Modify: `src/tui/App.tsx`

**Step 1: Import parseKnomitRef**

```ts
import { parseKnomitRef } from "./refs.js";
```

**Step 2: Handle Enter on ref items in right panel**

In the right panel `key.return` handler (~line 206), add a case for refs after the existing `fact` case:

```ts
} else if (item.type === "ref" && item.ref && !item.ref.external) {
  dispatch({
    type: "FOLLOW_REF",
    path: item.ref.path,
    commit: item.ref.commit,
  });
}
```

**Step 3: Handle Backspace/Left with navStack**

In the left panel Backspace/Left handler (~line 244), change the logic to check navStack first:

```ts
else if (key.leftArrow || key.backspace || key.delete) {
  if (state.navStack.length > 0) {
    dispatch({ type: "NAV_BACK" });
  } else if (state.historyMode) {
    dispatch({ type: "TOGGLE_HISTORY", target: "" });
  } else if (state.searchActive) {
    dispatch({ type: "CLEAR_SEARCH" });
  } else {
    dispatch({ type: "GO_UP" });
  }
}
```

**Step 4: Sync history to ref commit**

The existing `useEffect` for `SET_HISTORY_ENTRIES` (line 128-138) already loads `repo.log(state.historyTarget)` and sets `historySelectedIndex: 0`. We need to sync to the ref's commit instead.

Add a new state field is not needed — the FOLLOW_REF action stores the commit in a way we can match. Add a ref to track the target commit:

In App.tsx, add a ref before the effects:

```ts
const refCommitTarget = React.useRef<string | null>(null);
```

In the keyboard handler where FOLLOW_REF is dispatched, set it:

```ts
} else if (item.type === "ref" && item.ref && !item.ref.external) {
  refCommitTarget.current = item.ref.commit;
  dispatch({
    type: "FOLLOW_REF",
    path: item.ref.path,
    commit: item.ref.commit,
  });
}
```

Modify the history entries effect (~line 128) to sync to the target commit after loading entries:

```ts
useEffect(() => {
  if (!state.historyMode || !state.historyTarget) return;
  (async () => {
    try {
      const entries = await repo.log(state.historyTarget);
      dispatch({ type: "SET_HISTORY_ENTRIES", entries });
      // Sync to ref commit if navigating via ref
      if (refCommitTarget.current) {
        const targetCommit = refCommitTarget.current;
        refCommitTarget.current = null;
        const idx = entries.findIndex((e) => e.commit.startsWith(targetCommit));
        if (idx >= 0) {
          // Dispatch multiple NAVIGATE_DOWN to reach the target index
          for (let i = 0; i < idx; i++) {
            dispatch({ type: "NAVIGATE_DOWN" });
          }
        }
      }
    } catch {
      dispatch({ type: "SET_HISTORY_ENTRIES", entries: [] });
    }
  })();
}, [state.historyMode, state.historyTarget]);
```

**Step 5: Run tests**

Run: `bun test ./tui/`
Expected: all pass

**Step 6: Commit**

```
git add src/tui/App.tsx
git commit -m "feat(tui): wire ref navigation keyboard handling and history sync"
```

---

### Task 6: Manual Smoke Test

**Step 1: Seed test data with refs**

Run: `bun scripts/seed.ts all /tmp/knomit-ref-test`

Then run synthesis to generate distilled facts with refs:
Run: `bun src/cli/synthesize.ts /tmp/knomit-ref-test` (or however synthesis is invoked)

**Step 2: Launch TUI and test navigation**

Run: `bun src/cli/tui.ts /tmp/knomit-ref-test`

Test cases:
1. Navigate to a distilled fact → verify refs appear as selectable items in right panel
2. Press → to focus right panel, ↓ to a ref, Enter to follow → verify history view opens at ref's commit
3. Press Backspace → verify you return to the distilled fact
4. Follow ref A → follow ref B → Backspace → verify you're at ref A → Backspace → verify you're at the original fact
5. Verify external refs render dimmed and Enter does nothing on them

**Step 3: Commit any fixes found during smoke test**

---

### Task 7: Final Commit

**Step 1: Run all TUI tests**

Run: `bun test ./tui/`
Expected: all pass

**Step 2: Final commit if any cleanup was needed**

```
git add -A
git commit -m "feat(tui): ref navigation complete"
```
