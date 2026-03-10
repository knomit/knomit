import React, { useState, useEffect } from "react";
import { Box, Text } from "ink";
import { glyph, type Theme } from "./theme.js";

const SPINNER_FRAMES = ["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"];

interface TopBarProps {
  branch: string;
  theme: Theme;
  commit?: string | null;
  syncing?: boolean;
}

export function TopBar({ branch, theme, commit, syncing }: TopBarProps) {
  const [frame, setFrame] = useState(0);

  useEffect(() => {
    if (!syncing) return;
    const timer = setInterval(() => setFrame((f) => (f + 1) % SPINNER_FRAMES.length), 80);
    return () => clearInterval(timer);
  }, [syncing]);

  return (
    <Box paddingX={2} backgroundColor={theme.crust} justifyContent="space-between">
      <Box gap={1}>
        <Text color={theme.primary} bold>knomit</Text>
        <Text color={theme.dim}>
          {glyph.branch} {branch}
        </Text>
      </Box>
      <Box gap={1}>
        {syncing && <Text color={theme.dim}>{SPINNER_FRAMES[frame]}</Text>}
        {commit && <Text color={theme.dim}>{commit.slice(0, 7)}</Text>}
      </Box>
    </Box>
  );
}
