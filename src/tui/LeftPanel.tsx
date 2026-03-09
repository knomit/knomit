import React from "react";
import { Box, Text } from "ink";
import { glyph, type Theme } from "./theme.js";
import type { ChildItem, SearchResultItem } from "./state.js";
import type { LogEntry } from "../git.js";

interface LeftPanelProps {
  currentPath: string;
  children: ChildItem[];
  searchActive: boolean;
  searchResults: SearchResultItem[];
  selectedIndex: number;
  breadcrumbSelected: boolean;
  focused: boolean;
  theme: Theme;
  searchType?: "text" | "domain";
  statusText?: string;
  historyMode?: boolean;
  historyEntries?: LogEntry[];
  historySelectedIndex?: number;
  historyTarget?: string;
  availableHeight?: number;
}

function Breadcrumb({ path, selected, focused, theme }: {
  path: string; selected: boolean; focused: boolean; theme: Theme;
}) {
  const segments = path.split("/");
  const isActive = selected && focused;
  return (
    <Box>
      <Text color={isActive ? theme.yellow : theme.dim}>
        {isActive ? `${glyph.cursor} ` : "  "}
      </Text>
      {segments.map((seg, i) => (
        <React.Fragment key={i}>
          {i > 0 && <Text color={theme.dim}> {glyph.breadcrumbSep} </Text>}
          <Text
            color={i === segments.length - 1 ? (isActive ? theme.yellow : theme.text) : theme.dim}
            bold={i === segments.length - 1}
          >
            {seg}
          </Text>
        </React.Fragment>
      ))}
    </Box>
  );
}

/** Compute scroll offset to keep selectedIndex visible within viewHeight rows */
function scrollOffset(selectedIndex: number, totalItems: number, viewHeight: number): number {
  if (totalItems <= viewHeight) return 0;
  const offset = Math.max(0, selectedIndex - Math.floor(viewHeight / 2));
  return Math.min(offset, totalItems - viewHeight);
}

