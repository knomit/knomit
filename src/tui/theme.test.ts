import { test, expect } from "bun:test";
import { defaultTheme, glyph } from "./theme";

test("defaultTheme has all Catppuccin Macchiato color keys", () => {
  expect(defaultTheme.primary).toBe("#cba6f7");
  expect(defaultTheme.secondary).toBe("#89b4fa");
  expect(defaultTheme.text).toBe("#cdd6f4");
  expect(defaultTheme.dim).toBe("#a6adc8");
  expect(defaultTheme.surface).toBe("#45475a");
  expect(defaultTheme.green).toBe("#a6e3a1");
  expect(defaultTheme.blue).toBe("#89b4fa");
  expect(defaultTheme.yellow).toBe("#f9e2af");
  expect(defaultTheme.red).toBe("#f38ba8");
  expect(defaultTheme.accent).toBe("#94e2d5");
  expect(defaultTheme.dark).toBe("#11111b");
  expect(defaultTheme.crust).toBe("#181926");
  expect(defaultTheme.mantle).toBe("#1e2030");
  expect(defaultTheme.base).toBe("#24273a");
});

test("glyph has activeIndicator and dashDivider", () => {
  expect(glyph.activeIndicator).toBe("▌");
  expect(glyph.dashDivider).toBe("╌");
});
