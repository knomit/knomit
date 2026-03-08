# Vendored Git Design

## Goal

Ship a bundled git binary per platform via dugite-native so knomit works without system git.

## Resolution chain

```
1. System git (which git)           → use it
2. Vendored git (<exec>/vendor/git/bin/git) → use it + set env vars
3. Neither                          → error
```

When vendored git is selected, set on every spawned process:
- `GIT_EXEC_PATH` = `<vendor>/git/libexec/git-core`
- `GIT_TEMPLATE_DIR` = `<vendor>/git/share/git-core/templates`
- `GIT_SSL_CAINFO` = `<vendor>/git/ssl/cacert.pem`

## Distribution layout

```
knomit/
├── knomit
├── lib/
│   ├── libsqlite3.dylib
│   ├── vec0.dylib
│   ├── onnxruntime_binding.node
│   └── libonnxruntime.dylib
└── vendor/
    └── git/
        ├── bin/git
        ├── libexec/git-core/
        ├── share/git-core/templates/
        ├── etc/gitconfig
        └── ssl/cacert.pem
```

## Source

dugite-native v2.53.0 from https://github.com/desktop/dugite-native/releases

Platform mapping:
- `darwin-arm64` → `macOS-arm64`
- `linux-x64` → `ubuntu-x64`
- `linux-arm64` → `ubuntu-arm64`
- `win32-x64` → `windows-x64`

## Blacklist strip

Delete after extraction to reduce size:

```
git-lfs
git-credential-manager*
git-svn
git-p4
git-gui
gitk
git-daemon
git-shell
git-http-backend
git-cvsserver
git-cvsimport
git-cvsexportcommit
git-send-email
git-request-pull
git-instaweb
git-archimport
scalar
share/gitweb/
share/perl5/
**/*.py
```

Everything else stays (blacklist approach — safer than whitelist).

## Git commands audit

### Current (`src/git.ts`)

init, add, commit, checkout, branch, tag, log, merge, merge --abort, fetch, push, diff, ls-tree, grep, rev-parse, remote, config

### Future (from research docs)

| Command | Research doc | Purpose |
|---|---|---|
| `commit -S` | provenance-and-signing | SSH-signed commits |
| `verify-commit` | provenance-and-signing | Verify signatures |
| `cherry-pick` | reconciliation-architecture | Apply commits across branches |
| `branch --remote` | reconciliation-architecture | List remote machine branches |
| `clone` | cross-repo-knowledge-discovery | Import foreign repos |

### Git config settings needed (provenance)

| Setting | Purpose |
|---|---|
| `gpg.format ssh` | Use SSH signing |
| `user.signingkey` | Which key to sign with |
| `gpg.ssh.allowedSignersFile` | Trusted signers |
| `commit.gpgsign true` | Auto-sign all commits |

### External dependencies (not vendored)

- `ssh-keygen` — ships with macOS, Linux (openssh-client), Windows 10+

## Changes

### `src/git.ts`

1. Update vendored path: `vendor/git` → `vendor/git/bin/git`
2. When vendored git is selected, set `GIT_EXEC_PATH`, `GIT_TEMPLATE_DIR`, `GIT_SSL_CAINFO` in the env passed to `Bun.spawnSync`
3. Update error message

### `scripts/build-all.ts`

Add `downloadGit()` step (same pattern as `downloadVecExtension()`):
1. Download dugite-native tarball for target platform
2. Extract to `<outDir>/vendor/git/`
3. Run blacklist strip
4. Delete tarball

## Testing

- Build: `build-all.ts --platform <current>`, verify `vendor/git/bin/git --version`
- Integration: confirm fallback to vendored when no system git in PATH
- Smoke: init repo, commit, fetch/push with vendored git