export function LeftPanel({
  currentPath,
  children,
  searchActive,
  searchResults,
  selectedIndex,
  breadcrumbSelected,
  focused,
  theme,
  searchType,
  statusText,
  historyMode,
  historyEntries,
  historySelectedIndex,
  historyTarget,
  availableHeight,
}: LeftPanelProps) {
  // Reserve lines for chrome: header(1) + target(1) + divider(1) + footer(1) + padding(1)
  const chromeLines = 5;
  const viewHeight = Math.max(3, (availableHeight ?? 24) - chromeLines);

  if (historyMode) {
    const entries = historyEntries ?? [];
    const selIdx = historySelectedIndex ?? 0;
    const targetLabel = historyTarget ?? "";

    // Pre-compute lines per entry: dot(1) + message(1) + connector(1) + episode tag overhead(3)
    const linesPerEntry = entries.map((entry, i) => {
      const prevEpisode = i > 0 ? entries[i - 1].episode : undefined;
      const showEpisode = entry.episode && entry.episode !== prevEpisode;
      let lines = 3; // dot + message + connector
      if (showEpisode) lines += (i > 0 ? 3 : 2); // [│] + # tag + │
      return lines;
    });

    // Find visible range that fits viewHeight, centered on selIdx
    let startIdx = selIdx;
    let endIdx = selIdx + 1;
    let usedLines = linesPerEntry[selIdx] ?? 3;

    // Expand upward then downward
    while (startIdx > 0 && usedLines + (linesPerEntry[startIdx - 1] ?? 3) <= viewHeight) {
      startIdx--;
      usedLines += linesPerEntry[startIdx] ?? 3;
    }
    while (endIdx < entries.length && usedLines + (linesPerEntry[endIdx] ?? 3) <= viewHeight) {
      usedLines += linesPerEntry[endIdx] ?? 3;
      endIdx++;
    }

    const entryOffset = startIdx;
    const visibleEntries = entries.slice(startIdx, endIdx);

    return (
      <Box flexDirection="column" width="40%" paddingX={2} paddingTop={1} overflow="hidden" backgroundColor={theme.mantle}>
        <Box>
          <Text color={theme.primary} bold>{glyph.bullet} History</Text>
          <Text color={theme.dim}> ({entries.length})</Text>
        </Box>
        <Box>
          <Text color={theme.dim}>  {targetLabel}</Text>
        </Box>
        <Box>
          <Text color={theme.dim}>  {glyph.dashDivider.repeat(20)}</Text>
        </Box>
        <Box flexDirection="column" flexGrow={1} overflow="hidden">
          {entries.length === 0 ? (
            <Text color={theme.dim}>  {glyph.empty} No history found.</Text>
          ) : (
            <>
              {entryOffset > 0 && (
                <Box>
                  <Text color={theme.dim}>  {glyph.timelineLine} {glyph.bullet}{glyph.bullet}{glyph.bullet} {entryOffset} more above</Text>
                </Box>
              )}
              {visibleEntries.map((entry, vi) => {
                const i = entryOffset + vi;
                const isActive = i === selIdx && focused;
                const isLast = i === entries.length - 1;
                const date = new Date(entry.date);
                const dateStr = date.toLocaleDateString("en-US", { month: "short", day: "numeric" });
                const timeStr = date.toLocaleTimeString("en-US", { hour: "2-digit", minute: "2-digit", hour12: false });
                const commitShort = entry.commit.slice(0, 7);
                const dotColor = isActive ? theme.yellow : theme.primary;
                const lineColor = theme.dim;
                const prevEpisode = i > 0 ? entries[i - 1].episode : undefined;
                const showEpisode = entry.episode && entry.episode !== prevEpisode;
                return (
                  <Box key={entry.commit} flexDirection="column">
                    {showEpisode && (
                      <>
                        {i > 0 && entryOffset !== i && (
                          <Box>
                            <Text color={lineColor}>  {glyph.timelineLine}</Text>
                          </Box>
                        )}
                        <Box>
                          <Text color={theme.secondary} bold>  # {entry.episode}</Text>
                        </Box>
                        <Box>
                          <Text color={lineColor}>  {glyph.timelineLine}</Text>
                        </Box>
                      </>
                    )}
                    <Box>
                      <Text color={dotColor}>{isActive ? glyph.cursor : " "} {glyph.timelineDot} </Text>
                      <Text color={isActive ? theme.yellow : theme.accent}>{commitShort}</Text>
                      <Text color={isActive ? theme.yellow : theme.dim}> {dateStr} {timeStr}</Text>
                    </Box>
                    <Box>
                      <Text color={lineColor}>  {isLast ? " " : glyph.timelineLine} </Text>
                      <Text color={isActive ? theme.yellow : theme.text} wrap="truncate-end">{entry.message}</Text>
                    </Box>
                    {!isLast && (
                      <Box>
                        <Text color={lineColor}>  {glyph.timelineLine}</Text>
                      </Box>
                    )}
                  </Box>
                );
              })}
              {entryOffset + visibleEntries.length < entries.length && (
                <Box>
                  <Text color={theme.dim}>  {glyph.timelineLine} {glyph.bullet}{glyph.bullet}{glyph.bullet} {entries.length - entryOffset - visibleEntries.length} more below</Text>
                </Box>
              )}
            </>
          )}
        </Box>
        <Box>
          <Text color={theme.dim}>  h: back  esc: back</Text>
        </Box>
      </Box>
    );
  }

  const items = searchActive
    ? searchResults.map((r) => ({
        key: r.file,
        icon: glyph.fact,
        label: r.title || r.file,
        type: "fact" as const,
        score: r.score,
      }))
    : children.map((c) => ({
        key: c.name,
        icon: c.type === "world" ? glyph.world : glyph.fact,
        label: c.name,
        type: c.type,
      }));

  // Compute scroll for regular item list (each item = 1 line)
  const itemSelIdx = breadcrumbSelected ? -1 : selectedIndex;
  const offset = scrollOffset(Math.max(0, itemSelIdx), items.length, viewHeight);
  const visibleItems = items.slice(offset, offset + viewHeight);

  return (
    <Box flexDirection="column" width="40%" paddingX={2} paddingTop={1} overflow="hidden" backgroundColor={theme.mantle}>
      {searchActive ? (
        <Box>
          <Text color={theme.secondary} bold>{glyph.search} Search results</Text>
          <Text color={theme.dim}> ({items.length})</Text>
        </Box>
      ) : (
        <Box>
          <Breadcrumb path={currentPath} selected={breadcrumbSelected} focused={focused} theme={theme} />
        </Box>
      )}
      <Box>
        <Text color={theme.dim}>  {glyph.dashDivider.repeat(20)}</Text>
      </Box>
      <Box flexDirection="column" flexGrow={1} overflow="hidden">
        {items.length === 0 ? (
          <Text color={theme.dim}>  {glyph.empty} {searchActive ? "No results" : "Empty"}</Text>
        ) : (
          <>
            {offset > 0 && (
              <Box>
                <Text color={theme.dim}>  {glyph.bullet}{glyph.bullet}{glyph.bullet} {offset} more above</Text>
              </Box>
            )}
            {visibleItems.map((item, vi) => {
              const i = offset + vi;
              const isSelected = !breadcrumbSelected && i === selectedIndex;
              const isActive = isSelected && focused;
              const iconColor = item.type === "world" ? theme.secondary : theme.accent;
              const showScore = searchType !== "domain" && "score" in item && item.score != null;
              return (
                <Box key={item.key}>
                  <Box flexShrink={0}>
                    <Text color={isActive ? theme.yellow : theme.dim}>
                      {isActive ? `${glyph.cursor} ` : "  "}
                    </Text>
                    {showScore && (
                      <Text color={isActive ? theme.yellow : theme.dim}>{String(item.score).padStart(3)}% </Text>
                    )}
                    <Text color={isActive ? theme.yellow : iconColor}>
                      {item.icon}
                    </Text>
                  </Box>
                  <Text color={isActive ? theme.yellow : theme.text} bold={isSelected} wrap="truncate-end">
                    {" "}{item.label}
                  </Text>
                </Box>
              );
            })}
            {offset + visibleItems.length < items.length && (
              <Box>
                <Text color={theme.dim}>  {glyph.bullet}{glyph.bullet}{glyph.bullet} {items.length - offset - visibleItems.length} more below</Text>
              </Box>
            )}
          </>
        )}
      </Box>
      {statusText && (
        <Box>
          <Text color={theme.dim}>  {statusText}</Text>
        </Box>
      )}
    </Box>
  );
}
