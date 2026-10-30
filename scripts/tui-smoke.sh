#!/usr/bin/env bash
# tui-smoke.sh — TUI smoke harness for the three new screens:
#   • maz recall --tui   (faceted filter strip)
#   • maz watch --demo   (sources pane / 2×3 grid)
#   • maz doc browse     (vault split-pane — TODO: command lands in Lane BC)
#
# Usage:  bash scripts/tui-smoke.sh [--artifacts <dir>]
#
# Requirements: tmux(1), curl(1), codesign(1) on macOS.
# The isolated daemon is a `maz mcp serve` process; it NEVER touches the
# production data dir.  The script asserts the temp dir path on every
# operation that could affect files.

set -euo pipefail

# ── constants ─────────────────────────────────────────────────────────────────

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
ARTIFACTS_DIR="${REPO_ROOT}/scratchpad/tui-captures"

# Override artifacts dir from --artifacts flag.
while [[ $# -gt 0 ]]; do
    case "$1" in
        --artifacts) ARTIFACTS_DIR="$2"; shift 2 ;;
        *) echo "unknown arg: $1" >&2; exit 1 ;;
    esac
done

ISOLATED_PORT=17842   # distinct from prod 7842; change if this port is in use
MAZ_BIN=""            # filled by build_maz
ISO_PID=0             # pid of the isolated mcp serve process
TMPDIR_ISO=""         # filled by start_isolated_daemon
PASS=0
FAIL=0

# ── helpers ───────────────────────────────────────────────────────────────────

log()  { printf '\033[1;34m[smoke]\033[0m %s\n' "$*"; }
ok()   { printf '\033[0;32m  PASS\033[0m %s\n' "$*"; (( PASS++ )) || true; }
fail() { printf '\033[0;31m  FAIL\033[0m %s\n' "$*"; (( FAIL++ )) || true; }
die()  { printf '\033[1;31mFATAL\033[0m %s\n' "$*" >&2; teardown; exit 1; }

# assert_safe_dir hard-fails if dir does not look like a temp test directory.
assert_safe_dir() {
    local dir="$1"
    if [[ -z "$dir" ]]; then
        die "assert_safe_dir: dir is empty"
    fi
    # Must be a subdirectory of /tmp (or /private/tmp on macOS).
    if [[ "$dir" != /tmp/maz-tui-* && "$dir" != /private/tmp/maz-tui-* ]]; then
        die "PROD-GUARD: TMPDIR_ISO='${dir}' is not under /tmp/maz-tui-* — refusing to proceed"
    fi
}

# ── phase 1: build maz ────────────────────────────────────────────────────────

build_maz() {
    log "building maz (-tags=sqlite_fts5) …"
    local out
    out="$(mktemp /tmp/maz-tui-bin-XXXXXX)"
    VERSION="$(git -C "${REPO_ROOT}" describe --tags --always --dirty 2>/dev/null || echo dev)"
    (cd "${REPO_ROOT}" && go build \
        -tags=sqlite_fts5 \
        -ldflags="-X github.com/mo-cmyk/mazarbul/internal/version.Version=${VERSION}" \
        -o "${out}" \
        ./cmd/maz) || die "go build failed"
    chmod +x "${out}"
    # macOS codesign so the binary survives Gatekeeper (silent SIGKILL otherwise).
    if [[ "$(uname -s)" == "Darwin" ]]; then
        codesign --force --sign - "${out}" 2>/dev/null || true
    fi
    MAZ_BIN="${out}"
    log "  bin: ${MAZ_BIN}"
}

# ── phase 2: start isolated daemon ───────────────────────────────────────────

start_isolated_daemon() {
    TMPDIR_ISO="$(mktemp -d /tmp/maz-tui-XXXXXX)"
    assert_safe_dir "${TMPDIR_ISO}"
    log "isolated data dir: ${TMPDIR_ISO}"
    log "starting mcp serve on unix socket + http :${ISOLATED_PORT} …"

    # Start in background; no-stdio so it doesn't block waiting for stdin.
    MAZ_DATA_DIR="${TMPDIR_ISO}" "${MAZ_BIN}" \
        --data-dir "${TMPDIR_ISO}" \
        mcp serve \
        --unix \
        "--http=127.0.0.1:${ISOLATED_PORT}" \
        --no-stdio \
        >"${TMPDIR_ISO}/mcp-serve.log" 2>&1 &
    ISO_PID=$!

    # Wait for the Unix socket to appear (up to 15 s).
    local tries=300
    while (( tries-- > 0 )); do
        if [[ -S "${TMPDIR_ISO}/mazarbul.sock" ]]; then
            log "  socket ready (pid ${ISO_PID})"
            return
        fi
        sleep 0.2
    done
    log "  mcp-serve.log:"
    cat "${TMPDIR_ISO}/mcp-serve.log" >&2 || true
    die "isolated daemon socket never appeared after 15 s"
}

