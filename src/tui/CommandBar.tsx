import React from "react";
import { Box, Text } from "ink";
import { TextInput } from "@inkjs/ui";
import type { Theme } from "./theme.js";

interface CommandBarProps {
  focused: boolean;
  theme: Theme;
  onSubmit: (text: string) => void;
  inputKey: number;
}

export function CommandBar({ focused, theme, onSubmit, inputKey }: CommandBarProps) {
  return (
    <Box borderStyle="single" borderTop={false} paddingX={1}>
      {focused ? (
        <Box key={inputKey}>
          <Text color={theme.primary} bold>{">"} </Text>
          <TextInput
            placeholder="search or /command"
            onSubmit={onSubmit}
          />
        </Box>
      ) : (
        <Text dimColor>{">"} Press / or Tab to search</Text>
      )}
    </Box>
  );
}
