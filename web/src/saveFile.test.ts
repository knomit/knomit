import { describe, expect, it, vi } from 'vitest';
import { saveTextFile, saveOutcomeLine, type SaveEnv } from './saveFile';

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
