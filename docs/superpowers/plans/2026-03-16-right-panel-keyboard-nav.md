# Right-panel keyboard navigation & commit fact-switcher Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add keyboard focus transfer between left and right panels, a collapsed fact-switcher for multi-file commits, and a focus ring that cycles through interactive elements (domains, entities, refs) in the right panel.

**Architecture:** `rightPanelFocused` lives in the global reducer as the single source of truth for key ownership; `focusIdx` is local to `RightPanel` since it depends on async content. Left panel and HistoryTimeline guard their own keydown handlers against `rightPanelFocused`. `FactSwitcher` is a self-contained component in `RightPanel.tsx`.

**Tech Stack:** React 18, TypeScript, Vitest (no React Testing Library — reducer tests only; component behaviour verified manually).

---

## Chunk 1: State layer

### Task 1: Extend AppState and reducer

**Files:**
- Modify: `web/src/state.ts`
- Test: `web/src/state.test.ts`

- [ ] **Step 1: Write failing tests**

Add these tests to the bottom of `web/src/state.test.ts`:

```ts
describe('right panel focus', () => {
  it('init has rightPanelFocused false', () => {
    expect(init.rightPanelFocused).toBe(false);
  });

  it('FOCUS_RIGHT_PANEL sets rightPanelFocused to true', () => {
    const s = reducer(init, { type: 'FOCUS_RIGHT_PANEL' });
    expect(s.rightPanelFocused).toBe(true);
  });

  it('BLUR_RIGHT_PANEL sets rightPanelFocused to false', () => {
    const s = reducer({ ...init, rightPanelFocused: true }, { type: 'BLUR_RIGHT_PANEL' });
    expect(s.rightPanelFocused).toBe(false);
  });

  it('NAVIGATE clears rightPanelFocused', () => {
    const s = reducer({ ...init, rightPanelFocused: true }, { type: 'NAVIGATE', path: 'kb/other' });
    expect(s.rightPanelFocused).toBe(false);
  });

  it('SET_REPO clears rightPanelFocused', () => {
    const s = reducer({ ...init, rightPanelFocused: true }, { type: 'SET_REPO', repo: 'other' });
    expect(s.rightPanelFocused).toBe(false);
  });
});
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
cd web && npm test
```

Expected: 5 failures — `rightPanelFocused` does not exist yet.

- [ ] **Step 3: Add `rightPanelFocused` to AppState**

In `web/src/state.ts`:

**a) Add the field to `AppState`** (after `historyCommit: string | null`):
```ts
  rightPanelFocused: boolean;
```

**b) Add two new actions to the `Action` union** (after `SET_REMOTE_ERROR`):
```ts
  | { type: 'FOCUS_RIGHT_PANEL' }
  | { type: 'BLUR_RIGHT_PANEL' }
```

**c) Add the field to `init`** (after `historyCommit: null`):
```ts
  rightPanelFocused: false,
```

**d) Add reducer cases** (after `SET_REMOTE_ERROR`):
```ts
case 'FOCUS_RIGHT_PANEL': return { ...s, rightPanelFocused: true };
case 'BLUR_RIGHT_PANEL': return { ...s, rightPanelFocused: false };
```

**e) Clear `rightPanelFocused` on `NAVIGATE`** — add `rightPanelFocused: false` to the existing return:
```ts
case 'NAVIGATE': return { ...s, currentPath: a.path, selectedFact: null, previewPath: null, rightMode: 'summary', searchQuery: '', similarTo: null, navStack: pushNav(s), rightPanelFocused: false };
```

