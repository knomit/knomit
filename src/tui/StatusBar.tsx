import React from "react";
import { Box, Text } from "ink";
import { TextInput } from "@inkjs/ui";
import { glyph, type Theme } from "./theme.js";

interface StatusBarProps {
  mode: "idle" | "search" | "cmdline";
  theme: Theme;
  onSearchSubmit: (text: string) => void;
  onCommandSubmit: (cmd: string) => void;
  inputKey: number;
}

export function StatusBar({ mode, theme, onSearchSubmit, onCommandSubmit, inputKey }: StatusBarProps) {
  if (mode === "search") {
    return (
      <Box key={`s${inputKey}`}>
        <Text backgroundColor={theme.primary} color={theme.dark} bold> {glyph.search} </Text>
        <Text> </Text>
        <TextInput placeholder="search..." onSubmit={onSearchSubmit} />
      </Box>
    );
  }

  if (mode === "cmdline") {
    return (
      <Box key={`c${inputKey}`}>
        <Text backgroundColor={theme.yellow} color={theme.dark} bold> : </Text>
        <Text> </Text>
        <TextInput placeholder="command..." onSubmit={onCommandSubmit} />
      </Box>
    );
  }

  return (
    <Box backgroundColor={theme.crust}>
      <Text color={theme.dim}>
        {" "}↑↓ navigate  ↵ open  ←→ panels  h history  / search  : command  q quit{" "}
      </Text>
    </Box>
  );
}
