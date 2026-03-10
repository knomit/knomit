export interface ParsedRef {
  path: string;
  commit: string;
  external?: true;
}

export function parseKnomitRef(ref: string): ParsedRef | null {
  // Local: knomit:blob/<commit>/<path>
  const localMatch = ref.match(/^knomit:blob\/([^/]+)\/(.+)$/);
  if (localMatch) {
    return { path: localMatch[2], commit: localMatch[1] };
  }

  // External: knomit://<host>/<owner>/<repo>/blob/<commit>/<path>
  const extMatch = ref.match(/^knomit:\/\/[^/]+\/[^/]+\/[^/]+\/blob\/([^/]+)\/(.+)$/);
  if (extMatch) {
    return { path: extMatch[2], commit: extMatch[1], external: true };
  }

  return null;
}
