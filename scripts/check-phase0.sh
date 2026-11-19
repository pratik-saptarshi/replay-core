#!/usr/bin/env bash
# Phase 0 gate for replay-core: the canonical-digest law is proven
# against the committed conformance corpus, bit-for-bit, with zero
# skipped cases. One command; read-only against the repo (test logs
# go to a temp dir). Exit 0 and a final "PHASE 0 GATE: GREEN" line
# mean every check passed.
#
# Note: corpus generation tooling lives outside this repo (corpus
# governance, testdata/corpus/README.md). This gate therefore verifies
# the committed corpus by manifest hashes + full digest recomputation
# rather than by regeneration; drift against the external reference law
# is caught at reviewed corpus regenerations, not here.
# Checks: gofmt, go vet, the full test suite, zero SKIP lines in the
# corpus-driven suites, corpus file hashes against the manifest, and
# the three named golden digests asserted independently of the corpus
# tables (defense against a corpus that drifted consistently).
set -u
cd "$(dirname "$0")/.." || exit 1

command -v go >/dev/null 2>&1 || PATH="$HOME/sdk/go/bin:$HOME/go/bin:$PATH"
export GOTMPDIR="${GOTMPDIR:-$HOME/.gotmp}"
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

# 3. Full test suite
if go test ./... >"$tmp/test.log" 2>&1; then
  say "PASS go test ./..."
else
  bad "go test ./... (log: $tmp/test.log)"; cat "$tmp/test.log"
fi

# 4. Zero skips in the corpus-driven suites, and the conformance
#    accounting line must be present (every corpus case executed).
if go test -v ./internal/canonical/ ./internal/record/ ./internal/conformance/ >"$tmp/verbose.log" 2>&1; then
  skips="$(grep -c -- '--- SKIP' "$tmp/verbose.log" || true)"
  if [ "$skips" = "0" ]; then
    say "PASS zero skipped tests in corpus-driven suites"
  else
    bad "zero-skip check: $skips skipped tests"; grep -- '--- SKIP' "$tmp/verbose.log"
  fi
  if grep -q 'corpus cases executed:' "$tmp/verbose.log"; then
    say "PASS $(grep 'corpus cases executed:' "$tmp/verbose.log" | head -1 | sed 's/.*\(corpus cases executed:.*\)/\1/')"
  else
    bad "conformance accounting line missing from test output"
  fi
else
  bad "verbose corpus-suite run failed (log: $tmp/verbose.log)"; cat "$tmp/verbose.log"
fi

# 5. Corpus files match the SHA-256 hashes recorded in the manifest.
hashes="$(python3 - <<'EOF'
import json
m = json.load(open("testdata/corpus/manifest.json"))
for name, h in sorted(m.get("file_hashes", {}).items()):
    print(name, h)
EOF
)"
if [ -z "$hashes" ]; then
  bad "corpus manifest records no file_hashes"
else
  while read -r name want; do
    got="$(sha256sum "testdata/corpus/$name" | awk '{print $1}')"
    if [ "$got" = "$want" ]; then
      say "PASS corpus hash $name"
    else
      bad "corpus hash $name: got $got, manifest records $want"
    fi
  done <<EOF
$hashes
EOF
fi

# 6. Named golden digests, asserted independently of the corpus tables.
for d in \
  "sha256:c1130f7072093a8a923135960f418ce2259bfe2050190bf0a464e97ee80c9df5" \
  "sha256:22e3bf15914b5007dea08a1079d1542470a369bcbed067687f345e7e85721130" \
  "sha256:0b67497cb4c0f2ed954b7988ecc321b481e7d173c4fc201adeb86591b2fd6310" ; do
  if grep -q "$d" testdata/corpus/kernel_v1.json; then
    say "PASS golden digest present: ${d:0:20}..."
  else
    bad "golden digest missing from corpus: $d"
  fi
done

[ "$fail" -eq 0 ] && say "PHASE 0 GATE: GREEN" || say "PHASE 0 GATE: RED"
exit "$fail"
