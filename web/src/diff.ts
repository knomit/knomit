export type DiffLine =
  | { kind: 'eq';  text: string }
  | { kind: 'add'; text: string }
  | { kind: 'del'; text: string };

/**
 * Compute a unified line-level diff between two strings using LCS.
 *
 * Splits on '\n' (no trailing-newline normalization). Output preserves
 * relative order: del lines for the from-only segment, then add lines
 * for the to-only segment, around an unchanged anchor.
 */
export function unifiedDiff(from: string, to: string): DiffLine[] {
  const a = from === '' ? [] : from.split('\n');
  const b = to   === '' ? [] : to.split('\n');
  const m = a.length, n = b.length;

  // LCS table — m+1 × n+1
  const dp: number[][] = Array.from({ length: m + 1 }, () => new Array(n + 1).fill(0));
  for (let i = 1; i <= m; i++) {
    for (let j = 1; j <= n; j++) {
      if (a[i - 1] === b[j - 1]) dp[i][j] = dp[i - 1][j - 1] + 1;
      else dp[i][j] = Math.max(dp[i - 1][j], dp[i][j - 1]);
    }
  }

  // Backtrack to produce the diff
  const out: DiffLine[] = [];
  let i = m, j = n;
  while (i > 0 && j > 0) {
    if (a[i - 1] === b[j - 1]) {
      out.push({ kind: 'eq', text: a[i - 1] });
      i--; j--;
    } else if (dp[i - 1][j] > dp[i][j - 1]) {
      out.push({ kind: 'del', text: a[i - 1] });
      i--;
    } else {
      out.push({ kind: 'add', text: b[j - 1] });
      j--;
    }
  }
  while (i > 0) { out.push({ kind: 'del', text: a[i - 1] }); i--; }
  while (j > 0) { out.push({ kind: 'add', text: b[j - 1] }); j--; }

  return out.reverse();
}
