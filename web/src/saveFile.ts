// Saving a file the page built (a fleet bundle, the fleet root key).
//
// In the desktop window the page runs in a webview where an `<a download>`
// click is not a save anyone can rely on (the same webview drops a
// target="_blank" link, see externalLinks.ts), so the desktop shell's own
// NativeService.WriteFile does it: it writes into the app's exports
// directory, owner-only, and answers the path it wrote. It is reached through
// the Wails runtime by name, imported by URL as eventStream.ts does, so the
// Wails dependency stays out of the browser bundle.
//
// In a plain browser, a save dialog where the browser has one
// (showSaveFilePicker), whose success and cancel are both known; else a
// download, which is handed to the browser and nothing more is known.
//
// The outcome says only what is known: `saved` with where (the desktop path,
// or the name chosen in the dialog), `downloaded` (handed to the browser),
// `cancelled`, or `failed` with the reason. A caller says "saved" for `saved`
// alone.

export type SaveOutcome =
  | { kind: 'saved'; where: string }
  | { kind: 'downloaded'; name: string }
  | { kind: 'cancelled' }
  | { kind: 'failed'; message: string };

/** NativeService.WriteFile's bound name: packagePath.TypeName.MethodName,
 *  and NativeService is in package main (see the desktop's SettingsApp). */
export const NATIVE_WRITE_FILE = 'main.NativeService.WriteFile';
const RUNTIME_URL = '/wails/runtime.js';

type Picker = (opts: { suggestedName: string }) => Promise<{ name?: string; createWritable: () => Promise<{ write: (d: Blob) => Promise<void>; close: () => Promise<void> }> }>;

export interface SaveEnv {
  desktop: boolean;
  /** The desktop shell's WriteFile: resolves with the absolute path written. */
  nativeWrite: (name: string, text: string) => Promise<string>;
  /** The browser's save dialog, when it has one. */
  picker?: Picker;
  /** A plain download. */
  download: (name: string, text: string, type: string) => void;
}

export function defaultSaveEnv(): SaveEnv {
  const w = typeof window !== 'undefined' ? (window as Window & { __KNOMIT_DESKTOP__?: boolean; showSaveFilePicker?: Picker }) : undefined;
  return {
    desktop: Boolean(w?.__KNOMIT_DESKTOP__),
    nativeWrite: async (name, text) => {
      const m = (await import(/* @vite-ignore */ RUNTIME_URL)) as { Call: { ByName: (n: string, ...a: unknown[]) => Promise<unknown> } };
      const path = await m.Call.ByName(NATIVE_WRITE_FILE, name, text);
      if (typeof path !== 'string' || !path) throw new Error('the desktop app did not say where it wrote the file');
      return path;
    },
    picker: typeof w?.showSaveFilePicker === 'function' ? w.showSaveFilePicker.bind(w) : undefined,
    download: blobDownload,
  };
}

/** saveTextFile saves text as name, by the route env offers, and says what
 *  happened. It never throws. */
export async function saveTextFile(name: string, text: string, type = 'text/plain', env: SaveEnv = defaultSaveEnv()): Promise<SaveOutcome> {
  try {
    if (env.desktop) return { kind: 'saved', where: await env.nativeWrite(name, text) };
    if (env.picker) {
      let handle;
      try {
        handle = await env.picker({ suggestedName: name });
      } catch (e) {
        if (e instanceof Error && e.name === 'AbortError') return { kind: 'cancelled' };
        throw e;
      }
      const out = await handle.createWritable();
      await out.write(new Blob([text], { type }));
      await out.close();
      return { kind: 'saved', where: handle.name || name };
    }
    env.download(name, text, type);
    return { kind: 'downloaded', name };
  } catch (e) {
    return { kind: 'failed', message: e instanceof Error ? e.message : String(e) };
  }
}

function blobDownload(name: string, text: string, type: string): void {
  const url = URL.createObjectURL(new Blob([text], { type }));
  const a = document.createElement('a');
  a.href = url;
  a.download = name;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

/** The line a page shows for an outcome other than `saved`. */
export function saveOutcomeLine(o: SaveOutcome): string {
  switch (o.kind) {
    case 'saved': return `Saved ${o.where}`;
    case 'downloaded': return `${o.name} was handed to the browser as a download; check that it arrived.`;
    case 'cancelled': return 'Not saved: the save was cancelled.';
    case 'failed': return `Not saved: ${o.message}`;
  }
}
