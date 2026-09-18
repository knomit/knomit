# fetchlibs

Downloads knomit's **native libraries** — ONNX Runtime and daulet/tokenizers'
`libtokenizers.a` — into a per-platform lib directory.

## Why it exists

The embedding stack links against native libs that are not vendored in the repo.
This is the cross-platform replacement for the old bash/`uname`/`make` fetch
logic: pure Go + stdlib, so it runs natively on Windows as well as macOS and
Linux, with no shell, `curl`, `tar`, or `make` required. It is the single source
of truth for which versions and URLs are used — see [`spec.go`](spec.go).

## Usage

```sh
go run ./tools/fetchlibs [-only ort,tokenizers] [dest-dir]
```

- `dest-dir` defaults to `dist/<goos>-<goarch>/lib`. The Makefile passes it
  explicitly.
- `-only` fetches a comma-separated subset (`ort`, `tokenizers`); default is all.

Each library is skipped if its target file is already present, so the command is
idempotent and safe to re-run.

## When a download fails

Transport errors, **5xx** and **429 Too Many Requests** are retried — four
attempts, doubling from 2s, 14s of waiting in the worst case — and each retry
prints a line to stderr, so a CI log shows a flaky release CDN as what it was
rather than as a pause.

**Every other 4xx fails immediately.** A 404 means [`spec.go`](spec.go) pins a
release that does not exist; retrying a wrong pin only delays the message that
says so. 429 is the single exception: a rate limit is transient by definition.

On a 429 the server's `Retry-After` is honoured in both RFC 9110 forms
(delta-seconds and HTTP-date). It can only make the wait **longer**, never
shorter, and it is clamped to the same 14s overall budget — a
`Retry-After: 3600` cannot turn a CI step into an hour-long hang. A negative or
unparseable value is ignored in favour of the normal backoff, never treated as
"retry now".

Normally you don't call it directly — `make setup` does:

```sh
make setup        # both libs
make ort          # ONNX Runtime only
make tokenizers   # tokenizers only
```
