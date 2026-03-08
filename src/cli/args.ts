import type { ArgsDef } from "citty";

export const globalArgs = {
  repo: {
    type: "string",
    description: "Path to the git repository (default: ~/.knomit)",
  },
  "cache-dir": {
    type: "string",
    description: "Path to the SQLite index and cache (default: ~/.cache/knomit)",
  },
} as const satisfies ArgsDef;
