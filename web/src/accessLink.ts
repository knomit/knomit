import { createContext } from 'react';

// AccessSecretsLink lets a component deep in a facet or a wizard send the
// reader to Access → Secrets, the one place a stored secret is created. App
// provides it: `open` shows the gear's server pages on Access with the
// Secrets section picked, and is null where that cannot happen (a read-only
// server has no Access page; a connect commit locks the gear). `serverOpen`
// says whether the server pages are showing, so a picker kept mounted under
// them (a draft wizard) re-reads the secrets when the reader comes back.
export type AccessSecretsLinkValue = { open: (() => void) | null; serverOpen: boolean };

export const AccessSecretsLink = createContext<AccessSecretsLinkValue>({ open: null, serverOpen: false });
