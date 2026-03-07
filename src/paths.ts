import { join, dirname } from "node:path";
import { existsSync } from "node:fs";

export function bundledLibDir(): string {
  return join(dirname(process.execPath), "lib");
}

export async function resolveLib(filename: string, libDir?: string): Promise<string | null> {
  const dir = libDir ?? bundledLibDir();
  const path = join(dir, filename);
  return existsSync(path) ? path : null;
}
