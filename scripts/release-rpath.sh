#!/usr/bin/env bash
# release-rpath.sh — make a built `maz` binary find a *bundled* liblbug at
# runtime, instead of the absolute Go-module-cache path the go-ladybug cgo
# directives bake in at link time.
#
# Why this exists: `maz` dynamically links liblbug (there is no static lib
# upstream). The binding hardcodes `-Wl,-rpath,${SRCDIR}/lib/dynamic/<plat>`,
# so a freshly built binary only resolves liblbug from the *build machine's*
# module cache. Shipped in a tarball that rpath is meaningless. This script
# rewrites the binary so it looks for the lib next to itself, supporting two
# install layouts:
#   - lib beside the binary      -> @loader_path/lib   ($ORIGIN/lib)
#   - FHS prefix (bin/ + lib/)    -> @loader_path/../lib ($ORIGIN/../lib)
# The release archive bundles liblbug under `lib/`; scripts/install.sh and the
# Homebrew formula preserve that relative layout.
#
# Usage: release-rpath.sh <binary> <goos>
#   <goos> = darwin | linux   (defaults to the host via `uname`)
#
# Idempotent: re-running is safe.

set -euo pipefail

bin="${1:?usage: release-rpath.sh <binary> <goos>}"
goos="${2:-}"
if [ -z "$goos" ]; then
    case "$(uname -s)" in
        Darwin) goos="darwin" ;;
        Linux)  goos="linux" ;;
        *) echo "ERROR: unsupported host $(uname -s)" >&2; exit 1 ;;
    esac
fi

[ -f "$bin" ] || { echo "ERROR: binary not found: $bin" >&2; exit 1; }

case "$goos" in
darwin)
    command -v install_name_tool >/dev/null 2>&1 || {
        echo "ERROR: install_name_tool not found (need Xcode CLT)" >&2; exit 1
    }
    # Drop any rpath pointing into the Go module cache (the dev-only path).
    # otool -l prints "         path <P> (offset N)" lines under LC_RPATH.
    while IFS= read -r p; do
        case "$p" in
            *go-ladybug*|*/pkg/mod/*)
                install_name_tool -delete_rpath "$p" "$bin" 2>/dev/null || true
                ;;
        esac
    done < <(otool -l "$bin" | awk '/LC_RPATH/{f=1;next} f&&/^[[:space:]]*path /{print $2;f=0}')

    # Add the relocatable rpaths (skip if already present to avoid a dup error).
    have_rpath() { otool -l "$bin" | awk '/LC_RPATH/{f=1;next} f&&/^[[:space:]]*path /{print $2;f=0}' | grep -qxF "$1"; }
    for rp in "@loader_path/lib" "@loader_path/../lib"; do
        have_rpath "$rp" || install_name_tool -add_rpath "$rp" "$bin"
    done
    echo "OK: rewrote rpath on $bin (darwin) -> @loader_path/lib, @loader_path/../lib"
    ;;
linux)
    command -v patchelf >/dev/null 2>&1 || {
        echo "ERROR: patchelf not found (apt-get install -y patchelf)" >&2; exit 1
    }
    # --set-rpath replaces all existing RUNPATH/RPATH entries, so the module
    # cache path is dropped in the same step. $ORIGIN is an ELF loader token and
    # must reach patchelf literally — single quotes are intentional.
    # shellcheck disable=SC2016
    patchelf --set-rpath '$ORIGIN/lib:$ORIGIN/../lib' "$bin"
    echo "OK: rewrote rpath on $bin (linux) -> \$ORIGIN/lib:\$ORIGIN/../lib"
    ;;
*)
    echo "ERROR: unsupported goos: $goos" >&2; exit 1
    ;;
esac
