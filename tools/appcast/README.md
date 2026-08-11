# appcast

Signs desktop release artifacts and builds the Sparkle **appcast feed** the
in-app updater reads.

## Why it exists

The desktop app's updater (wails/v3 `pkg/updater`) fetches an `appcast.xml` and
verifies each item's Ed25519 signature before installing. This tool produces
both halves: the `.ed25519` sidecars and the feed itself.

Two things to know:

- Signatures cover the **SHA-256 digest** of each artifact, which is what
  wails verifies. They are *not* interchangeable with Sparkle's own
  `sign_update`, which signs file contents — the feed vocabulary is shared, the
  signing scheme is not.
- The public key is baked into every desktop binary via `-ldflags`, so clients
  pin it at build time. **A lost private key cannot be rotated** for anyone
  already running; they would have to reinstall by hand.

## Usage

```text
appcast keygen
appcast sign <file>...
appcast feed -releases <path> -link <feed-url> [-sigs <dir>] [-out <path>]
             [-require-version <semver>]
appcast version
```

### `keygen`

Run **once, by hand, never in CI**. Prints a fresh keypair: the public half goes
in the repo variable `UPDATE_PUBLIC_KEY`, the private half in the repo secret
`UPDATE_PRIVATE_KEY`. Back the private key up offline.

### `sign`

Reads the base64 private key from `$UPDATE_PRIVATE_KEY` and writes
`<file>.ed25519` beside each input. Inputs that are already sidecars are
skipped, so passing a glob over a staging dir is safe to re-run.

```sh
UPDATE_PRIVATE_KEY=... go run ./tools/appcast sign release/*
```

### `feed`

Reads GitHub's releases API JSON and emits `appcast.xml`.

| flag | default | meaning |
|---|---|---|
| `-releases` | _(required)_ | path to the GitHub releases API JSON |
| `-link` | _(required)_ | public URL the published feed will live at |
| `-sigs` | `.` | directory holding the `.ed25519` sidecars |
| `-out` | `appcast.xml` | output path |
| `-require-version` | — | fail unless the feed carries an item for this version |

```sh
go run ./tools/appcast feed \
  -releases releases.json \
  -sigs feed/sigs \
  -link "https://knomit.github.io/knomit/appcast.xml" \
  -out feed/appcast.xml
```

Drafts and prereleases are excluded. Two failure modes are deliberate:

- An **empty feed** is refused — publishing one would tell every client it is up
  to date and silently retire the update channel.
- `-require-version` catches a release whose signature sidecar went missing;
  without it the older items alone keep the feed looking healthy.

Release notes are pulled from the `<!-- appcast:begin -->` / `<!-- appcast:end -->`
fence in the GitHub release body, which must never be empty.

Both `sign` and `feed` are driven by
[`.github/workflows/release-stable.yml`](../../.github/workflows/release-stable.yml).
