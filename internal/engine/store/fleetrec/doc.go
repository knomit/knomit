// Package fleetrec reads a fleet repository's records out of git: the member
// records under <root>/members/, the certificate artifacts the fleet ontology
// declares (pki: root, crl, leaf), and the first-parent history of the
// fleet's main (the Fleet page's Members-since and Activity tabs). Every read
// is of ONE commit, and every read is bounded.
//
// It is a pure reader over go-git objects. It sits directly above the vcs
// child (internal/engine/store/internal/vcs, for the ontology at a commit and the
// tree-shape check) and imports nothing else of the store; the verify
// cluster (the judge and the integrity audit) and the parent's Service
// facade both reach UP to it, never the reverse. It must not import its
// parent package store; callers outside store import the plain data types
// (Member, Fleet, PKI, ...) from here.
package fleetrec
