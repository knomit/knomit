import React from "react";
import { Box, Text } from "ink";
import { glyph, type Theme } from "./theme.js";

interface TopBarProps {
  branch: string;
  theme: Theme;
  commit?: string | null;
}

export function TopBar({ branch, theme, commit }: TopBarProps) {
  return (
    <Box paddingX={2} backgroundColor={theme.crust} justifyContent="space-between">
      <Box gap={1}>
        <Text color={theme.primary} bold>knomit</Text>
        <Text color={theme.dim}>
          {glyph.branch} {branch}
        </Text>
      </Box>
      {commit && (
        <Text color={theme.dim}>{commit.slice(0, 7)}</Text>
      )}
    </Box>
  );
}
