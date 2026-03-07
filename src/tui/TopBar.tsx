import React from "react";
import { Box, Text } from "ink";
import { glyph, type Theme } from "./theme.js";

interface TopBarProps {
  branch: string;
  theme: Theme;
}

export function TopBar({ branch, theme }: TopBarProps) {
  return (
    <Box paddingX={2} backgroundColor={theme.crust}>
      <Box gap={1}>
        <Text color={theme.primary} bold>knomit</Text>
        <Text color={theme.dim}>
          {glyph.branch} {branch}
        </Text>
      </Box>
    </Box>
  );
}
