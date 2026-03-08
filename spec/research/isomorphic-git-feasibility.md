# Research: Git Library Feasibility (isomorphic-git, libgit2, native git)

## Current State

Knomit currently shells out to native git via `Bun.spawnSync()` in `src/git.ts`. This works but couples the system to a system-installed git binary and limits portability (browser, edge, embedded).

The question: could isomorphic-git replace native git for all knomit operations?

## Operation Coverage

### What isomorphic-git supports

| Operation | Knomit Usage | isomorphic-git |
|-----------|-------------|----------------|
| `init` | Create repo | `git.init()` |
| `add` | Stage files | `git.add()` |
| `commit` | Learn facts | `git.commit()` |
| `branch` | Machine branches | `git.branch()` |
| `checkout` | Switch branches | `git.checkout()` |
| `tag` | Learning moments | `git.tag()` / `git.listTags()` |
| `log` | Fact history | `git.log()` |
| `merge` | Reconciliation | `git.merge()` |
| `fetch` / `push` | Remote sync | `git.fetch()` / `git.push()` |
| `config` | User identity | `git.setConfig()` |
| `readBlob` / `readCommit` | File access | `git.readBlob()` / `git.readCommit()` |

These cover knomit's current operations in `src/git.ts`.

### What isomorphic-git does NOT support

| Operation | Knomit Need | Status |
|-----------|------------|--------|
| **SSH commit signing** | Provenance (per research) | **Not supported.** Only PGP/OpenPGP via `onSign` callback plugin. No SSH signing. |
| `git grep` | `knomit_search` tool | **Not available.** Would need to implement search manually over the working tree. |
| `git diff` | Reconciliation conflict display | **Limited.** No direct equivalent to `git diff --name-status`. |
| `git merge --abort` | Conflict handling | **Not available as a command.** Merge is lower-level. |
| `git verify-commit` | Trust verification | **Partial.** Can verify PGP signatures from `log()` output, but no SSH signature verification. |

### The SSH Signing Gap

This is the critical blocker. The provenance research recommends SSH commit signing with GitHub as the key server. isomorphic-git's signing infrastructure is PGP-only:

```js
// isomorphic-git's commit signing — PGP only
await git.commit({
  fs, dir,
  message: 'learn: something',
  onSign: pgp.sign,       // OpenPGP.js or isomorphic-pgp plugin
  signingKey: privateKey,  // PGP private key
})
```

There's no `onSign` adapter for SSH keys. Git's SSH signing (added in git 2.34) uses a different signature format (`-----BEGIN SSH SIGNATURE-----`) than PGP (`-----BEGIN PGP SIGNATURE-----`). The `gpgsig` field in the commit object is format-agnostic, but isomorphic-git's verification code assumes PGP.

Options:
1. **Write a custom `onSign` callback** that shells out to `ssh-keygen -Y sign` — but this defeats the purpose of using isomorphic-git (still needs a binary)
2. **Implement SSH signing in pure JS** — possible with existing crypto libraries but significant work
3. **Use PGP instead of SSH** — contradicts the provenance research recommendation (SSH is simpler, more widely adopted)
4. **Accept the gap** — use isomorphic-git for everything except signing, shell out to native git for `commit -S` and `verify-commit`

## Concurrency and Multi-Process Safety

### In-process concurrency: handled

isomorphic-git has a manager layer (`GitRefManager`, `GitIndexManager`, `GitConfigManager`) that provides **in-process async locking**:

- `GitRefManager` locks on individual ref paths (e.g., `refs/heads/main`)
- `GitIndexManager` locks on `.git/index`
- `GitConfigManager` locks on `.git/config`

This means concurrent async operations within a single Node.js/Bun process won't corrupt the repo. Two parallel `git.commit()` calls in the same process will be serialized correctly.

### Multi-process safety: NOT handled

isomorphic-git does **not** implement `.lock` files like native git. Native git creates `refs/heads/main.lock` when writing to `refs/heads/main`, preventing other git processes from writing simultaneously. isomorphic-git's locks are in-memory — invisible to other processes.

If two knomit processes (or a knomit process and a native git command) operate on the same repo simultaneously:
- Index corruption is possible
- Ref updates can race
- Loose objects can be partially written

This matters for knomit because:
- Multi-machine sync might trigger concurrent operations
- A user might run `knomit` while a sync cron job is active
- MCP server requests could overlap

### LightningFS warning

isomorphic-git's recommended filesystem backend (LightningFS) can apply file operations **out of order** and may corrupt the repo on crash. The mitigation is calling `fs.flush()` after operations, but this is easy to forget and adds latency.

