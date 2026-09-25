#!/usr/bin/env bash
# go-test-fault-guard.sh EXPECTED_LOGS
#
# Fails when any go test stream captured by go-test-capture.sh in this job
# contains a recovered nil dereference. Runs on the Windows legs only.
#
# WHY A RECOVERED NIL DEREFERENCE IS NOT HARMLESS ON WINDOWS/AMD64. Everywhere
# it is a hardware fault; on unix the kernel delivers SIGSEGV on the signal
# stack and the goroutine stack is untouched, so a recovered one is only a
# log line. On windows/amd64 the kernel dispatches the exception ON THE
# FAULTING GOROUTINE'S STACK — CONTEXT + XSTATE + EXCEPTION_RECORD plus the
# dispatcher frames. Go reserves 4 KiB for that (runtime/stack.go
# stackSystem); on AMX-capable Intel hosts the kernel lays out the 8 KiB
# XTILEDATA area as well and the frame measures ~11.6 KiB, so it overruns the
# stack into the heap span beneath it. Under the Green Tea GC (default since
# Go 1.26) that span's inline mark bits are what gets clobbered, and a LATER
# sweep dies with "fatal error: found pointer to free object", on whatever
# goroutine happens to sweep that span, nowhere near the fault.
# golang/go#81238, open; CL 828724 is the proposed fix and no released Go
# carries it.
#
# That is knomit#279: five build-test (windows-2025) runs between 2026-09-18
# and 2026-09-23 crashed exactly that way, every one right after
# TestReadOnlyRouter_FactRouteBypassRegression drove the router with a nil
# *repos.Manager. The test no longer does. This guard catches the NEXT test
# whose fault is recovered AND PRINTED — chi's Recoverer prints, testing
# prints an unrecovered one before failing — which is every case a log can
# see. It cannot see a bare `recover()` that swallows the panic silently;
# that is a review rule, not a grep: on Windows a test must never reach a nil
# dependency, whatever it does with the panic.
#
# A hit here is a bug in the TEST, not in this guard: give the test the real
# dependency it dereferenced. Do not weaken the pattern.
#
# EXPECTED_LOGS is how many go test steps ran before this one. Fewer means a
# step above was skipped or died before writing its stream; the job is red
# already in that case, and the count keeps this guard from reporting "no
# faults" over nothing (an empty glob would otherwise pass).
set -uo pipefail
expected=$1
tmp="$(cygpath -u "$RUNNER_TEMP" 2>/dev/null || printf '%s' "$RUNNER_TEMP")"
shopt -s nullglob
logs=("$tmp"/gotest-*.jsonl)
if [ "${#logs[@]}" -ne "$expected" ]; then
	echo "::warning::expected $expected go test streams, found ${#logs[@]}; a step above did not run to completion"
fi
if [ "${#logs[@]}" -eq 0 ]; then
	echo "::error::no go test streams to check"
	exit 1
fi
if grep -n 'invalid memory address or nil pointer dereference' "${logs[@]}"; then
	echo "::error::a test took a real access violation on windows/amd64; under golang/go#81238 that corrupts the heap under the goroutine stack on AMX hosts (see .github/scripts/go-test-fault-guard.sh and knomit#279)"
	exit 1
fi
echo "no recovered nil dereferences in ${#logs[@]} go test streams"
