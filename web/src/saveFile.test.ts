import { describe, expect, it, vi } from 'vitest';
import { saveTextFile, saveTextFileAs, canChooseLocation, saveOutcomeLine, type SaveEnv } from './saveFile';

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

// Export asks WHERE: saveTextFileAs never writes unasked.
describe('saveTextFileAs (asks where to save)', () => {
  const file = () => {
    const write = vi.fn().mockResolvedValue(undefined);
    const close = vi.fn().mockResolvedValue(undefined);
    const createWritable = vi.fn(async () => ({ write, close }));
    return { write, close, createWritable };
  };

  it('desktop: opens the NATIVE save dialog with the suggested name and does not use the silent exports-dir write', async () => {
    const nativeSaveAs = vi.fn().mockResolvedValue('/Users/x/Documents/mine.md');
    const nativeWrite = vi.fn();
    const out = await saveTextFileAs('title-ab12cd34.md', '# doc', 'text/markdown', env({ desktop: true, nativeSaveAs, nativeWrite }));
    expect(nativeSaveAs).toHaveBeenCalledWith('title-ab12cd34.md', '# doc');
    expect(nativeWrite).not.toHaveBeenCalled();
    expect(out).toEqual({ kind: 'saved', where: '/Users/x/Documents/mine.md' });
  });

  it('desktop: a cancelled dialog (empty path) writes nothing and is `cancelled`, not a failure', async () => {
    const out = await saveTextFileAs('a.md', 'x', 'text/markdown', env({ desktop: true, nativeSaveAs: vi.fn().mockResolvedValue(''), nativeWrite: vi.fn() }));
    expect(out).toEqual({ kind: 'cancelled' });
  });

  it('desktop: a dialog that errors is failed, never saved', async () => {
    const out = await saveTextFileAs('a.md', 'x', 'text/markdown', env({ desktop: true, nativeSaveAs: vi.fn().mockRejectedValue(new Error('no dialog')) }));
    expect(out).toEqual({ kind: 'failed', message: 'no dialog' });
  });

  it('browser with showSaveFilePicker: asks first with the suggested name, then writes the chosen file', async () => {
    const f = file();
    const picker = vi.fn().mockResolvedValue({ name: 'chosen.md', createWritable: f.createWritable });
    const download = vi.fn();
    const out = await saveTextFileAs('a.md', 'body', 'text/markdown', env({ picker, download }));
    expect(picker).toHaveBeenCalledWith({ suggestedName: 'a.md' });
    expect(f.write).toHaveBeenCalledTimes(1);
    expect(download).not.toHaveBeenCalled();
    expect(out).toEqual({ kind: 'saved', where: 'chosen.md' });
  });

  it('browser: cancelling the dialog (AbortError) opens no file and writes nothing', async () => {
    const f = file();
    const abort = Object.assign(new Error('The user aborted a request.'), { name: 'AbortError' });
    const picker = vi.fn().mockRejectedValue(abort);
    const download = vi.fn();
    const out = await saveTextFileAs('a.md', 'body', 'text/markdown', env({ picker, download }));
    expect(out).toEqual({ kind: 'cancelled' });
    expect(f.createWritable).not.toHaveBeenCalled();
    expect(download).not.toHaveBeenCalled();
  });

  it('browser: a dialog refused for want of a fresh click is `needs-gesture`, and nothing is written or downloaded', async () => {
    const picker = vi.fn().mockRejectedValue(Object.assign(new Error('must be handling a user gesture'), { name: 'SecurityError' }));
    const download = vi.fn();
    const out = await saveTextFileAs('a.md', 'body', 'text/markdown', env({ picker, download }));
    expect(out).toEqual({ kind: 'needs-gesture' });
    expect(download).not.toHaveBeenCalled();
  });

  it('a browser with no dialog (Safari, Firefox) cannot ask: canChooseLocation is false and the fallback is a plain download', async () => {
    const e = env({});
    expect(canChooseLocation(e)).toBe(false);
    expect(canChooseLocation(env({ desktop: true }))).toBe(true);
    expect(canChooseLocation(env({ picker: vi.fn() }))).toBe(true);
    const out = await saveTextFileAs('a.md', 'body', 'text/markdown', e);
    expect(e.download).toHaveBeenCalledWith('a.md', 'body', 'text/markdown');
    expect(out).toEqual({ kind: 'downloaded', name: 'a.md' });
  });
});
