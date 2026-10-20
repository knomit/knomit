// Package gitserve is the store's smart-HTTP git protocol handler:
// upload-pack (the curated advertisement, the pack negotiation and the pack
// itself, including shallow requests and the sideband) and receive-pack for an
// enrolled peer updating its own agent branch (the quarantine, the new-commit
// walk, the private-path closure, the mode-driven incoming-push judge, the
// all-or-nothing promote and the registration through the commit chokepoint).
//
// It sits above vcs, verify and remotesync in the store's child order. It reads a
// store through the one shared vcs handle, judges a push with verify
// (JudgeIncoming, PrivatePathClosure, VerifyModeAt), and moves a pushed branch
// through remotesync's AdvanceBranchTo; it never reaches the Service. The parent
// builds the handler once per Service (NewHandler) and re-exports PushPolicy
// and WithPushPolicy, which the web edge builds per request.
package gitserve
