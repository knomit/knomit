import { test, expect } from "bun:test";
import { bundledLibDir, resolveLib } from "./paths";
import { join, dirname } from "node:path";

test("bundledLibDir returns lib/ relative to execPath", () => {
  const dir = bundledLibDir();
  expect(dir).toBe(join(dirname(process.execPath), "lib"));
});

test("resolveLib returns bundled path when file exists", async () => {
  // Create a temp file to simulate bundled lib
  const tmpDir = join(import.meta.dir, ".test-lib");
  await Bun.write(join(tmpDir, "test.so"), "fake");
  const result = await resolveLib("test.so", tmpDir);
  expect(result).toBe(join(tmpDir, "test.so"));
  // cleanup
  const { rmSync } = await import("node:fs");
  rmSync(tmpDir, { recursive: true });
});

test("resolveLib returns null when file does not exist", async () => {
  const result = await resolveLib("nonexistent.so", "/tmp/no-such-dir");
  expect(result).toBeNull();
});