**f) Clear on `SET_REPO`** — same pattern, add `rightPanelFocused: false` to the existing return spread.

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd web && npm test
```

Expected: all tests pass.

- [ ] **Step 5: Commit**

```bash
git add web/src/state.ts web/src/state.test.ts
git commit -m "feat: add rightPanelFocused to state with FOCUS/BLUR actions"
```

---

## Chunk 2: Guard existing keydown handlers

### Task 2: LeftPanel — guard keys and wire `→` focus transfer

**Files:**
- Modify: `web/src/LeftPanel.tsx`

> **Note:** `LeftPanel`'s keydown `useEffect` has **no dependency array** (line ~143). This means it re-registers the handler on every render, so it never has a stale closure problem. Do **not** add a dependency array to this `useEffect` — doing so without a complete dep list would actually introduce stale closure bugs. The guard added below will work correctly as-is.

- [ ] **Step 1: Add `→` focus-transfer and guard**

In `web/src/LeftPanel.tsx`, find the keydown handler in the `useEffect` (around lines 119–143). Replace the browse-mode-only section (the lines after `if (state.leftMode === 'history') return;`) with:

```ts
if (state.leftMode === 'history') return;
if (state.rightPanelFocused) return; // right panel owns j/k/enter when focused
if (e.key === 'ArrowDown' || e.key === 'j') { e.preventDefault(); moveSelection(1); }
if (e.key === 'ArrowUp' || e.key === 'k') { e.preventDefault(); moveSelection(-1); }
if (e.key === 'ArrowLeft' && !isSearchMode) { e.preventDefault(); dispatch({ type: 'GO_UP' }); }
if (e.key === 'Enter') { e.preventDefault(); activateSelected(); }
if (e.key === 'ArrowRight') {
  e.preventDefault();
  // In search mode, all results are facts → always transfer focus to right panel.
  // In browse mode, transfer only when selected item is a fact (not a directory).
  const isDir = !isSearchMode && children[selectedIdx]?.is_dir;
  if (isDir) {
    activateSelected(); // navigate into directory as before
  } else {
    dispatch({ type: 'FOCUS_RIGHT_PANEL' });
  }
}
```

- [ ] **Step 2: Run tests**

```bash
cd web && npm test
```

Expected: all tests pass (TypeScript must also compile cleanly — `tsc --noEmit` if in doubt).

- [ ] **Step 3: Commit**

```bash
git add web/src/LeftPanel.tsx
git commit -m "feat: wire right-arrow focus transfer and guard j/k when right panel focused"
```

---

### Task 3: HistoryTimeline — guard keys and fix missing dependency array

**Files:**
- Modify: `web/src/HistoryTimeline.tsx`

- [ ] **Step 1: Fix dependency array and add guard**

In `web/src/HistoryTimeline.tsx`, find the keydown `useEffect` (lines ~90–103). It currently has **no dependency array** (same as LeftPanel), but unlike LeftPanel, this handler is simpler and safe to add deps to. Replace it with:

```ts
useEffect(() => {
  const handler = (e: KeyboardEvent) => {
    if (state.rightPanelFocused) return; // right panel owns these keys when focused
    if (entries.length === 0) return;
    if (e.key === 'ArrowDown' || e.key === 'j') { e.preventDefault(); navigate(1); }
    if (e.key === 'ArrowUp' || e.key === 'k') { e.preventDefault(); navigate(-1); }
    if (e.key === 'Enter') {
      e.preventDefault();
      const entry = entries[selectedIdx];
      if (entry) dispatch({ type: 'SELECT_COMMIT', commit: entry.commit });
    }
  };
  window.addEventListener('keydown', handler);
  return () => window.removeEventListener('keydown', handler);
}, [state.rightPanelFocused, entries, selectedIdx, navigate, dispatch]);
```

- [ ] **Step 2: Run tests**

```bash
cd web && npm test
```

Expected: all tests pass.

- [ ] **Step 3: Commit**

```bash
git add web/src/HistoryTimeline.tsx
git commit -m "fix: guard HistoryTimeline keys when right panel focused; fix missing useEffect deps"
```

---

## Chunk 3: FactSwitcher component and right-panel focus system

### Task 4: Add FactSwitcher to RightPanel

**Files:**
- Modify: `web/src/RightPanel.tsx`

- [ ] **Step 1: Add FactSwitcher component**

Add this new component **above** the `export function RightPanel` line. Present the final controlled-prop version directly:

```tsx
interface SwitcherProps {
  files: CommitFile[];
  selectedPath: string | null;
  onSelect: (path: string) => void;
  focusIdx: number;       // 0 = trigger is focused (when right panel focused); -1 = not focused
  dropdownFocusIdx: number; // index within dropdown when open; -1 = none
  open: boolean;
  onToggle: () => void;
}

