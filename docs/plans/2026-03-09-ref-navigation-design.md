# TUI Ref Navigation Design

Navigate `knomit:` refs from the fact view to open referenced facts at their exact commit, with history timeline synced to that point.

## Decisions

- **Always historical**: refs resolve to the exact commit in the URI, never HEAD
- **Refs as selectable items**: added to the right panel's selectable list after domains/entities, navigated with ↑/↓ and Enter
- **Deep back stack**: browser-style navigation stack in AppState (navStack: NavEntry[])
- **Back via Backspace/Left**: consistent with existing "go up" behavior
- **SQLite fast path**: if search index has the fact cached at the ref's commit, read from SQLite; otherwise fall back to `repo.readFileAtCommit()`
- **External refs non-navigable**: `knomit://` refs (cross-repo) render dimmed, not selectable

## Ref Parsing

`parseKnomitRef(ref: string): ParsedRef | null`

```
"knomit:blob/abc1234/know/people/alice/likes-rock.md"
  → { path: "know/people/alice/likes-rock.md", commit: "abc1234" }

"knomit://github.com/org/repo/blob/abc1234/src/file.ts"
  → { path: "src/file.ts", commit: "abc1234", external: true }

"https://example.com"  → null
"issue #42"            → null
```

## State Changes

### New types

```ts
interface NavEntry {
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
}
```

### AppState additions

- `navStack: NavEntry[]` (default `[]`)

### New actions

- `FOLLOW_REF { path: string; commit: string }` — push current view state onto navStack, switch to history mode viewing path at commit
- `NAV_BACK` — pop navStack, restore previous view state. No-op when stack is empty.

### Interaction with existing actions

- Backspace/Left when `navStack.length > 0` dispatches `NAV_BACK` instead of `TOGGLE_HISTORY` or `GO_UP`
- `savedNavState` (search) is orthogonal — search state is captured in NavEntry when following a ref during search

## Selectable Refs in FactView

`buildFactSelectableItems()` extended to include refs:

```
RightSelectableItem
  | { type: "domain", label: string }
  | { type: "entity", label: string }
  | { type: "ref", label: string, ref: ParsedRef }
```

External refs included in the list but rendered dimmed and not actionable on Enter.

## Ref Resolution Flow

When user selects a ref and presses Enter:

1. Parse ref with `parseKnomitRef()`
2. If external → no-op
3. Push current state onto navStack
4. Dispatch `FOLLOW_REF { path, commit }`
5. App.tsx effect: load `repo.log(path)` for history timeline
6. Find the entry matching `commit` in the log (or nearest ancestor)
7. Set `historySelectedIndex` to that position
8. Load fact content: check SQLite for cached version at commit, fall back to `repo.readFileAtCommit(path, commit)`
9. Left panel shows history timeline synced to the commit; right panel shows fact content at that commit

## Testing

### `parseKnomitRef` unit tests
- Local ref → `{ path, commit }`
- External ref → `{ path, commit, external: true }`
- Non-knomit strings → null
- Paths with multiple slashes

### Reducer tests in `state.test.ts`
- FOLLOW_REF pushes state, enters history mode
- NAV_BACK pops and restores
- NAV_BACK on empty stack is no-op
- Deep stack: follow 3 refs, back 3 times, verify each restore
- FOLLOW_REF during active search preserves search state in NavEntry

## Files to modify

- `src/tui/state.ts` — NavEntry type, navStack field, FOLLOW_REF/NAV_BACK actions
- `src/tui/state.test.ts` — reducer tests
- `src/tui/RightPanel.tsx` — refs as selectable items, dimmed external refs
- `src/tui/App.tsx` — keyboard handler for Enter on ref items, Backspace/Left with navStack, ref resolution effect
- New: `src/tui/refs.ts` — `parseKnomitRef()` function
- New: `src/tui/refs.test.ts` — parser tests
