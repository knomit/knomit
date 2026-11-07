import { describe, it, expect } from 'vitest';
import { factCandidates, isFactCandidate, repoChipName, turnPrefer } from './chatFactLinks';
import type { ChatTurn, ToolCall } from './chatState';

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
      // Commit hashes, 7 to 40 lowercase hex: the server looks them up in the turn's one repo.
      'abc1234', 'deadbeefcafe', 'abc1234' + 'f'.repeat(33),
    ]) expect(isFactCandidate(c), c).toBe(true);
  });

  it('leaves everything else as code', () => {
    for (const c of [
      '', '3FC7797', 'abc123', 'abc1234' + 'f'.repeat(34), 'npm run lint', 'web/src/App.tsx', 'internal/edge/web/readonly.go',
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
    // An id is only an id at the server's length: 12 hex or the full id, never a shorter prefix.
    expect(repoChipName('kb://3ec012', repos)).toBeNull();
    expect(repoChipName('kb://3ec012f5b4d', repos)).toBeNull();
  });
  it('takes the full root id as well as its 12-hex prefix', () => {
    const full = [{ name: 'knomit-kb', id: '3ec012f5b4d2aaaabbbbccccddddeeeeffff0000' }];
    expect(repoChipName('kb://3ec012f5b4d2', full)).toBe('knomit-kb');
    expect(repoChipName('kb://3ec012f5b4d2aaaabbbbccccddddeeeeffff0000', full)).toBe('knomit-kb');
    expect(repoChipName('kb://3ec012f5b4d2aaaa', full)).toBeNull();
  });
});

describe('turnPrefer', () => {
  const call = (id: string, name: string, args: unknown, isError = false): ToolCall => ({ id, name, state: 'done', isError, args });
  const turn = (...calls: ToolCall[]): ChatTurn => ({ parts: [{ kind: 'group', calls }] }) as unknown as ChatTurn;

  it('a failed bind is not a repo the turn worked in, so one repo stays preferred', () => {
    const t = turn(
      call('a', 'knomit_bind', { repo: 'one' }),
      call('b', 'knomit_bind', { repo: 'two' }, true),
      call('c', 'knomit_query', { text: 'x' }),
    );
    expect(turnPrefer([t], 0)).toEqual(['one']);
  });

  it('a successful bind to the second repo does count, latest first', () => {
    const t = turn(
      call('a', 'knomit_bind', { repo: 'one' }),
      call('b', 'knomit_bind', { repo: 'two' }),
    );
    expect(turnPrefer([t], 0)).toEqual(['two', 'one']);
  });

  it('a failed call that is not a bind keeps counting', () => {
    const t = turn(
      call('a', 'knomit_bind', { repo: 'one' }),
      call('b', 'knomit_query', { text: 'x', repo: 'two' }, true),
    );
    expect(turnPrefer([t], 0)).toEqual(['two', 'one']);
  });

  it('a failed bind in an earlier turn leaves the binding in force as it was', () => {
    const first = turn(call('a', 'knomit_bind', { repo: 'one' }), call('b', 'knomit_bind', { repo: 'two' }, true));
    const second = turn(call('c', 'knomit_query', { text: 'x' }));
    expect(turnPrefer([first, second], 1)).toEqual(['one']);
  });
});
