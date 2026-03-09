import React from "react";
import { Box, Text } from "ink";
import { glyph, type Theme } from "./theme.js";

interface TopBarProps {
  branch: string;
  theme: Theme;
  embeddings?: boolean;
}

export function TopBar({ branch, theme, embeddings }: TopBarProps) {
  return (
    <Box paddingX={2} backgroundColor={theme.crust} justifyContent="space-between">
      <Box gap={1}>
        <Text color={theme.primary} bold>knomit</Text>
        <Text color={theme.dim}>
          {glyph.branch} {branch}
        </Text>
      </Box>
      <Box gap={1}>
        {embeddings !== undefined && (
          <Text color={embeddings ? theme.green : theme.dim}>e</Text>
        )}
      </Box>
    </Box>
  );
}
