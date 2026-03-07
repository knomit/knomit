import React from "react";
import { Box, Text } from "ink";
import { glyph, type Theme } from "./theme.js";
import type { ChildItem, SearchResultItem } from "./state.js";

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
}: LeftPanelProps) {
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
      <Box flexDirection="column" flexGrow={1}>
        {items.length === 0 ? (
          <Text color={theme.dim}>  {glyph.empty} {searchActive ? "No results" : "Empty"}</Text>
        ) : (
          items.map((item, i) => {
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
          })
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
