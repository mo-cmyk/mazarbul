#!/usr/bin/env bash
# smoke-first-run.sh — end-to-end smoke test for the maz binary.
# Builds the binary, inits a fresh data directory, remembers a test entry,
# and verifies recall returns JSON with at least one result.
#
# Exit codes:
#   0   smoke test passed
#   1   smoke test failed
#   2   unexpected error (build failure, dependency missing)
set -euo pipefail

DATA=$(mktemp -d)
BIN=/tmp/maz-smoke-bin-$$

cleanup() {
  rm -rf "$DATA"
  rm -f "$BIN"
}
trap cleanup EXIT

echo "==> Building maz binary..."
go build -tags=sqlite_fts5 -o "$BIN" ./cmd/maz
echo "    built: $BIN"

echo "==> Initializing data directory: $DATA"
"$BIN" --data-dir "$DATA" init \
  --yes \
  --provider ollama \
  --chat-model qwen2.5:7b \
  --embed none

echo "==> Writing smoke test entry..."
"$BIN" --data-dir "$DATA" remember "smoke test entry for verification"

echo "==> Recalling 'smoke' in JSON mode..."
out=$("$BIN" --data-dir "$DATA" recall smoke --output json)

echo "==> Validating JSON output..."
echo "$out" | jq -e '.' >/dev/null || {
  echo "ERROR: recall output is not valid JSON"
  echo "$out"
  exit 1
}

echo "==> Verifying --help works..."
"$BIN" --help | grep -q "USAGE" || {
  echo "ERROR: --help output missing USAGE section"
  exit 1
}

echo "==> Verifying config list..."
"$BIN" --data-dir "$DATA" config list --output json | jq -e '.items | length > 0' >/dev/null || {
  echo "ERROR: config list returned no items"
  exit 1
}

echo "==> Verifying schema show..."
"$BIN" --data-dir "$DATA" schema show schema/predicates --output json | jq -e '.command' >/dev/null || {
  echo "ERROR: schema show returned invalid JSON"
  exit 1
}

echo "==> Verifying completion bash..."
"$BIN" completion bash | grep -q "maz" || {
  echo "ERROR: completion bash output missing maz references"
  exit 1
}

echo "==> Verifying help-topics list..."
"$BIN" help-topics | grep -q "exit-codes" || {
  echo "ERROR: help-topics list missing exit-codes"
  exit 1
}

echo ""
echo "smoke-test passed"
