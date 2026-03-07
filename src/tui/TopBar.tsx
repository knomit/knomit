import React from "react";
import { Box, Text } from "ink";
import type { Theme } from "./theme.js";
import { glyph } from "./theme.js";

interface TopBarProps {
  branch: string;
  theme: Theme;
}

export function TopBar({ branch, theme }: TopBarProps) {
  return (
    <Box paddingX={1} justifyContent="space-between">
      <Box gap={1}>
        <Text color={theme.primary} bold>knomit</Text>
        <Text color={theme.muted}>
          {glyph.branch} {branch}
        </Text>
      </Box>
      <Text color={theme.muted}>
        ↑↓ navigate  ↵ open  ← back  h history  / search  q quit
      </Text>
    </Box>
  );
}
