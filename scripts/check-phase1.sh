#!/usr/bin/env bash
# Phase 1 gate for replay-core: the trace store (F2), cuts/slices
# (F3), and the Git snapshot store (F4) are proven assembled — one
# synthetic run round-trips end-to-end through all three, with
# GC-survival and concurrent-append evidence — and the Phase 0
# gate still passes underneath. One command; read-only against
# the repo (test logs go to a temp dir). Exit 0 and a final
# "PHASE 1 GATE: GREEN" line mean every check passed.
#
# Checks: gofmt, go vet, per-package test runs (canonical,
# record, conformance, store, slice, snapshot — sequentially, one
# package per invocation, per the plan's environment constraint),
# a zero-skip scan per package, a presence check that
# TestPhase1Gate ran and passed, and scripts/check-phase0.sh
# required to print its GREEN line (Phase 1 must not regress
# Phase 0).
#
# Zero-skip exception, stated plainly: the store package's two
# re-exec helpers, TestAppenderProcess and TestRaceProcess,
# report SKIP in the parent process on every run — by design
# (plan Task 6): they execute as child processes under
# TestConcurrentAppendsAcrossProcesses and
# TestSameIntentRaceAcrossProcesses, and those parents fail if
# any child fails. They are the only tolerated SKIP lines, named
# here; a skip by any other test in any package fails this gate.
# The Phase 1 gate test itself adds no skipping helper: its
# appender children re-enter TestPhase1Gate under an environment
# variable, so the conformance package stays at zero skips and
# the Phase 0 gate's own scan is unaffected.
#
# Builds are CGO builds (plan Decision 1: the SQLite driver), so
# CGO_ENABLED=1 is exported here rather than left to the
# toolchain default.
set -u
cd "$(dirname "$0")/.." || exit 1

command -v go >/dev/null 2>&1 || PATH="$HOME/sdk/go/bin:$HOME/go/bin:$PATH"
export GOTMPDIR="${GOTMPDIR:-$HOME/.gotmp}"
export CGO_ENABLED=1
mkdir -p "$GOTMPDIR"

fail=0
say() { printf '%s\n' "$*"; }
bad() { say "FAIL $*"; fail=1; }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# 1. Formatting
unformatted="$(find . -name '*.go' -not -path './.git/*' -exec gofmt -l {} +)"
if [ -z "$unformatted" ]; then
  say "PASS gofmt"
else
  bad "gofmt: unformatted files: $unformatted"
fi

# 2. Vet
if go vet ./... >"$tmp/vet.log" 2>&1; then
  say "PASS go vet ./..."
else
  bad "go vet ./... (log: $tmp/vet.log)"; cat "$tmp/vet.log"
fi

# 3. Per-package tests, in plan order, one package per
# invocation; the same run's verbose log feeds the zero-skip
# scan (step 4) and the gate presence check (step 5).
for pkg in canonical record conformance store slice snapshot; do
  log="$tmp/test-$pkg.log"
  if go test -v -count=1 "./internal/$pkg/" >"$log" 2>&1; then
    say "PASS go test -count=1 ./internal/$pkg/"
  else
    bad "go test -count=1 ./internal/$pkg/ (log: $log)"; cat "$log"
  fi

  # 4. Zero skips in this package. The two named re-exec
  # helpers documented in the header are excluded only from
  # the store package's log — that is where they live; in any
  # other package's log, every SKIP line is unexpected.
  if [ "$pkg" = "store" ]; then
    unexpected="$(grep -- '--- SKIP' "$log" | grep -vE -- '--- SKIP: (TestAppenderProcess|TestRaceProcess) \(' || true)"
  else
    unexpected="$(grep -- '--- SKIP' "$log" || true)"
  fi
  if [ -z "$unexpected" ]; then
    say "PASS zero skipped tests in ./internal/$pkg/"
  else
    bad "zero-skip check ./internal/$pkg/: unexpected skipped tests:"; say "$unexpected"
  fi
done

# 5. The Phase 1 gate demonstration ran and passed. The
# pattern is anchored to the test's own PASS line ("... ("),
# so a subtest's PASS line cannot satisfy the check.
if grep -qE -- '--- PASS: TestPhase1Gate \(' "$tmp/test-conformance.log"; then
  say "PASS TestPhase1Gate present and passing"
else
  bad "TestPhase1Gate PASS line missing from the conformance run"
fi

# 6. Phase 0 must not regress: run its gate and require the
# GREEN line.
if bash scripts/check-phase0.sh >"$tmp/phase0.log" 2>&1 && grep -q 'PHASE 0 GATE: GREEN' "$tmp/phase0.log"; then
  say "PASS scripts/check-phase0.sh (PHASE 0 GATE: GREEN)"
else
  bad "scripts/check-phase0.sh did not print PHASE 0 GATE: GREEN (log: $tmp/phase0.log)"; cat "$tmp/phase0.log"
fi

[ "$fail" -eq 0 ] && say "PHASE 1 GATE: GREEN" || say "PHASE 1 GATE: RED"
exit "$fail"
