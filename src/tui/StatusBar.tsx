import React from "react";
import { Box, Text } from "ink";
import { TextInput } from "@inkjs/ui";
import type { Theme } from "./theme.js";
import { glyph } from "./theme.js";

interface StatusBarProps {
  focused: boolean;
  theme: Theme;
  onSubmit: (text: string) => void;
  inputKey: number;
}

export function StatusBar({ focused, theme, onSubmit, inputKey }: StatusBarProps) {
  if (focused) {
    return (
      <Box key={inputKey}>
        <Text backgroundColor={theme.primary} color={theme.dark} bold> {glyph.search} </Text>
        <Text> </Text>
        <TextInput placeholder="search..." onSubmit={onSubmit} />
      </Box>
    );
  }

  return (
    <Box backgroundColor={theme.crust}>
      <Text color={theme.dim}>
        {" "}↑↓ navigate  ↵ open  ←→ panels  h history  / search  q quit{" "}
      </Text>
    </Box>
  );
}
