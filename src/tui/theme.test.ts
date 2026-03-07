import { test, expect } from "bun:test";
import { defaultTheme, type Theme } from "./theme";

test("defaultTheme has all required color keys", () => {
  expect(defaultTheme.primary).toBe("magenta");
  expect(defaultTheme.secondary).toBe("cyan");
  expect(defaultTheme.highlight).toBe("magentaBright");
  expect(defaultTheme.dimmed).toBe("gray");
  expect(defaultTheme.error).toBe("red");
  expect(defaultTheme.success).toBe("green");
});
