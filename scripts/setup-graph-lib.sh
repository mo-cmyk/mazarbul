#!/usr/bin/env bash
# setup-graph-lib.sh installs the LadybugDB dynamic library into the local
# Go module cache so CGO can link against it.
#
# The github.com/LadybugDB/go-ladybug Go module ships only the Go source +
# lbug.h header, not the platform-specific liblbug.dylib / liblbug.so. The
# upstream release at github.com/LadybugDB/ladybug attaches the libs as
# tarballs (one per platform). This script downloads the matching tarball,
# extracts it into the module cache, and lets the existing CGO LDFLAGS
# resolve.
#
# Idempotent: re-running is safe and fast (skips download if dylib present).
# Required by: mise run test, mise run build, mise run ci.

set -euo pipefail

# Pin the engine version. Bump alongside the go-ladybug module bump per
# AGENTS.md rule 10 (dedicated PR with benchmark diff).
ENGINE_VERSION="v0.16.1"

# Detect platform.
os=$(uname -s)
arch=$(uname -m)
case "$os/$arch" in
    Darwin/arm64)    platform="osx-arm64"; cache_dir="darwin"; lib="liblbug.dylib" ;;
    Darwin/x86_64)   platform="osx-x86_64"; cache_dir="darwin"; lib="liblbug.dylib" ;;
    Linux/x86_64)    platform="linux-x86_64"; cache_dir="linux-amd64"; lib="liblbug.so" ;;
    Linux/aarch64)   platform="linux-aarch64"; cache_dir="linux-arm64"; lib="liblbug.so" ;;
    Linux/arm64)     platform="linux-aarch64"; cache_dir="linux-arm64"; lib="liblbug.so" ;;
    *)               echo "ERROR: unsupported platform $os/$arch" >&2; exit 1 ;;
esac

# Resolve the go-ladybug module version from go.mod (single source of truth).
mod_version=$(go list -m -f '{{.Version}}' github.com/LadybugDB/go-ladybug 2>/dev/null)
if [ -z "$mod_version" ]; then
    echo "ERROR: github.com/LadybugDB/go-ladybug not in go.mod" >&2
    exit 1
fi

GOMODCACHE=$(go env GOMODCACHE)
# Go encodes uppercase letters in module paths as exclamation-prefixed lowercase
# (e.g. LadybugDB -> !ladybug!d!b).
mod_dir="${GOMODCACHE}/github.com/!ladybug!d!b/go-ladybug@${mod_version}"

if [ ! -d "$mod_dir" ]; then
    echo "Downloading Go module github.com/LadybugDB/go-ladybug@${mod_version}..."
    go mod download github.com/LadybugDB/go-ladybug
fi

lib_dir="${mod_dir}/lib/dynamic/${cache_dir}"
if [ -f "${lib_dir}/${lib}" ]; then
    echo "OK: ${lib} already present at ${lib_dir}"
    exit 0
fi

asset="liblbug-${platform}.tar.gz"
url="https://github.com/LadybugDB/ladybug/releases/download/${ENGINE_VERSION}/${asset}"

echo "Downloading ${asset} (${ENGINE_VERSION}) from upstream releases..."
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL -o "${tmp}/${asset}" "${url}"

# The module cache is read-only by default; make our slice writable so we can
# drop the lib in place.
chmod -R u+w "${mod_dir}"
mkdir -p "${lib_dir}"
tar -xzf "${tmp}/${asset}" -C "${lib_dir}"

if [ ! -f "${lib_dir}/${lib}" ]; then
    echo "ERROR: ${lib} not found after extraction in ${lib_dir}" >&2
    ls -la "${lib_dir}" >&2
    exit 1
fi

echo "Installed ${lib} -> ${lib_dir}"
