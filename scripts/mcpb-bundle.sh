#!/usr/bin/env bash
# mcpb-bundle.sh: pack a staged release tree into an MCP Bundle (.mcpb).
#
# An MCPB (formerly DXT) is a zip with manifest.json at its root that MCP
# clients such as Claude Desktop install in one click. The manifest declares a
# `binary` server whose entry point is the bundled `maz`, started as
# `maz mcp serve` over stdio. Spec: github.com/modelcontextprotocol/mcpb
# (MANIFEST.md, manifest_version 0.3).
#
# One bundle per OS/arch: the manifest can only constrain the OS
# (`compatibility.platforms`), not the CPU, and liblbug is a per-platform
# dynamic library, so each release target ships its own bundle.
#
# Bundle layout:
#   manifest.json
#   server/maz                 (rpath @loader_path/lib resolves server/lib)
#   server/lib/<liblbug soname> (the one file the binary links, dereferenced)
#   LICENSE NOTICE README.md
#
# No data dir is configured: the bundle uses the user's default data dir, so
# it shares memory with the `maz` CLI. `maz mcp serve` initialises a missing
# data dir on first start; `maz init` is only needed to pick a provider.
#
# Usage: mcpb-bundle.sh <stage-dir> <tag> <goos> <out.mcpb>
#   <stage-dir> holds `maz` (rpath already rewritten) and `lib/liblbug.*`.
# Needs node (npx) for the pinned @anthropic-ai/mcpb CLI, plus jq.

set -euo pipefail

MCPB_CLI="@anthropic-ai/mcpb@2.1.2"

usage="usage: mcpb-bundle.sh <stage-dir> <tag> <goos> <out.mcpb>"
stage="${1:?$usage}"
tag="${2:?$usage}"
goos="${3:?$usage}"
out="${4:?$usage}"
version="${tag#v}"

[ -x "$stage/maz" ] || { echo "ERROR: no executable $stage/maz" >&2; exit 1; }

# The soname the binary actually loads (liblbug.0.dylib, liblbug.so.0, ...).
# Copy only that file, dereferenced: zip packers do not keep symlinks, so
# copying the whole symlink set would ship the 20 MB engine several times.
case "$goos" in
darwin) soname=$(otool -L "$stage/maz" | awk '/liblbug/ {print $1}' | sed 's|.*/||') ;;
linux) soname=$(readelf -d "$stage/maz" | sed -n 's/.*Shared library: \[\(liblbug[^]]*\)\].*/\1/p') ;;
*) echo "ERROR: unsupported goos: $goos" >&2; exit 1 ;;
esac
[ -n "$soname" ] || { echo "ERROR: $stage/maz does not link liblbug" >&2; exit 1; }
[ -e "$stage/lib/$soname" ] || { echo "ERROR: $stage/lib/$soname missing" >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/server/lib"
cp "$stage/maz" "$work/server/maz"
cp -L "$stage/lib/$soname" "$work/server/lib/$soname"
chmod 0644 "$work/server/lib/$soname" # module-cache / Cellar copies are read-only
for f in LICENSE NOTICE README.md; do
	[ -f "$stage/$f" ] && cp "$stage/$f" "$work/$f"
done

repo="https://github.com/mo-cmyk/mazarbul"
# shellcheck disable=SC2016 # ${__dirname} is an MCPB placeholder, not a shell var.
jq -n \
	--arg version "$version" \
	--arg goos "$goos" \
	--arg repo "$repo" \
	--arg dirname '${__dirname}' \
	'{
		manifest_version: "0.3",
		name: "mazarbul",
		display_name: "Mazarbul",
		version: $version,
		description: "Local-first memory for AI agents: entities, facts, relations and documents with semantic recall.",
		long_description: "Mazarbul is the local-first system of records for the era of agents. It gives your agent durable memory across conversations: a knowledge graph of entities, relations and facts with provenance, plus documents and semantic recall. Everything stays on your machine.\n\nThe bundle runs `maz mcp serve` against your default Mazarbul data dir, so it shares memory with the `maz` CLI. Run `maz init` once to pick an embedding provider (Ollama by default).",
		author: {name: "Moritz Steigerwald", url: "https://github.com/mo-cmyk"},
		repository: {type: "git", url: ($repo + ".git")},
		homepage: $repo,
		documentation: ($repo + "#readme"),
		support: ($repo + "/issues"),
		license: "Apache-2.0",
		keywords: ["memory", "knowledge-graph", "local-first", "agents", "mcp"],
		server: {
			type: "binary",
			entry_point: "server/maz",
			mcp_config: {
				command: ($dirname + "/server/maz"),
				args: ["mcp", "serve"],
				env: {}
			}
		},
		tools_generated: true,
		compatibility: {platforms: [$goos]}
	}' >"$work/manifest.json"

npx -y "$MCPB_CLI" validate "$work/manifest.json"
npx -y "$MCPB_CLI" pack "$work" "$out"
npx -y "$MCPB_CLI" info "$out"
