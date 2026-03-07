import React from "react";
import { Box, Text } from "ink";
import type { Theme } from "./theme.js";
import { glyph, box } from "./theme.js";
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
  statusText?: string;
}

function Breadcrumb({ path, selected, focused, theme }: {
  path: string; selected: boolean; focused: boolean; theme: Theme;
}) {
  const segments = path.split("/");
  const isActive = selected && focused;
  return (
    <Box paddingX={1}>
      <Text color={isActive ? theme.highlight : theme.muted}>
        {isActive ? `${glyph.cursor} ` : "  "}
      </Text>
      {segments.map((seg, i) => (
        <React.Fragment key={i}>
          {i > 0 && <Text color={theme.muted}> {glyph.breadcrumbSep} </Text>}
          <Text
            color={i === segments.length - 1 ? (isActive ? theme.highlight : theme.primary) : theme.muted}
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
  statusText,
}: LeftPanelProps) {
  const items = searchActive
    ? searchResults.map((r) => ({
        key: r.file,
        icon: glyph.fact,
        label: r.title || r.file,
        type: "fact" as const,
      }))
    : children.map((c) => ({
        key: c.name,
        icon: c.type === "world" ? glyph.world : glyph.fact,
        label: c.name,
        type: c.type,
      }));

  return (
    <Box flexDirection="column" width="40%" borderStyle="round" borderColor={theme.muted} overflow="hidden">
      {searchActive ? (
        <Box paddingX={1}>
          <Text color={theme.secondary} bold>{glyph.search} Search results</Text>
          <Text color={theme.muted}> ({items.length})</Text>
        </Box>
      ) : (
        <Breadcrumb path={currentPath} selected={breadcrumbSelected} focused={focused} theme={theme} />
      )}
      <Box paddingX={1}>
        <Text color={theme.muted}>{box.horizontal.repeat(30)}</Text>
      </Box>
      <Box flexDirection="column" paddingX={1} flexGrow={1}>
        {items.length === 0 ? (
          <Text color={theme.muted}> {glyph.empty} {searchActive ? "No results" : "Empty"}</Text>
        ) : (
          items.map((item, i) => {
            const isSelected = !breadcrumbSelected && i === selectedIndex;
            const isActive = isSelected && focused;
            const iconColor = item.type === "world" ? theme.secondary : theme.accent;
            return (
              <Box key={item.key}>
                <Text color={isActive ? theme.highlight : theme.muted}>
                  {isActive ? `${glyph.cursor} ` : "  "}
                </Text>
                <Text color={isActive ? theme.highlight : iconColor}>
                  {item.icon}
                </Text>
                <Text color={isActive ? theme.highlight : undefined} bold={isSelected}>
                  {" "}{item.label}
                </Text>
              </Box>
            );
          })
        )}
      </Box>
      {statusText && (
        <Box paddingX={1}>
          <Text color={theme.muted}>{statusText}</Text>
        </Box>
      )}
    </Box>
  );
}