# ── phase 3: seed fixtures ────────────────────────────────────────────────────

seed_fixtures() {
    assert_safe_dir "${TMPDIR_ISO}"
    log "seeding fixture documents …"

    # Write markdown files directly into the vault directory.  The doc browse
    # loader reads them via the listDocuments MCP tool which scans the vault dir
    # under the data dir.  This avoids any PROD-daemon routing concern.
    local vault="${TMPDIR_ISO}/vault"
    mkdir -p "${vault}"

    cat > "${vault}/meeting-2026-06-01.md" << 'MDEOF'
# Erebor migration kickoff

We aligned on moving the RFC integration to the new pipeline.
Sam owns the rollout; target end of Q2.

See [[Ring Mk 4000 / RFC 1149]] for spec.
MDEOF

    cat > "${vault}/erebor-roadmap.md" << 'MDEOF'
# Erebor roadmap

Key milestones for the Erebor project.

## Phase 1 — RFC integration
Move the RFC 1149 pipeline to the new connector architecture.

[[meeting-2026-06-01]] — kickoff notes
MDEOF

    cat > "${vault}/bree-brief.md" << 'MDEOF'
# Bree Ltd. brief

Bree Ltd. is the primary vendor for the Erebor rollout. Contact: Sam.

[[Project Eriador]] — dangling link (no such document, tests ⚠ rendering)
MDEOF

    # Import the vault into the graph.  listDocuments (and `maz doc browse`)
    # return document ENTITIES, not raw files on disk, so the files above are
    # invisible until imported.
    log "importing vault into the graph …"
    MAZ_DATA_DIR="${TMPDIR_ISO}" "${MAZ_BIN}" \
        --data-dir "${TMPDIR_ISO}" \
        doc import "${vault}" \
        >/dev/null 2>&1 || log "  (doc import failed — doc browse may be empty; non-fatal)"

    # Seed a memory episode via maz remember pointed at the isolated dir.
    # MAZ_DATA_DIR ensures the routing check in isDefaultDataDir returns false,
    # preventing accidental prod-daemon routing.
    log "seeding recall episodes …"
    MAZ_DATA_DIR="${TMPDIR_ISO}" "${MAZ_BIN}" \
        --data-dir "${TMPDIR_ISO}" \
        remember "Erebor migration kickoff: Sam owns the RFC 1149 rollout." \
        >/dev/null 2>&1 || log "  (remember seed failed — recall pane may be empty; non-fatal)"

    log "fixtures seeded"
}

# ── phase 4: per-screen tmux capture ─────────────────────────────────────────

mkdir -p "${ARTIFACTS_DIR}"

# capture_screen launches a tmux session running CMD, drives KEYS (newline-
# delimited), captures the pane, asserts PATTERN is present, saves artifacts.
# Args: screen_name cmd keys_file pattern
capture_screen() {
    local name="$1"
    local cmd="$2"
    local keys="$3"      # newline-delimited key sequences to send
    local pattern="$4"   # grep-E pattern that must appear in the capture
    local sess="maz-smoke-${name}-$$"
    local plain_out="${ARTIFACTS_DIR}/${name}_plain.txt"
    local color_out="${ARTIFACTS_DIR}/${name}_color.txt"

    log "── screen: ${name} ──────────────────────────────────────"
    log "  cmd: ${cmd}"

    # Start a 120×40 headless tmux session.
    tmux new-session -d -s "${sess}" -x 120 -y 40 "${cmd}" 2>/dev/null || \
        { fail "${name}: tmux session failed to start"; return; }

    # Give the TUI up to 4 seconds to render its initial frame.
    sleep 4

    # Drive key sequences.
    while IFS= read -r key; do
        [[ -z "${key}" ]] && continue
        tmux send-keys -t "${sess}" "${key}" "" 2>/dev/null || true
        sleep 0.5
    done <<< "${keys}"

    # Extra settle time after keys.
    sleep 1

    # Capture plain and colour panes.
    tmux capture-pane -p    -t "${sess}" > "${plain_out}" 2>/dev/null || true
    tmux capture-pane -p -e -t "${sess}" > "${color_out}" 2>/dev/null || true

    # Kill the session.
    tmux kill-session -t "${sess}" 2>/dev/null || true

    # Assert expected pattern in plain capture.
    if grep -qE "${pattern}" "${plain_out}" 2>/dev/null; then
        ok "${name}: pattern '${pattern}' found"
    else
        fail "${name}: pattern '${pattern}' NOT found in capture"
        log "  plain capture (first 30 lines):"
        head -30 "${plain_out}" | sed 's/^/    /' >&2 || true
    fi

    log "  artifacts: ${plain_out}  ${color_out}"
}

