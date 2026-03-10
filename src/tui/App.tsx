import React, { useReducer, useEffect, useCallback, useState, useRef } from "react";
import { render, Box, useInput, useApp, useStdout } from "ink";
import type { GitRepo } from "../git.js";
import type { SearchIndex, StatsResult } from "../search-index.js";
import { parseFact, type Frontmatter } from "../facts.js";
import { parseKnomitRef } from "./refs.js";
import type { SummaryChild, RightSelectableItem, HistoricalData } from "./RightPanel.js";
import { defaultTheme } from "./theme.js";
import { reducer, initialState, type ChildItem } from "./state.js";
import { TopBar } from "./TopBar.js";
import { LeftPanel } from "./LeftPanel.js";
import { RightPanel } from "./RightPanel.js";
import { StatusBar } from "./StatusBar.js";
import { exploreHandler } from "../tools/explore.js";
import { log } from "../logger.js";

const theme = defaultTheme;
const PULL_INTERVAL_MS = parseInt(process.env.KNOMIT_POLL_INTERVAL ?? "30000", 10);

function App({ repo, searchIndex }: { repo: GitRepo; searchIndex: SearchIndex }) {
  const [state, dispatch] = useReducer(reducer, initialState);
  const { exit } = useApp();
  const { stdout } = useStdout();
  const [inputKey, setInputKey] = useState(0);

  // Track terminal size
  const [termSize, setTermSize] = useState({
    columns: stdout?.columns ?? 80,
    rows: stdout?.rows ?? 24,
  });
  useEffect(() => {
    if (!stdout) return;
    const onResize = () => setTermSize({ columns: stdout.columns, rows: stdout.rows });
    stdout.on("resize", onResize);
    return () => { stdout.off("resize", onResize); };
  }, [stdout]);

  // Periodic sync: pull from remote + re-index local changes from other processes
  const [lastPull, setLastPull] = useState(0);
  useEffect(() => {
    const timer = setInterval(async () => {
      try {
        // Pull from remote if available
        const synced = await repo.sync();
        if (synced.conflict) {
          log.warn("tui: pull found merge conflict, skipping");
        }
        // Re-index or detect changes from other processes (e.g. MCP)
        const indexed = await searchIndex.sync(repo);
        if (indexed) {
          setLastPull((n) => n + 1);
          log.info("tui: background sync detected new data");
        }
      } catch (err) {
        log.debug(`tui: background sync failed: ${err}`);
      }
    }, PULL_INTERVAL_MS);
    return () => clearInterval(timer);
  }, [repo, searchIndex]);

  // Right panel data
  const [factTitle, setFactTitle] = useState("");
  const [factBody, setFactBody] = useState("");
  const [factFrontmatter, setFactFrontmatter] = useState<Frontmatter | undefined>();
  const [stats, setStats] = useState<StatsResult | null>(null);
  const [summaryChildren, setSummaryChildren] = useState<SummaryChild[]>([]);
  const [historical, setHistorical] = useState<HistoricalData | null>(null);

  const rightItemsRef = useRef<RightSelectableItem[]>([]);
  const refCommitTarget = useRef<string | null>(null);

  const handleRightItemsChanged = useCallback((items: RightSelectableItem[]) => {
    rightItemsRef.current = items;
    dispatch({ type: "SET_RIGHT_ITEM_COUNT", count: items.length });
  }, []);

  const branch = repo.branchName;

  // Load children when path changes (not when toggling search — children are preserved)
  useEffect(() => {
    if (state.searchActive) return;
    (async () => {
      try {
        const result = await exploreHandler(repo, { path: state.currentPath }, { skipSync: true });
        dispatch({ type: "SET_CHILDREN", children: result.children });
      } catch {
        dispatch({ type: "SET_CHILDREN", children: [] });
      }
    })();
  }, [state.currentPath, lastPull]);

  // Load stats and fact summaries when statsPath changes and in summary mode
  useEffect(() => {
    if (state.rightPanelMode !== "summary") return;
    try {
      const s = searchIndex.stats(state.statsPath);
      setStats(s);
    } catch {
      setStats(null);
    }
    (async () => {
      try {
        const result = await exploreHandler(repo, { path: state.statsPath }, { skipSync: true });
        setSummaryChildren(result.children);
      } catch {
        setSummaryChildren([]);
      }
    })();
  }, [state.statsPath, state.rightPanelMode]);

  // Load fact content when currentFact changes
  useEffect(() => {
    if (!state.currentFact || state.rightPanelMode === "history") return;
    (async () => {
      try {
        const raw = await repo.readFile(state.currentFact!);
        const parsed = parseFact(raw);
        setFactTitle(parsed.title);
        setFactBody(parsed.body);
        setFactFrontmatter(parsed.frontmatter);
      } catch {
        if (state.navStack.length > 0) {
          dispatch({ type: "NAV_BACK" });
        } else {
          setFactTitle("Error loading fact");
          setFactBody("");
          setFactFrontmatter(undefined);
        }
      }
    })();
  }, [state.currentFact, state.rightPanelMode]);

  // Load history entries when entering history mode
  useEffect(() => {
    if (!state.historyMode || !state.historyTarget) return;
    (async () => {
      try {
        const targetCommit = refCommitTarget.current;
        refCommitTarget.current = null;
        const entries = await repo.log(state.historyTarget, targetCommit ?? undefined);
        dispatch({ type: "SET_HISTORY_ENTRIES", entries });
        // Sync to ref commit if navigating via ref
        if (targetCommit) {
          // Strip git revision suffixes (e.g. "^") for hash matching
          const hashPart = targetCommit.replace(/[\^~].*$/, "");
          const idx = entries.findIndex((e) => e.commit.startsWith(hashPart));
          if (idx >= 0) {
            dispatch({ type: "SET_HISTORY_SELECTED_INDEX", index: idx });
          }
        }
      } catch {
        dispatch({ type: "SET_HISTORY_ENTRIES", entries: [] });
      }
    })();
  }, [state.historyMode, state.historyTarget]);

  // Load content at selected historical commit
  const selectedHistoryEntry = state.historyMode ? state.historyEntries[state.historySelectedIndex] : null;
  useEffect(() => {
    if (!state.historyMode || !selectedHistoryEntry) {
      setHistorical(null);
      return;
    }
    const commit = selectedHistoryEntry.commit;
    const target = state.historyTarget;
    const entry = selectedHistoryEntry;
    const isFact = target.endsWith(".md");
    (async () => {
      try {
        const commitBody = await repo.commitBody(commit);
        if (isFact) {
          const [raw, lineDiff] = await Promise.all([
            repo.readFileAtCommit(target, commit),
            repo.diffFileAtCommit(target, commit),
          ]);
          setHistorical({ content: raw, lineDiff, entry, commitBody });
        } else {
          const [entries, diff] = await Promise.all([
            repo.listDirAtCommit(target, commit),
            repo.diffAtCommit(commit, target),
          ]);
          setHistorical({
            children: entries.map((e) => ({
              name: e.name,
              type: e.isDirectory ? "world" as const : "fact" as const,
            })),
            changedFiles: { added: diff.added, modified: diff.modified, deleted: diff.deleted },
            entry,
            commitBody,
          });
        }
      } catch {
        setHistorical(null);
      }
    })();
  }, [state.historyMode, selectedHistoryEntry?.commit, state.historyTarget]);

  // Keyboard handling
  useInput((input, key) => {
    if (key.ctrl && input === "c") {
      exit();
      return;
    }

    if (state.focusZone === "command" || state.focusZone === "cmdline") {
      if (key.escape) {
        dispatch({ type: "SET_FOCUS", zone: "left" });
      }
      return;
    }

    if (state.focusZone === "right") {
      if (key.escape) {
        dispatch({ type: "SET_FOCUS", zone: "left" });
        if (state.searchActive) dispatch({ type: "CLEAR_SEARCH" });
        return;
      } else if (key.leftArrow) {
        dispatch({ type: "SET_FOCUS", zone: "left" });
      } else if (key.upArrow) {
        dispatch({ type: "RIGHT_NAVIGATE_UP" });
      } else if (key.downArrow) {
        dispatch({ type: "RIGHT_NAVIGATE_DOWN" });
      } else if (key.return) {
        const item = rightItemsRef.current[state.rightSelectedIndex];
        if (item) {
          if (item.type === "domain" || item.type === "entity") {
            handleDomainSearch(item.label);
          } else if (item.type === "fact" && item.path) {
            dispatch({ type: "SET_FOCUS", zone: "left" });
            const factPath = `${state.statsPath}/${item.path}`;
            dispatch({
              type: "SET_SEARCH_RESULTS",
              results: [{ file: factPath, title: item.label, body: "", score: 0 }],
              searchType: "domain",
            });
          } else if (item.type === "ref" && item.ref && !item.ref.external) {
            refCommitTarget.current = item.ref.commit;
            dispatch({
              type: "FOLLOW_REF",
              path: item.ref.path,
              commit: item.ref.commit,
            });
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
        }
      } else if (input === "q") {
        exit();
      } else if (input === "h") {
        if (state.navStack.length > 0) {
          dispatch({ type: "NAV_BACK" });
        } else {
          dispatch({ type: "TOGGLE_HISTORY", target: state.currentFact ?? state.statsPath });
        }
      }
      return;
    }

    // Left panel focused
    if (input === "q") {
      exit();
      return;
    }
    if (key.upArrow) dispatch({ type: "NAVIGATE_UP" });
    else if (key.downArrow) dispatch({ type: "NAVIGATE_DOWN" });
    else if (key.return) {
      if (state.historyMode) return;
      if (state.breadcrumbSelected) {
        dispatch({ type: "GO_UP" });
      } else {
        dispatch({ type: "OPEN_ITEM" });
      }
    }
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
    else if (key.rightArrow) {
      if (state.rightItemCount > 0) {
        dispatch({ type: "SET_FOCUS", zone: "right" });
      }
    }
    else if (input === "/" || key.tab) {
      if (!state.historyMode) {
        dispatch({ type: "SET_FOCUS", zone: "command" });
      }
    }
    else if (input === ":") {
      if (!state.historyMode) {
        dispatch({ type: "SET_FOCUS", zone: "cmdline" });
      }
    }
    else if (input === "h") {
      if (state.navStack.length > 0) {
        dispatch({ type: "NAV_BACK" });
      } else {
        const target = state.currentFact ?? state.statsPath;
        dispatch({ type: "TOGGLE_HISTORY", target });
      }
    }
    else if (key.escape) {
      if (state.historyMode) {
        dispatch({ type: "TOGGLE_HISTORY", target: "" });
      } else if (state.searchActive) {
        dispatch({ type: "CLEAR_SEARCH" });
      }
    }
  });

  const dispatchSearch = useCallback(async (text: string, searchType?: "text" | "domain") => {
    try {
      dispatch({ type: "SET_LOADING", loading: true });
      dispatch({ type: "SET_FOCUS", zone: "left" });
      const results = await searchIndex.search({ text, min_confidence: 0 });
      dispatch({
        type: "SET_SEARCH_RESULTS",
        results: results.map((r) => ({
          file: r.path,
          title: r.title,
          body: r.body,
          score: r.score,
        })),
        searchType,
      });
    } catch {
      // ignore search errors
    } finally {
      dispatch({ type: "SET_LOADING", loading: false });
    }
  }, [searchIndex]);

  const handleDomainSearch = useCallback(async (domain: string) => {
    await dispatchSearch(domain, "domain");
  }, [dispatchSearch]);

  const handleSearchSubmit = useCallback(async (text: string) => {
    const trimmed = text.trim();
    if (!trimmed) return;
    setInputKey((k: number) => k + 1);
    await dispatchSearch(trimmed);
  }, [dispatchSearch]);

  const handleCommandSubmit = useCallback(async (text: string) => {
    const trimmed = text.trim();
    if (!trimmed) return;
    setInputKey((k: number) => k + 1);
    dispatch({ type: "SET_FOCUS", zone: "left" });
    switch (trimmed) {
      case "rebuild": {
        dispatch({ type: "SET_LOADING", loading: true });
        try {
          await searchIndex.rebuild(repo);
          dispatch({ type: "SET_CHILDREN", children: [] });
          const result = await exploreHandler(repo, { path: "worlds" }, { skipSync: true });
          dispatch({ type: "SET_CHILDREN", children: result.children });
        } finally {
          dispatch({ type: "SET_LOADING", loading: false });
        }
        break;
      }
    }
  }, [searchIndex, repo]);

  return (
    <Box flexDirection="column" width={termSize.columns} height={termSize.rows}>
      <TopBar branch={branch} theme={theme} embeddings={searchIndex.hasEmbeddings} />
      <Box flexDirection="row" flexGrow={1} overflow="hidden">
        <LeftPanel
          currentPath={state.currentPath}
          children={state.children}
          searchActive={state.searchActive}
          searchResults={state.searchResults}
          selectedIndex={state.selectedIndex}
          breadcrumbSelected={state.breadcrumbSelected}
          focused={state.focusZone === "left"}
          theme={theme}
          searchType={state.searchType}
          statusText={
            state.searchActive && !state.breadcrumbSelected
              ? state.searchResults[state.selectedIndex]?.file
              : undefined
          }
          historyMode={state.historyMode}
          historyEntries={state.historyEntries}
          historySelectedIndex={state.historySelectedIndex}
          historyTarget={state.historyTarget}
          availableHeight={termSize.rows - 2}
        />
        <RightPanel
          mode={state.rightPanelMode === "history" ? "summary" : state.rightPanelMode}
          theme={theme}
          stats={stats}
          summaryChildren={state.rightPanelMode === "summary" ? summaryChildren : undefined}
          factTitle={factTitle}
          factBody={factBody}
          factFrontmatter={factFrontmatter}
          focused={state.focusZone === "right"}
          selectedIndex={state.rightSelectedIndex}
          onItemsChanged={handleRightItemsChanged}
          historical={state.historyMode ? historical ?? undefined : undefined}
          availableHeight={termSize.rows - 2}
          historyTarget={state.historyTarget}
        />
      </Box>
      <StatusBar
        mode={state.focusZone === "command" ? "search" : state.focusZone === "cmdline" ? "cmdline" : "idle"}
        theme={theme}
        onSearchSubmit={handleSearchSubmit}
        onCommandSubmit={handleCommandSubmit}
        inputKey={inputKey}
      />
    </Box>
  );
}

export function startApp(repo: GitRepo, searchIndex: SearchIndex) {
  // Enter alternate screen buffer (like vim/htop) — terminal restores on exit
  process.stdout.write("\x1b[?1049h");

  const instance = render(<App repo={repo} searchIndex={searchIndex} />, {
    exitOnCtrlC: false,
  });

  const leave = () => {
    process.stdout.write("\x1b[?1049l");
    process.exit(0);
  };

  process.on("SIGINT", leave);
  process.on("SIGTERM", leave);

  instance.waitUntilExit().then(leave);
}
