import React from "react";
import { Box, Text } from "ink";
import type { Theme } from "./theme.js";
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
}

function truncatePath(path: string, maxLen: number): string {
  if (path.length <= maxLen) return path;
  return "…" + path.slice(path.length - maxLen + 1);
}

function Breadcrumb({ path, selected, focused, theme }: {
  path: string; selected: boolean; focused: boolean; theme: Theme;
}) {
  const segments = path.split("/");
  return (
    <Box paddingX={1}>
      {selected && focused ? (
        <Text color={theme.highlight} bold>▸ </Text>
      ) : (
        <Text>  </Text>
      )}
      {segments.map((seg, i) => (
        <React.Fragment key={i}>
          {i > 0 && <Text dimColor> / </Text>}
          <Text
            color={selected && focused && i === segments.length - 1 ? theme.highlight : undefined}
            bold={selected && i === segments.length - 1}
            dimColor={!selected || i < segments.length - 1}
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
}: LeftPanelProps) {
  const items = searchActive
    ? searchResults.map((r) => ({
        key: r.file,
        icon: "📄",
        label: r.title || r.file,
        dim: r.file,
      }))
    : children.map((c) => ({
        key: c.name,
        icon: c.type === "world" ? "📁" : "📄",
        label: c.name,
        dim: c.summary ?? "",
      }));

  return (
    <Box flexDirection="column" width="40%" borderStyle="single" borderRight={false} overflow="hidden">
      {searchActive ? (
        <Box paddingX={1}>
          <Text dimColor>Search results</Text>
        </Box>
      ) : (
        <Breadcrumb path={currentPath} selected={breadcrumbSelected} focused={focused} theme={theme} />
      )}
      <Box flexDirection="column" paddingX={1}>
        {items.length === 0 ? (
          <Text dimColor>{searchActive ? "No results" : "Empty"}</Text>
        ) : (
          items.map((item, i) => {
            const isSelected = !breadcrumbSelected && i === selectedIndex;
            return (
              <Text key={item.key}>
                <Text
                  color={isSelected && focused ? theme.highlight : undefined}
                  bold={isSelected}
                >
                  {isSelected && focused ? "▸ " : "  "}
                  {item.icon} {item.label}
                </Text>
                {item.dim && !isSelected ? (
                  <Text dimColor> — {item.dim}</Text>
                ) : null}
              </Text>
            );
          })
        )}
      </Box>
    </Box>
  );
}
