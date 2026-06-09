# REST API HATEOAS Plan 01 — Completion Note

**Branch:** `feat/rest-hateoas-01` (worktree at `.claude/worktrees/rest-hateoas-01`)
**Completed:** 2026-04-11

## Endpoints now reachable under `/api/v1-new`

- `GET /api/v1-new` — API root with `_links` to `repos` and `openapi`
- `GET /api/v1-new/repos` — repo collection (HAL `CollectionView`)
- `GET /api/v1-new/repos/{repo}` — repo detail with `branches` link
- `GET /api/v1-new/repos/{repo}/branches` — branch collection
- `GET /api/v1-new/repos/{repo}/branches/{branch}` — branch root (full sub-collection link map)
- `GET /api/v1-new/repos/{repo}/branches/{branch}/facts/{path...}` — single-fact HAL envelope with structured refs and anchor-aware `_links`

All responses `application/hal+json`; all errors `application/problem+json` (RFC 9457). Legacy `/api/v1` routes untouched and mounted alongside.

## Packages touched

- **Created:** `internal/web/hal/` (link, collection, problem, writer, anchor, URLBuilder — format-agnostic HAL primitives)
- **Modified/created in `internal/web/`:** `v2router.go`, `server.go` (mount + test hooks), `branch_middleware.go`, `handlers_v2_root.go`, `handlers_v2_repos.go`, `handlers_v2_branches.go`, `handlers_v2_fact.go`, `fact_view.go`, `ref_view.go`, plus tests for each.

Zero changes to `internal/store`, `internal/repos`, `internal/fact` (verified via `git diff --stat dev -- …`).

## Tests added

43 new test functions: 17 in `internal/web/hal` + 26 in `internal/web`. Full `make test` green across the module.

## Deviations from plan

- **No `store.ErrNotFound` sentinel exists in the codebase.** The plan's Task 6.3 test referenced it; dropped in favor of the local `errFactNotFound` sentinel (which is what production code actually returns).
- **`URLBuilder.branchOrCommitPrefix` exported to `BranchOrCommitPrefix`** because `fact_view.go` in package `web` needs to call it from outside `hal`.
- Dropped the plan's dead `var _ = strings.TrimSpace` guard in `handlers_v2_fact.go`.

## Known limitations (deferred to Plan 02)

- `defaultFactReader.Exists` is a stub (always returns `false`); production structured-ref "broken" detection defaults to broken until Plan 02 wires the per-request resolver.
- Commit-anchored handlers not yet implemented — only the HEAD-anchored single-fact path is live.
- Collections beyond `/repos` and `/branches` (facts, topics, search, commits, events, stats, domains, synthesis-runs, index-rebuilds) not yet implemented — the branch-root `_links` references them, and unregistered routes return `404 problem+json`.
- Writes, async jobs, MCP profile wiring, OpenAPI spec serving — all deferred.
- `contextTODO()` placeholder used inside production readers — Plan 02 threads the request context end-to-end.

## Handoff

Plan 02 extends `FactView`/`RefView` to all remaining fact-serving endpoints, lights up the collection handlers, and delivers commit-anchored views with the full resolver.
