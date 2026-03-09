import { appendFileSync, openSync, closeSync, statSync, renameSync, unlinkSync, existsSync, mkdirSync } from "node:fs";
import { dirname } from "node:path";

export type LogLevel = "debug" | "info" | "warn" | "error";

const LEVELS: Record<LogLevel, number> = {
  debug: 0,
  info: 1,
  warn: 2,
  error: 3,
};

function resolveLevel(): LogLevel {
  const env = process.env.KNOMIT_LOG_LEVEL?.toLowerCase();
  if (env && env in LEVELS) return env as LogLevel;
  if (process.env.KNOMIT_VERBOSE === "1" || process.env.KNOMIT_VERBOSE === "true") return "debug";
  return "info";
}

const level = resolveLevel();
const threshold = LEVELS[level];

let logFilePath: string | null = null;
let maxLogSize = 1024 * 1024; // 1 MB
let maxLogFiles = 3;

function shouldLog(l: LogLevel): boolean {
  return LEVELS[l] >= threshold;
}

function format(l: LogLevel, msg: string): string {
  return `knomit[${l}]: ${msg}`;
}

function rotateIfNeeded(): void {
  if (!logFilePath) return;
  try {
    const st = statSync(logFilePath);
    if (st.size < maxLogSize) return;
  } catch {
    return;
  }

  // Rotate: .2 -> .3 (delete), .1 -> .2, current -> .1
  for (let i = maxLogFiles - 1; i >= 1; i--) {
    const from = i === 1 ? logFilePath : `${logFilePath}.${i}`;
    const to = `${logFilePath}.${i + 1}`;
    if (i === maxLogFiles - 1) {
      try { unlinkSync(to); } catch { /* ignore */ }
    }
    if (existsSync(from)) {
      try { renameSync(from, to); } catch { /* ignore */ }
    }
  }
  try { renameSync(logFilePath, `${logFilePath}.1`); } catch { /* ignore */ }
}

function emit(l: LogLevel, msg: string): void {
  if (!shouldLog(l)) return;
  const line = format(l, msg);
  if (logFilePath) {
    rotateIfNeeded();
    appendFileSync(logFilePath, line + "\n");
  } else {
    console.error(line);
  }
}

/**
 * Redirect all log output to a file instead of stderr.
 * Call once at startup when running in TUI mode.
 */
export function setLogFile(path: string, options?: { maxSize?: number; maxFiles?: number }): void {
  logFilePath = path;
  if (options?.maxSize) maxLogSize = options.maxSize;
  if (options?.maxFiles) maxLogFiles = options.maxFiles;
  // Ensure parent directory and file exist
  mkdirSync(dirname(path), { recursive: true });
  const fd = openSync(path, "a");
  closeSync(fd);
}

export function getLogFilePath(): string | null {
  return logFilePath;
}

export const log = {
  debug(msg: string) { emit("debug", msg); },
  info(msg: string) { emit("info", msg); },
  warn(msg: string) { emit("warn", msg); },
  error(msg: string) { emit("error", msg); },
};