For server-side use (knomit's primary environment), the Node.js `fs` module is safer but slower.

## What Native Git Gives Us for Free

| Feature | Native git | isomorphic-git |
|---------|-----------|----------------|
| SSH signing | Built-in (git 2.34+) | Not supported |
| `.lock` files | Built-in | Not implemented |
| `git grep` | Built-in, fast | Not available |
| Merge conflict resolution | Full support | Basic |
| Pack file optimization | Automatic | Limited |
| Sparse checkout | Built-in | Not available |
| Hooks | Built-in | Not available |
| `allowed_signers` | Built-in | Not available |

## Recommendation

**Stay with native git.** The current `Bun.spawnSync()` approach in `src/git.ts` is the right choice for knomit's use case.

### Why

1. **SSH signing is non-negotiable** for the provenance model. isomorphic-git doesn't support it and there's no clean path to adding it.

2. **Multi-process safety matters.** Knomit runs as an MCP server handling concurrent requests, with sync jobs running in the background. Native git's lock files prevent corruption. isomorphic-git's in-memory locks don't.

3. **`git grep` is essential** for `knomit_search`. Reimplementing full-text search over the working tree in JS is unnecessary when git does it natively.

4. **The portability argument is weak.** Knomit is a server-side knowledge system. It runs on machines with git installed. Browser portability isn't a requirement. Edge/embedded scenarios aren't in scope.

5. **The current approach works.** `Bun.spawnSync()` is simple, well-tested, and gives access to every git feature without abstraction gaps.

### When isomorphic-git would make sense

- If knomit needed to run in the browser (e.g., a web-based knowledge explorer)
- If the target environment couldn't install native git
- If signing wasn't needed

None of these apply to knomit's current architecture.

## libgit2 / Native Bindings

Also evaluated: using libgit2 (the C library) via native bindings instead of shelling out to git.

### Available bindings

| Package | Status | Last npm publish | API style |
|---------|--------|-----------------|-----------|
| **nodegit** | Effectively abandoned | ~2019 (6 years ago) | NAN/node-gyp (not N-API) |
| **node-gitteh** | Dead | Ancient (requires Node 0.8) | — |
| **libgit2 (emscripten)** | Dead | 9 years ago | WASM port |

**There is no modern N-API libgit2 package on npm.** To use libgit2 today you'd need to build bindings from scratch, either via:
- **napi-rs** + Rust `git2` crate → N-API addon
- **bun:ffi** → direct C calls to libgit2
- **Bun `cc`** → compile C wrapper

### libgit2 doesn't support SSH signing either

This is the same blocker. libgit2 has no native SSH commit signing support. GitButler (a libgit2-based client) found that "most [libgit2 clients] only support GPG and all of them shell out to GPG commands to get it done, none appear to support SSH signing." Even with perfect bindings, we'd still need to shell out for signing.

### Bun + libgit2 performance

The Bun team tested libgit2 internally and found it **3x slower than spawning git** for operations like `git init`. The overhead of FFI/N-API call marshaling can exceed the cost of process spawning for short-lived operations.

### Verdict on libgit2

Not viable for knomit:
1. No maintained JS bindings exist
2. No SSH signing support (same gap as isomorphic-git)
3. Slower than native git in Bun benchmarks
4. Building new bindings is significant effort for zero gain

## Summary: All Roads Lead to Native Git

| Approach | SSH Signing | Multi-process Safe | Maintained | Verdict |
|----------|------------|-------------------|------------|---------|
| `Bun.spawnSync("git", ...)` | Yes (2.34+) | Yes (.lock files) | N/A (uses system git) | **Use this** |
| isomorphic-git | No (PGP only) | No (in-memory locks) | Yes | Blocked |
| nodegit (libgit2) | No | Yes (.lock files) | No (abandoned) | Dead |
| New libgit2 bindings | No | Yes | Would need building | Not worth it |
| bun:ffi → libgit2 | No | Yes | Would need building | Not worth it |

The native git binary is the only option that supports SSH signing, `.lock` file safety, `git grep`, merge conflict handling, and `allowed_signers` verification — all features knomit needs.

### Hardening the current approach

Instead of switching libraries, invest in hardening the native git wrapper:

1. **Add process-level locking** — use a lockfile (`knomit.lock`) to prevent concurrent knomit operations on the same repo
2. **Verify git version** — require git 2.34+ at init time for SSH signing support
3. **Structured error handling** — parse git stderr for known error patterns (lock contention, merge conflicts, network failures) and surface actionable messages
