import { createContext } from 'react';

// AccessSecretsLink lets a component deep in a facet or a wizard send the
// reader to Access → Secrets, the one place a stored secret is created. App
// provides it:
// - `exists`: the server has an Access page at all (a read-only one does not),
//   so a hint may point there;
// - `open`: shows the gear's server pages on Access with the Secrets section
//   picked; null where that cannot happen now (no Access page, or a connect
//   commit locks the gear);
// - `serverOpen`: the server pages are showing, so a picker kept mounted
//   under them re-reads the secrets when the reader comes back.
export type AccessSecretsLinkValue = { exists: boolean; open: (() => void) | null; serverOpen: boolean };

export const AccessSecretsLink = createContext<AccessSecretsLinkValue>({ exists: true, open: null, serverOpen: false });
