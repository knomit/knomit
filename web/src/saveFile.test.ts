import { describe, expect, it, vi } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { saveTextFile, saveExport, saveOutcomeLine, type SaveEnv } from './saveFile';

// PR #528 S1: the desktop saves through NativeService.WriteFile and says
// where; a browser uses its save dialog where it has one (success and cancel
// both known) and a download otherwise (only "handed to the browser" is
// known). Nothing says "saved" unless the save succeeded.
function env(over: Partial<SaveEnv>): SaveEnv {
  return { desktop: false, nativeWrite: vi.fn(), download: vi.fn(), ...over };
}

describe('saveTextFile', () => {
  it('in the desktop app writes through WriteFile and answers its path', async () => {
    const nativeWrite = vi.fn().mockResolvedValue('/Users/x/Library/Application Support/knomit/exports/a.json');
    const download = vi.fn();
    const out = await saveTextFile('a.json', '{"k":1}', 'application/json', env({ desktop: true, nativeWrite, download }));
    expect(nativeWrite).toHaveBeenCalledWith('a.json', '{"k":1}');
    expect(download).not.toHaveBeenCalled();
    expect(out).toEqual({ kind: 'saved', where: '/Users/x/Library/Application Support/knomit/exports/a.json' });
  });

  it('in the desktop app reports a refused write as failed, never saved', async () => {
    const out = await saveTextFile('a.json', 'x', 'text/plain', env({ desktop: true, nativeWrite: vi.fn().mockRejectedValue(new Error('disk full')) }));
    expect(out).toEqual({ kind: 'failed', message: 'disk full' });
    expect(saveOutcomeLine(out)).toBe('Not saved: disk full');
  });

  it('in a browser with a save dialog writes the chosen file', async () => {
    const write = vi.fn().mockResolvedValue(undefined);
    const close = vi.fn().mockResolvedValue(undefined);
    const picker = vi.fn().mockResolvedValue({ name: 'mine.json', createWritable: async () => ({ write, close }) });
    const nativeWrite = vi.fn();
    const out = await saveTextFile('a.json', 'x', 'text/plain', env({ picker, nativeWrite }));
    expect(picker).toHaveBeenCalledWith({ suggestedName: 'a.json' });
    expect(write).toHaveBeenCalledTimes(1);
    expect(close).toHaveBeenCalledTimes(1);
    expect(nativeWrite).not.toHaveBeenCalled();
    expect(out).toEqual({ kind: 'saved', where: 'mine.json' });
  });

  it('in a browser reports a cancelled dialog as cancelled', async () => {
    const abort = Object.assign(new Error('The user aborted a request.'), { name: 'AbortError' });
    const out = await saveTextFile('a.json', 'x', 'text/plain', env({ picker: vi.fn().mockRejectedValue(abort) }));
    expect(out).toEqual({ kind: 'cancelled' });
    expect(saveOutcomeLine(out)).toBe('Not saved: the save was cancelled.');
  });

  it('in a browser without a dialog downloads, and claims no more than that', async () => {
    const download = vi.fn();
    const out = await saveTextFile('a.json', 'x', 'application/json', env({ download }));
    expect(download).toHaveBeenCalledWith('a.json', 'x', 'application/json');
    expect(out).toEqual({ kind: 'downloaded', name: 'a.json' });
    expect(saveOutcomeLine(out)).not.toMatch(/^Saved/);
  });
});

// Export: the desktop app ALWAYS asks (native dialog); a browser does a plain download. No picker anywhere.
describe('saveExport', () => {
  it('desktop: opens the NATIVE save dialog with the suggested name and does not use the silent exports-dir write', async () => {
    const nativeSaveAs = vi.fn().mockResolvedValue('/Users/x/Downloads/mine.md');
    const nativeWrite = vi.fn();
    const download = vi.fn();
    const out = await saveExport('title-ab12cd34.md', '# doc', 'text/markdown', env({ desktop: true, nativeSaveAs, nativeWrite, download }));
    expect(nativeSaveAs).toHaveBeenCalledWith('title-ab12cd34.md', '# doc');
    expect(nativeWrite).not.toHaveBeenCalled();
    expect(download).not.toHaveBeenCalled();
    expect(out).toEqual({ kind: 'saved', where: '/Users/x/Downloads/mine.md' });
  });

  it('desktop: a cancelled dialog (empty path) writes nothing, downloads nothing, and is `cancelled`, not a failure', async () => {
    const download = vi.fn();
    const out = await saveExport('a.md', 'x', 'text/markdown', env({ desktop: true, nativeSaveAs: vi.fn().mockResolvedValue(''), nativeWrite: vi.fn(), download }));
    expect(out).toEqual({ kind: 'cancelled' });
    expect(download).not.toHaveBeenCalled();
  });

  it('desktop: a dialog that errors is failed, never saved, and never falls back to a download', async () => {
    const download = vi.fn();
    const out = await saveExport('a.md', 'x', 'text/markdown', env({ desktop: true, nativeSaveAs: vi.fn().mockRejectedValue(new Error('no dialog')), download }));
    expect(out).toEqual({ kind: 'failed', message: 'no dialog' });
    expect(download).not.toHaveBeenCalled();
  });

  it('browser: a normal download into the browser\'s download location, even where showSaveFilePicker exists', async () => {
    const picker = vi.fn();
    const e = env({ picker });
    const out = await saveExport('a.md', 'body', 'text/markdown', e);
    expect(e.download).toHaveBeenCalledWith('a.md', 'body', 'text/markdown');
    expect(picker).not.toHaveBeenCalled();                       // no picker, in any browser
    expect(out).toEqual({ kind: 'downloaded', name: 'a.md' });
  });

  it('browser: the same without a picker (Safari, Firefox): every browser behaves alike', async () => {
    const e = env({});
    const out = await saveExport('a.md', 'body', 'text/markdown', e);
    expect(e.download).toHaveBeenCalledTimes(1);
    expect(out).toEqual({ kind: 'downloaded', name: 'a.md' });
  });

  it('the source has no showSaveFilePicker path for Export and no stale-click outcome', () => {
    const src = readFileSync(resolve(__dirname, 'saveFile.ts'), 'utf8');
    const exportFn = src.slice(src.indexOf('export async function saveExport'), src.indexOf('function blobDownload'));
    expect(exportFn).not.toMatch(/picker|showSaveFilePicker/);
    expect(src).not.toMatch(/needs-gesture|canChooseLocation|saveTextFileAs/);
  });
});
