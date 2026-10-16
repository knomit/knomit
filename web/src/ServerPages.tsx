import { LogsFacet } from './LogsFacet';
import { AuthPage } from './ManageOAuth';
import type { ServerPage } from './facets';

// ServerPages is the body behind the gear: the server's own pages, which
// belong to no repository, lens or fleet. Authorizations is an indexed page
// like a target's Settings; Logs is a toolbar over the log. The fleet is not
// here: it is a target in the rail, with its own tabs.
//
// Which pages exist is App's to decide (serverPagesFor, from the read-only
// flag and the OAuth probe); `page` here is always one of them.
export function ServerPages({ page, onAuthCount }: {
  page: ServerPage;
  /** The Authorizations page reports its own count while it polls. */
  onAuthCount: (n: number) => void;
}) {
  return (
    <div data-testid="server-pages" style={body}>
      {page === 'logs' && <LogsFacet />}
      {page === 'auth' && <AuthPage onCount={onAuthCount} />}
    </div>
  );
}

// The body takes the column's remaining height and hands it on bounded, so
// the pages below scroll inside themselves.
const body: React.CSSProperties = {
  flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column', overflow: 'hidden',
};
