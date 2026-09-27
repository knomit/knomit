-- F09: commit signature verification state (see internal/store/verify_*.go).
--
-- verify_context is a CACHE, keyed by commit: the policy (mode + admitted
-- signers) in force AT that commit, as the verifier's fold computed it. It
-- exists for two commits per upstream: the anchor (refs/knomit/verified/
-- <upstream>, written only while verification is on) and, for a repo that is
-- off, the off-scan watermark (meta key verify_scan:<upstream>). A missing row
-- is recomputed by folding the history from the root, so losing it costs one
-- walk and nothing else. It is NEVER read from the anchor commit's own
-- ontology file, which may carry a policy change the fold rejected.
--
-- verify_accepted is the operator's per-instance waiver list (`knomit verify
-- accept <commit>`): it waives a failing SIGNATURE on that one commit, never a
-- rejected policy change.
--
-- CREATE ... IF NOT EXISTS: the body must re-run cleanly (upWithRecovery).
CREATE TABLE IF NOT EXISTS verify_context (
    commit_hash TEXT    PRIMARY KEY,
    mode        TEXT    NOT NULL,
    signers     TEXT    NOT NULL,
    computed_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS verify_accepted (
    commit_hash TEXT    PRIMARY KEY,
    accepted_at INTEGER NOT NULL,
    note        TEXT    NOT NULL DEFAULT ''
);
