#!/usr/bin/env bash
# setup-graph-lib.sh installs the LadybugDB engine (liblbug + lbug.h) into the
# go-ladybug module directory in the local Go module cache so CGO can build and
# link against it.
#
# Since v0.17 the github.com/LadybugDB/go-ladybug module ships only Go source:
# no header and no native library. Its default (bundled) cgo directives expect
# both under ${SRCDIR}/lib:
#   CFLAGS:  -I${SRCDIR}/lib
#   LDFLAGS: -L${SRCDIR}/lib -llbug -Wl,-rpath,${SRCDIR}/lib
# The upstream release at github.com/LadybugDB/ladybug attaches one tarball
# per platform holding lbug.h plus the shared library, so header and library
# always come from the same engine build. This script extracts that tarball
# into <module dir>/lib.
#
# Idempotent: re-running is safe and fast (skips the download when the lib
# for ENGINE_VERSION is already in place).
# Required by: mise run test, mise run build, mise run ci.

set -euo pipefail

# The engine release and the go-ladybug module version are a matched pair: the
# binding's Go code calls the C API declared in lbug.h, and a mismatched header
# silently corrupts memory (go-ladybug v0.13.1 + engine v0.16.1 overflowed the
# caller's stack in lbug_default_system_config). Bump both together, in a
# dedicated PR with a benchmark diff (AGENTS.md rule 10).
ENGINE_VERSION="v0.17.1"
MODULE_VERSION="v0.17.0"

# Detect platform.
os=$(uname -s)
arch=$(uname -m)
case "$os/$arch" in
    Darwin/arm64)    platform="osx-arm64"; lib="liblbug.dylib" ;;
    Darwin/x86_64)   platform="osx-x86_64"; lib="liblbug.dylib" ;;
    Linux/x86_64)    platform="linux-x86_64"; lib="liblbug.so" ;;
    Linux/aarch64)   platform="linux-aarch64"; lib="liblbug.so" ;;
    Linux/arm64)     platform="linux-aarch64"; lib="liblbug.so" ;;
    *)               echo "ERROR: unsupported platform $os/$arch" >&2; exit 1 ;;
esac

# Resolve the go-ladybug module version from go.mod (single source of truth)
# and refuse to pair it with an engine it was not built against.
mod_version=$(go list -m -f '{{.Version}}' github.com/LadybugDB/go-ladybug 2>/dev/null)
if [ -z "$mod_version" ]; then
    echo "ERROR: github.com/LadybugDB/go-ladybug not in go.mod" >&2
    exit 1
fi
if [ "$mod_version" != "$MODULE_VERSION" ]; then
    echo "ERROR: go.mod has go-ladybug ${mod_version} but this script pairs engine ${ENGINE_VERSION} with ${MODULE_VERSION}." >&2
    echo "       Bump ENGINE_VERSION and MODULE_VERSION in $0 together with go.mod." >&2
    exit 1
fi

go mod download github.com/LadybugDB/go-ladybug
mod_dir=$(go list -m -f '{{.Dir}}' github.com/LadybugDB/go-ladybug)
if [ -z "$mod_dir" ] || [ ! -d "$mod_dir" ]; then
    echo "ERROR: go-ladybug module dir not found" >&2
    exit 1
fi

lib_dir="${mod_dir}/lib"
stamp="${lib_dir}/.engine-version"
if [ -f "${lib_dir}/${lib}" ] && [ -f "${lib_dir}/lbug.h" ] &&
    [ "$(cat "$stamp" 2>/dev/null)" = "$ENGINE_VERSION" ]; then
    echo "OK: ${lib} (${ENGINE_VERSION}) already present at ${lib_dir}"
    exit 0
fi

asset="liblbug-${platform}.tar.gz"
url="https://github.com/LadybugDB/ladybug/releases/download/${ENGINE_VERSION}/${asset}"

echo "Downloading ${asset} (${ENGINE_VERSION}) from upstream releases..."
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL -o "${tmp}/${asset}" "${url}"

# The module cache is read-only by default; make our slice writable so we can
# drop the lib in place. Clear any previous engine so a stale lib or header
# never survives a version bump.
chmod -R u+w "${mod_dir}"
rm -rf "${lib_dir}"
mkdir -p "${lib_dir}"
tar -xzf "${tmp}/${asset}" -C "${lib_dir}"

if [ ! -f "${lib_dir}/${lib}" ] || [ ! -f "${lib_dir}/lbug.h" ]; then
    echo "ERROR: ${lib} or lbug.h not found after extraction in ${lib_dir}" >&2
    ls -la "${lib_dir}" >&2
    exit 1
fi
printf '%s\n' "$ENGINE_VERSION" > "$stamp"

echo "Installed ${lib} + lbug.h (${ENGINE_VERSION}) -> ${lib_dir}"
