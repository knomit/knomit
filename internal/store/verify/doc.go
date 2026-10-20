// Package verify is the store's judge: pure predicates over commits and trees
// that say whether a change may enter a knowledge base. It holds the SSH
// signature check (VerifyCommitSignature, CommitPayload), the tree-shape rule,
// the acceptance gate over a range (CheckRange, with the Judge's record,
// ownership, certificate and artifact rules), merge rule M3 (CheckM3) and the
// own-lineage rule E4 (CheckOwnLineage), the ontology's validations over a
// branch's net change (BranchFactsValid, ValidateNewFacts), the incoming-push
// judge receive-pack and the fetch path call (JudgeIncoming,
// PrivatePathClosure, SignerIsPusher, VerifyModeAt) and the on-demand audit.
//
// It reads a store only through *vcs.Repo (the git layer) and a fleet
// repository's records only through fleetrec; it never writes, never moves a
// ref and is never called from the fact write path (TestVerifyIsNeverInTheWritePath
// pins the names). It must not import its parent package store: the parent
// re-exports the public names callers outside store use, and keeps the
// repoHandler methods the receive-pack, sync and clone paths call (they pass
// the embedded *vcs.Repo). The own-range judge that publishes the fleet's
// main (verify_own.go) is a publish-time concern and stays in the parent,
// driving a Judge with its Entry and OwnOntology flags.
package verify
