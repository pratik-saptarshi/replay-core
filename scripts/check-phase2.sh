#!/usr/bin/env bash
# Phase 2 gate for replay-core: the carriers (F5) and journaled
# settlement (F6) are proven assembled — one settlement story
# forks this repository through the copy carrier, settles
# path-disjoint outputs, refuses overlapping applies and
# non-fast-forward selects, and recovers a killed apply to
# exactly its pre- or post- state — and the Phase 1 gate still
# passes underneath. One command; read-only against the repo
# (test logs go to a temp dir). Exit 0 and a final
# "PHASE 2 GATE: GREEN" line mean every check passed.
#
# The gate does not discharge, and must never be read as
# discharging, the host-deferred criteria AC-F5-8, AC-F5-9, or
# AC-F5-10's extent-sharing clause (plan Decision 7): those
# drivers' real-workspace evidence belongs to a host that can
# exercise them.
#
# Checks: gofmt, go vet, per-package test runs (canonical,
# record, conformance, store, slice, snapshot, carrier, settle
# — sequentially, one package per invocation, per the plan's
# environment constraint), a zero-skip scan per package,
# presence checks that TestPhase2Gate and TestPhase1Gate ran
# and passed, and scripts/check-phase1.sh required to print
# its GREEN line (which itself requires Phase 0's).
#
# Zero-skip exceptions, stated plainly: the store package's two
# re-exec helpers, TestAppenderProcess and TestRaceProcess,
# report SKIP in the parent process on every run — by design
# (Phase 1, plan Task 6): they execute as child processes under
# TestConcurrentAppendsAcrossProcesses and
# TestSameIntentRaceAcrossProcesses, and those parents fail if
# any child fails. The settle package's TestSettleCrashChild
# does the same (Phase 2, plan Task 8) under
# TestApplyCrashRecovery and TestRecoverCompletesVerifiedJournal.
# They are the only tolerated SKIP lines, each named here and
# excluded only from its own package's log; a skip by any other
# test in any package fails this gate. The Phase 2 gate test
# itself adds no skipping helper: its crash child re-enters
# TestPhase2Gate under an environment variable, so the
# conformance package stays at zero skips.
#
# Builds are CGO builds (plan Decision 1: the SQLite driver),
# so CGO_ENABLED=1 is exported here rather than left to the
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
# scan (step 4) and the gate presence checks (step 5).
for pkg in canonical record conformance store slice snapshot carrier settle; do
  log="$tmp/test-$pkg.log"
  if go test -v -count=1 "./internal/$pkg/" >"$log" 2>&1; then
    say "PASS go test -count=1 ./internal/$pkg/"
  else
    bad "go test -count=1 ./internal/$pkg/ (log: $log)"; cat "$log"
  fi

  # 4. Zero skips in this package. The named re-exec helpers
  # documented in the header are excluded only from their own
  # package's log — the store's two from the store log, the
  # settle crash child from the settle log; in any other
  # package's log, every SKIP line is unexpected.
  case "$pkg" in
    store)
      unexpected="$(grep -- '--- SKIP' "$log" | grep -vE -- '--- SKIP: (TestAppenderProcess|TestRaceProcess) \(' || true)"
      ;;
    settle)
      unexpected="$(grep -- '--- SKIP' "$log" | grep -vE -- '--- SKIP: (TestSettleCrashChild) \(' || true)"
      ;;
    *)
      unexpected="$(grep -- '--- SKIP' "$log" || true)"
      ;;
  esac
  if [ -z "$unexpected" ]; then
    say "PASS zero skipped tests in ./internal/$pkg/"
  else
    bad "zero-skip check ./internal/$pkg/: unexpected skipped tests:"; say "$unexpected"
  fi
done

# 5. The Phase 2 and Phase 1 gate demonstrations ran and
# passed. The patterns are anchored to the tests' own PASS
# lines ("... ("), so a subtest's PASS line cannot satisfy
# either check.
if grep -qE -- '--- PASS: TestPhase2Gate \(' "$tmp/test-conformance.log"; then
  say "PASS TestPhase2Gate present and passing"
else
  bad "TestPhase2Gate PASS line missing from the conformance run"
fi
if grep -qE -- '--- PASS: TestPhase1Gate \(' "$tmp/test-conformance.log"; then
  say "PASS TestPhase1Gate present and passing"
else
  bad "TestPhase1Gate PASS line missing from the conformance run"
fi

# 6. Phase 1 must not regress: run its gate and require the
# GREEN line (which itself requires Phase 0's).
if bash scripts/check-phase1.sh >"$tmp/phase1.log" 2>&1 && grep -q 'PHASE 1 GATE: GREEN' "$tmp/phase1.log"; then
  say "PASS scripts/check-phase1.sh (PHASE 1 GATE: GREEN)"
else
  bad "scripts/check-phase1.sh did not print PHASE 1 GATE: GREEN (log: $tmp/phase1.log)"; cat "$tmp/phase1.log"
fi

[ "$fail" -eq 0 ] && say "PHASE 2 GATE: GREEN" || say "PHASE 2 GATE: RED"
exit "$fail"