# ── screen A: maz recall --tui ────────────────────────────────────────────────

run_recall_screen() {
    assert_safe_dir "${TMPDIR_ISO}"

    # Keys: type a query, Tab to focus filter strip, Space to toggle a pill,
    # then r to reset, then q to quit.
    local keys
    keys="$(printf 'erebor migration\t \r')"

    capture_screen "recall" \
        "MAZ_DATA_DIR=${TMPDIR_ISO} ${MAZ_BIN} --data-dir ${TMPDIR_ISO} recall --tui erebor" \
        "${keys}" \
        "recall|RESULTS|filter|quit"
}

# ── screen B: maz watch (sources pane) ───────────────────────────────────────

run_watch_screen() {
    # --demo uses FakeEventStreamer (no live daemon needed), which includes the
    # four-row Sources() set: live, stale, down, not-connected.
    local keys
    keys="$(printf '5')"

    capture_screen "watch" \
        "${MAZ_BIN} watch --demo" \
        "${keys}" \
        "\[5\] sources|sources|local-git|not connected"
}

# ── screen C: maz doc browse ─────────────────────────────────────────────────

run_doc_browse_screen() {
    assert_safe_dir "${TMPDIR_ISO}"

    # TODO(confirm after lane lands): `maz doc browse` is added by Lane BC.
    # The command does not exist yet; skip with a clear notice rather than
    # erroring the whole harness.
    if ! MAZ_DATA_DIR="${TMPDIR_ISO}" "${MAZ_BIN}" doc --help 2>&1 | grep -q "browse"; then
        log "  SKIP doc browse — command not yet in binary (Lane BC in flight)"
        log "  # TODO(confirm after lane lands): uncomment when 'maz doc browse' ships"
        return
    fi

    # Keys: j moves down one row, Enter opens detail, L follows first link, q quits.
    local keys
    keys="$(printf 'j\n\nL')"

    capture_screen "doc-browse" \
        "MAZ_DATA_DIR=${TMPDIR_ISO} ${MAZ_BIN} --data-dir ${TMPDIR_ISO} doc browse" \
        "${keys}" \
        "DOCUMENTS|browse|Links|quit"
}

# ── teardown ──────────────────────────────────────────────────────────────────

teardown() {
    log "tearing down …"
    # Kill all smoke tmux sessions (belt-and-suspenders).
    tmux list-sessions 2>/dev/null | grep "maz-smoke-" | \
        awk -F: '{print $1}' | xargs -I{} tmux kill-session -t {} 2>/dev/null || true

    if (( ISO_PID > 0 )); then
        kill "${ISO_PID}" 2>/dev/null || true
        wait "${ISO_PID}" 2>/dev/null || true
        ISO_PID=0
    fi

    if [[ -n "${TMPDIR_ISO}" ]]; then
        assert_safe_dir "${TMPDIR_ISO}"
        rm -rf "${TMPDIR_ISO}"
        TMPDIR_ISO=""
    fi

    if [[ -n "${MAZ_BIN}" && -f "${MAZ_BIN}" ]]; then
        rm -f "${MAZ_BIN}"
        MAZ_BIN=""
    fi
}

trap teardown EXIT INT TERM

# ── main ──────────────────────────────────────────────────────────────────────

main() {
    log "TUI smoke harness starting"
    log "artifacts dir: ${ARTIFACTS_DIR}"

    build_maz
    start_isolated_daemon
    seed_fixtures

    run_recall_screen
    run_watch_screen
    run_doc_browse_screen

    log ""
    log "══════════════════════════════════════════════"
    log "Results: ${PASS} PASS  ${FAIL} FAIL"
    log "Captures: ${ARTIFACTS_DIR}/"
    log "══════════════════════════════════════════════"

    if (( FAIL > 0 )); then
        exit 1
    fi
}

main "$@"
