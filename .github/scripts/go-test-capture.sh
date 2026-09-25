#!/usr/bin/env bash
# go-test-capture.sh NAME [go test args...]
#
# Runs `go test -json` with the given arguments, keeps the complete event
# stream in $RUNNER_TEMP/gotest-NAME.jsonl, and prints to the console what a
# plain `go test` would have: one line per package as it finishes, then the
# full output of every package that failed. The exit status is go test's.
#
# WHY -json AND NOT A TEE OF PLAIN `go test`. Plain go test DISCARDS
# everything a PASSING package wrote to stdout or stderr and prints only
# "ok  pkg". A recovered panic that chi's Recoverer pretty-printed inside a
# passing test never reaches the log at all, so a guard grepping that log can
# only fire after the package has already failed — a post-mortem, not a
# guard. -json makes the test binary run with -test.v=test2json, and then
# every test's output is in the stream, pass or fail. That is the whole
# reason this script exists; go-test-fault-guard.sh reads the stream.
#
# WHY THE CONSOLE IS STILL TERSE. -v on a package like internal/web is
# thousands of zerolog lines nobody reads on a green run. The jq filter keeps
# the per-package lines, and the failure branch prints a failed package's
# output the way plain go test would have — after the fact, in a log group.
#
# $RUNNER_TEMP is a Windows-spelled path on the Windows runner; cygpath turns
# it into one this bash can glob later (the action.yml convention).
set -uo pipefail
name=$1
shift
tmp="$(cygpath -u "$RUNNER_TEMP" 2>/dev/null || printf '%s' "$RUNNER_TEMP")"
log="$tmp/gotest-$name.jsonl"

go test -json "$@" | tee "$log" |
	jq -r 'select(.Test == null and (.Action == "pass" or .Action == "fail" or .Action == "skip"))
		| "\(.Action)\t\(.Package)\t\(.Elapsed // 0)s"'
# go test's own status, not jq's: under pipefail $? would be the LAST failing
# command, which hides "go test failed" behind a healthy jq.
rc=${PIPESTATUS[0]}

if [ "$rc" -ne 0 ]; then
	# tr: jq built for Windows ends its lines in CRLF, and a CR inside $pkg
	# would make the --arg match nothing. Seen on this exact loop.
	jq -r 'select(.Test == null and .Action == "fail") | .Package' "$log" | tr -d '\r' | while read -r pkg; do
		echo "::group::output of $pkg"
		jq -rj --arg p "$pkg" 'select(.Action == "output" and .Package == $p) | .Output' "$log"
		echo "::endgroup::"
	done
fi
exit "$rc"