function FactSwitcher({ files, selectedPath, onSelect, focusIdx, dropdownFocusIdx, open, onToggle }: SwitcherProps) {
  const viewable = files.filter(f => f.action !== 'deleted');
  const currentIdx = viewable.findIndex(f => f.path === selectedPath);
  const current = viewable[currentIdx] ?? viewable[0] ?? null;

  const actionStyle = (action: string): React.CSSProperties => ({
    fontSize: 9, padding: '1px 5px', borderRadius: 3, fontFamily: 'monospace', fontWeight: 600,
    color: action === 'added' ? '#7c9' : action === 'modified' ? '#8af' : '#f88',
    background: action === 'added' ? '#1a2e1a' : action === 'modified' ? '#1a1a2e' : '#2e1a1a',
    ...(action === 'deleted' ? { opacity: 0.5 } : {}),
  });

  const triggerFocused = focusIdx === 0;

  return (
    <div style={{ margin: '10px 16px 12px' }}>
      <div
        onClick={onToggle}
        style={{
          display: 'flex', alignItems: 'center', gap: 8, padding: '7px 10px',
          background: triggerFocused ? '#2a2a3a' : '#1a1a2a',
          border: '1px solid #2a2a3a',
          borderRadius: open ? '6px 6px 0 0' : 6,
          cursor: 'pointer', userSelect: 'none' as const,
        }}
      >
        {current && <span style={actionStyle(current.action)}>{current.action[0].toUpperCase()}</span>}
        <span style={{ flex: 1, fontSize: 12, color: '#ddd', fontFamily: 'monospace', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
          {current ? current.path.replace(/\.md$/, '') : '—'}
        </span>
        <span style={{ fontSize: 11, color: '#555', flexShrink: 0 }}>
          {currentIdx + 1} / {viewable.length}
        </span>
        <span style={{ fontSize: 11, color: '#555', flexShrink: 0 }}>{open ? '▴' : '▾'}</span>
      </div>

      {open && (
        <div style={{ background: '#1a1a2a', border: '1px solid #2a2a3a', borderTop: 'none', borderRadius: '0 0 6px 6px', overflow: 'hidden' }}>
          {files.map((f, i) => {
            const isDeleted = f.action === 'deleted';
            const isSelected = f.path === selectedPath;
            const isDdFocused = dropdownFocusIdx === i;
            return (
              <div
                key={f.path}
                onClick={() => { if (isDeleted) return; onSelect(f.path); onToggle(); }}
                style={{
                  display: 'flex', alignItems: 'center', gap: 8, padding: '7px 10px',
                  cursor: isDeleted ? 'default' : 'pointer',
                  opacity: isDeleted ? 0.4 : 1,
                  background: isDdFocused ? '#2a2a3a' : isSelected ? '#222233' : 'transparent',
                  outline: isDdFocused ? '1px solid rgba(136,170,255,0.3)' : 'none',
                  outlineOffset: -1,
                }}
              >
                <span style={actionStyle(f.action)}>{f.action[0].toUpperCase()}</span>
                <span style={{ fontSize: 12, color: '#ddd', fontFamily: 'monospace' }}>{f.path.replace(/\.md$/, '')}</span>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
```

- [ ] **Step 2: Add FocusTarget type and buildFocusTargets at module scope**

Add these **before** the `FactSwitcher` function (at module scope, not inside any component):

```ts
type FocusTarget =
  | { kind: 'switcher' }
  | { kind: 'domain'; value: string }
  | { kind: 'entity'; value: string }
  | { kind: 'ref'; value: string }
  | { kind: 'similar' };

// hasSwitcher: true when the commit view has multiple viewable files.
// hasSimilar: true when the "Find similar" button will actually be rendered
//   (only in fact view where dispatch is passed to renderFact, NOT in the time-travel multi-file path).
function buildFocusTargets(f: Fact | null, hasSwitcher: boolean, hasSimilar: boolean): FocusTarget[] {
  const targets: FocusTarget[] = [];
  if (hasSwitcher) targets.push({ kind: 'switcher' });
  if (!f) return targets;
  const domainNames: string[] = (f.domain ?? []).map((d: string | [string, number]) => typeof d === 'string' ? d : d[0]);
  const entityNames: string[] = (f.entities ?? []).map((e: string | [string, number]) => typeof e === 'string' ? e : e[0]);
  for (const d of domainNames) targets.push({ kind: 'domain', value: d });
  for (const e of entityNames) targets.push({ kind: 'entity', value: e });
  for (const r of (f.refs ?? [])) {
    if (r.startsWith('http://') || r.startsWith('https://')) targets.push({ kind: 'ref', value: r });
  }
  if (hasSimilar) targets.push({ kind: 'similar' });
  return targets;
}
```

- [ ] **Step 3: Wire FactSwitcher into the multi-file commit view**

In `RightPanel`, add new local state at the top of the function body (alongside existing `useState` calls):

```tsx
const [focusIdx, setFocusIdx] = useState(0);
const [switcherOpen, setSwitcherOpen] = useState(false);
const [dropdownFocusIdx, setDropdownFocusIdx] = useState(-1);
```

Also add `hasSwitcher` **before any early returns** in the function body. This must appear before the single-file early-return below, because that return references `!hasSwitcher`:

```tsx
const hasSwitcher = !!(state.historyCommit && commitDetail && commitDetail.files.filter(f => f.action !== 'deleted').length > 1);
```

In the existing `useEffect` that fetches commit detail, add at the start of the `.then` callback:

```tsx
setFocusIdx(0);
setSwitcherOpen(false);
setDropdownFocusIdx(-1);
```

Now replace the multi-file render block. **Important:** the existing single-file early-return at lines ~197–199 must remain untouched:

```tsx
// Time-travel: single file auto-loaded → show fact normally  ← KEEP THIS BLOCK UNCHANGED
if (state.historyCommit && fact && !hasSwitcher) {
  return renderFact(fact, search);
}
```

Then the multi-file block (replace the existing `if (state.historyCommit && commitDetail)` block):

```tsx
// Time-travel: multiple viewable files → show FactSwitcher + selected fact below
if (state.historyCommit && commitDetail) {
  const viewable = commitDetail.files.filter(f => f.action !== 'deleted');

  return (
    <div
      onClick={() => dispatch({ type: 'FOCUS_RIGHT_PANEL' })}
      style={{ display: 'flex', flexDirection: 'column', height: '100%', overflowY: 'auto', boxSizing: 'border-box' }}
    >
      {/* Commit header */}
      <div style={{ padding: '16px 20px 8px', borderBottom: '1px solid #222', display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap', flexShrink: 0 }}>
        <span style={{ color: '#7c9', fontFamily: 'monospace', fontSize: 12 }}>{commitDetail.commit.slice(0, 7)}</span>
        <span style={{ color: '#666', fontSize: 11 }}>{relativeTime(commitDetail.date)}</span>
        {commitDetail.tags.map(tag => {
          const tc = tagColor(tag);
          return <span key={tag} style={{ color: tc.color, background: tc.bg, padding: '1px 6px', borderRadius: 3, fontSize: 10, fontFamily: 'monospace' }}>{tag}</span>;
        })}
      </div>
      <div style={{ color: '#888', fontSize: 12, padding: '6px 20px 0', flexShrink: 0 }}>{commitDetail.message}</div>

      {viewable.length > 1 && (
        <FactSwitcher
          files={commitDetail.files}
          selectedPath={commitSelectedFile}
          onSelect={path => {
            setCommitSelectedFile(path);
            api.fact(state.repo, path, state.historyCommit!).then(setFact).catch(() => setFact(null));
            setFocusIdx(0);
          }}
          focusIdx={state.rightPanelFocused ? focusIdx : -1}
          dropdownFocusIdx={dropdownFocusIdx}
          open={switcherOpen}
          onToggle={() => setSwitcherOpen(o => !o)}
        />
      )}

      {fact && <div style={{ flex: 1 }}>{renderFact(fact, search)}</div>}
      {!fact && viewable.length > 0 && (
        <div style={{ padding: '16px 20px', color: '#666', fontSize: 13 }}>Select a fact above.</div>
      )}
    </div>
  );
}
```

> **Note on single-file path:** When `viewable.length === 1`, the single-file early-return at the top fires first (because `fact` is auto-loaded and set). The `hasSwitcher` variable below will be `false` for this case, keeping that path clean.

- [ ] **Step 4: Run tests**

```bash
cd web && npm test
```

Expected: all tests pass.

- [ ] **Step 5: Manual smoke test**

- Open a repo with commits that touch multiple facts.
- Enter history mode, click a multi-file commit.
- Verify the FactSwitcher bar appears (collapsed), showing "1 / N" and `▾`.
- Click the bar → dropdown opens with all files; deleted files are greyed out and unclickable.
- Click a viewable file → fact loads below; dropdown closes.
- Single-file commits: no switcher visible, fact loads directly as before.

- [ ] **Step 6: Commit**

```bash
git add web/src/RightPanel.tsx
git commit -m "feat: add FactSwitcher component for multi-file commits"
```

---

### Task 5: Right-panel focus system

**Files:**
- Modify: `web/src/RightPanel.tsx`

- [ ] **Step 1: Compute focusTargets**

`hasSwitcher` was already declared in Task 4 Step 3. Add `focusTargets` near it (after the `hasSwitcher` line):

```tsx
// hasSimilar is true only in regular fact view (not in time-travel multi-file path)
const hasSimilar = !!fact && !hasSwitcher;
const focusTargets = buildFocusTargets(fact, hasSwitcher, hasSimilar);
```

Add a reset `useEffect` so `focusIdx` resets to `0` when content changes:

```tsx
useEffect(() => {
  setFocusIdx(0);
  setSwitcherOpen(false);
  setDropdownFocusIdx(-1);
}, [fact, commitDetail, state.rightMode]);
```

- [ ] **Step 2: Add the keydown handler**

Add this `useEffect` to `RightPanel`. The dependency array includes `focusIdx` because the handler reads it directly — **do not omit `focusIdx` from deps**, otherwise the handler captures a stale value and navigation silently breaks.

```tsx
useEffect(() => {
  if (!state.rightPanelFocused) return;
  const handler = (e: KeyboardEvent) => {
    if (e.key === 'ArrowLeft') {
      e.preventDefault();
      dispatch({ type: 'BLUR_RIGHT_PANEL' });
      return;
    }

    const moveInDropdown = (delta: 1 | -1) => {
      const total = commitDetail!.files.length;
      setDropdownFocusIdx(i => (i + delta + total) % total);
    };

    const cycleFact = (delta: 1 | -1) => {
      const viewable = commitDetail!.files.filter(f => f.action !== 'deleted');
      const cur = viewable.findIndex(f => f.path === commitSelectedFile);
      const next = (cur + delta + viewable.length) % viewable.length;
      const nextFile = viewable[next];
      setCommitSelectedFile(nextFile.path);
      api.fact(state.repo, nextFile.path, state.historyCommit!).then(setFact).catch(() => setFact(null));
      setFocusIdx(0);
    };

    if (e.key === 'ArrowDown' || e.key === 'j') {
      e.preventDefault();
      if (hasSwitcher && switcherOpen) { moveInDropdown(1); return; }
      if (hasSwitcher && focusTargets[focusIdx]?.kind === 'switcher') { cycleFact(1); return; }
      setFocusIdx(i => focusTargets.length > 0 ? (i + 1) % focusTargets.length : 0);
      return;
    }

    if (e.key === 'ArrowUp' || e.key === 'k') {
      e.preventDefault();
      if (hasSwitcher && switcherOpen) { moveInDropdown(-1); return; }
      if (hasSwitcher && focusTargets[focusIdx]?.kind === 'switcher') { cycleFact(-1); return; }
      setFocusIdx(i => focusTargets.length > 0 ? (i - 1 + focusTargets.length) % focusTargets.length : 0);
      return;
    }

    if (e.key === 'Enter') {
      e.preventDefault();
      const target = focusTargets[focusIdx];
      if (!target) return;
      if (target.kind === 'switcher') {
        if (switcherOpen && dropdownFocusIdx >= 0) {
          const f = commitDetail!.files[dropdownFocusIdx];
          if (f && f.action !== 'deleted') {
            setCommitSelectedFile(f.path);
            api.fact(state.repo, f.path, state.historyCommit!).then(setFact).catch(() => setFact(null));
            setSwitcherOpen(false);
            setDropdownFocusIdx(-1);
            setFocusIdx(0);
          }
        } else {
          setSwitcherOpen(o => !o);
        }
        return;
      }
      if (target.kind === 'domain') { dispatch({ type: 'SEARCH', query: `domain:${target.value}` }); return; }
      if (target.kind === 'entity') { dispatch({ type: 'SEARCH', query: `entity:${target.value}` }); return; }
      if (target.kind === 'ref') { window.open(target.value, '_blank', 'noopener'); return; }
      if (target.kind === 'similar' && fact) { dispatch({ type: 'SIMILAR_SEARCH', path: fact.path, text: fact.body || '' }); return; }
    }
  };
  window.addEventListener('keydown', handler);
  return () => window.removeEventListener('keydown', handler);
}, [state.rightPanelFocused, focusIdx, focusTargets, switcherOpen, dropdownFocusIdx, hasSwitcher, commitDetail, commitSelectedFile, fact, state.repo, state.historyCommit, dispatch]);
```

- [ ] **Step 3: Render focus rings on interactive elements**

Add an optional `focusedValue?: string` prop to `TagCloud`:

```tsx
function TagCloud({ label, entries, color, searchPrefix, onSearch, focusedValue }: {
  label: string;
  entries: [string, number][] | string[];
  color: string;
  searchPrefix: string;
  onSearch: (query: string) => void;
  focusedValue?: string;
}) {
```

Inside `TagCloud`, add `outline` to the `<span>` style:

```tsx
outline: name === focusedValue ? `2px solid rgba(${color},0.55)` : 'none',
outlineOffset: 1,
```

Update the `renderFact` signature to accept a fourth optional parameter `focusInfo`:

```tsx
interface FocusInfo {
  target: FocusTarget | null;
}

function renderFact(fact: Fact, search: (q: string) => void, dispatch?: Dispatch<Action>, focusInfo?: FocusInfo) {
  const ft = focusInfo?.target ?? null;
```

Pass `focusedValue` to both `TagCloud` calls inside `renderFact`:

```tsx
<TagCloud label="Domains" entries={fact.domain || []} color="119,204,153" searchPrefix="domain:" onSearch={search}
  focusedValue={ft?.kind === 'domain' ? ft.value : undefined} />
<TagCloud label="Entities" entries={fact.entities || []} color="136,170,255" searchPrefix="entity:" onSearch={search}
  focusedValue={ft?.kind === 'entity' ? ft.value : undefined} />
```

Add `outline` to URL ref `<a>` elements:

```tsx
style={{
  ...(existing styles),
  outline: ft?.kind === 'ref' && ft.value === ref ? '2px solid rgba(136,170,255,0.55)' : 'none',
  outlineOffset: 2,
}}
```

Add `outline` to the "Find similar" button:

```tsx
style={{
  ...(existing styles),
  outline: ft?.kind === 'similar' ? '2px solid rgba(136,170,255,0.55)' : 'none',
  outlineOffset: 1,
}}
```

Compute `focusInfo` in `RightPanel` and pass it to every `renderFact` call:

```tsx
const focusInfo: FocusInfo = state.rightPanelFocused ? { target: focusTargets[focusIdx] ?? null } : { target: null };
```

Update all call sites of `renderFact`:
- `renderFact(fact, search)` → `renderFact(fact, search, undefined, focusInfo)`
- `renderFact(fact, search, dispatch)` → `renderFact(fact, search, dispatch, focusInfo)`

- [ ] **Step 4: Run tests**

```bash
cd web && npm test
```

Expected: all tests pass.

- [ ] **Step 5: Manual smoke test**

- In browse mode, select a fact with domains, entities, and at least one URL ref. Press `→` — focus ring appears on first domain pill. (No layout change.)
- `j` moves to next domain, then entity, then URL ref, then "Find similar" button, then wraps back.
- `k` moves backwards.
- `Enter` on a domain pill → triggers domain search; focus returns to left panel (search mode).
- `Enter` on a URL ref → new tab opens.
- `Enter` on "Find similar" → similarity search triggers.
- `←` at any point → focus returns to left panel; all rings disappear; `j`/`k` on left work normally.
- In history mode, multi-file commit:
  - Press `→` — focus ring on the switcher trigger bar.
  - `j`/`k` → cycles through facts, loading each one immediately.
  - `Enter` → dropdown opens.
  - `j`/`k` within open dropdown → moves highlight row.
  - `Enter` on highlighted row → loads that fact, closes dropdown.
  - `←` → back to left panel.

- [ ] **Step 6: Commit**

```bash
git add web/src/RightPanel.tsx
git commit -m "feat: right-panel focus system with keyboard navigation and focus rings"
```
