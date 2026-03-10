import { log } from "./logger";

/** Spawn a CLI process, stream stdout via onChunk, and return the full output. */
export async function runCliProcess(
  cliName: string,
  args: string[],
  stdinContent: string,
  onChunk?: (text: string) => void
): Promise<string> {
  log.info(`${cliName}: spawning (stdin ${stdinContent.length} bytes)`);
  const t0 = Date.now();
  const proc = Bun.spawn(args, {
    stdin: new Blob([stdinContent]),
    stdout: "pipe",
    stderr: "pipe",
  });

  // Drain stderr concurrently to avoid pipe deadlock
  const stderrPromise = new Response(proc.stderr).text();

  const chunks: string[] = [];
  const reader = proc.stdout.getReader();
  const decoder = new TextDecoder();
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    const chunk = decoder.decode(value, { stream: true });
    chunks.push(chunk);
    if (onChunk) onChunk(chunk);
  }

  const result = chunks.join("");
  const exitCode = await proc.exited;
  const stderr = await stderrPromise;
  const elapsed = ((Date.now() - t0) / 1000).toFixed(1);

  if (stderr.trim()) {
    log.info(`${cliName}: stderr: ${stderr.trim()}`);
  }
  log.info(`${cliName}: exited ${exitCode} in ${elapsed}s, stdout ${result.length} bytes`);

  if (exitCode !== 0) {
    throw new Error(`${cliName} CLI exited with code ${exitCode}: ${stderr}`);
  }

  return result;
}
