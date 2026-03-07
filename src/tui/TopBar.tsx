import React from "react";
import { Box, Text } from "ink";
import type { Theme } from "./theme.js";

interface TopBarProps {
  branch: string;
  theme: Theme;
}

export function TopBar({ branch, theme }: TopBarProps) {
  return (
    <Box
      borderStyle="single"
      borderBottom={false}
      paddingX={1}
      justifyContent="space-between"
    >
      <Text bold color={theme.primary}>knomit</Text>
      <Text dimColor>branch: {branch}</Text>
      <Text dimColor>↑↓ Enter ← h / Tab q</Text>
    </Box>
  );
}
