import { createContext } from 'react';

// AccessSecretsLink lets a component deep in a facet or a wizard send the
// reader to Access → Secrets, the one place a stored secret is created. App
// provides it:
// - `exists`: the server has an Access page at all (a read-only one does not),
//   so a hint may point there;
// - `open`: shows the gear's server pages on Access with the Secrets section
//   picked; null where that cannot happen now (no Access page, or a connect
//   commit locks the gear). Pass the element the reader used (the link): focus
//   returns to it, or to its picker, when the reader comes back;
// - `serverOpen`: the server pages are showing;
// - `returns`: how many times the server pages have closed. Anything kept
//   mounted under them that shows a secret's name or status (the picker, the
//   Remote card, the fleet's Access row) re-reads when it moves.
export type AccessSecretsLinkValue = {
  exists: boolean;
  open: ((from?: HTMLElement) => void) | null;
  serverOpen: boolean;
  returns: number;
};

export const AccessSecretsLink = createContext<AccessSecretsLinkValue>({ exists: true, open: null, serverOpen: false, returns: 0 });
