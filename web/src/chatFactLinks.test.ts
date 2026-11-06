import { describe, it, expect } from 'vitest';
import { factCandidates, isFactCandidate, repoChipName } from './chatFactLinks';

describe('isFactCandidate', () => {
  it('takes 8-hex short ids, fact handles and paths, and kb:// URIs', () => {
    for (const c of [
      '3fc77970',
      'invariants/web/gate/3fc77970',
      'kb/invariants/web/gate/3fc77970.md',
      'kb/decisions/web/chat/results/3fb0f049.md',
      'synthesis/agent-access-is-blast-radius.md',
      'kb://3ec012f5b4d2/kb/decisions/web/chat/results/3fb0f049.md',
      'kb://knomit-kb/kb/a/b.md',
    ]) expect(isFactCandidate(c), c).toBe(true);
  });

  it('leaves everything else as code', () => {
    for (const c of [
      '', 'deadbeefcafe', '3FC7797', 'abc1234', 'npm run lint', 'web/src/App.tsx', 'internal/edge/web/readonly.go',
      'kb://', 'kb://3ec012f5b4d2', 'a/../b.md', 'kb/a b/c.md', 'TypeIcon', 'agent/mindev.local-8ef0cd32',
    ]) expect(isFactCandidate(c), c).toBe(false);
  });
});

describe('factCandidates', () => {
  it('finds candidates in inline code only, once each, in the order they appear', () => {
    const md = [
      'See `3fc77970` and `kb/a/b/11111111.md` (again: `3fc77970`).',
      '',
      'Plain 22222222 and kb/x/y/33333333.md in prose are not code.',
      '',
      '```',
      '44444444',
      'kb/in/a/fence.md',
      '```',
      '',
      '| practice | incidents |',
      '|---|---|',
      '| `kb://3ec012f5b4d2/kb/p/q/55555555.md` | `66666666`, `77777777` |',
      '',
      'Not a candidate: `npm test`, ``double `tick` 88888888``.',
    ].join('\n');
    expect(factCandidates(md)).toEqual([
      '3fc77970', 'kb/a/b/11111111.md', 'kb://3ec012f5b4d2/kb/p/q/55555555.md', '66666666', '77777777',
    ]);
  });

  it('is empty for an answer that cites nothing', () => {
    expect(factCandidates('No facts here, just `code`.')).toEqual([]);
  });
});

describe('repoChipName', () => {
  const repos = [{ name: 'knomit-kb', id: '3ec012f5b4d2' }, { name: 'agentic-engineering', id: '87b70fb1aeb1' }];
  it('names the repo of a bare kb:// store URI, by id or by name, and a bare repo name', () => {
    expect(repoChipName('kb://3ec012f5b4d2', repos)).toBe('knomit-kb');
    expect(repoChipName('kb://3ec012f5b4d2/', repos)).toBe('knomit-kb');
    expect(repoChipName('kb://agentic-engineering', repos)).toBe('agentic-engineering');
    expect(repoChipName('agentic-engineering', repos)).toBe('agentic-engineering');
  });
  it('is null for anything else', () => {
    expect(repoChipName('kb://000000000000', repos)).toBeNull();
    expect(repoChipName('other-repo', repos)).toBeNull();
    expect(repoChipName('kb://3ec012f5b4d2/kb/a.md', repos)).toBeNull();
  });
});
