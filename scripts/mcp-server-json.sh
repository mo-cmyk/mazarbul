#!/usr/bin/env bash
# mcp-server-json.sh: render the MCP Registry server.json for a published release.
#
# The repo-root server.json is the template: it carries the static metadata
# (name, title, description, repository) with a placeholder version and no
# packages. release-please.yml runs this after the release assets are attached,
# fills in the version and one `mcpb` package per platform bundle (GitHub
# release download URL + sha256 from checksums.txt), and publishes the result
# to registry.modelcontextprotocol.io with `mcp-publisher publish`.
#
# Usage: mcp-server-json.sh <tag> <checksums.txt>   (prints server.json)

set -euo pipefail

tag="${1:?usage: mcp-server-json.sh <tag> <checksums.txt>}"
sums="${2:?usage: mcp-server-json.sh <tag> <checksums.txt>}"
version="${tag#v}"
base="https://github.com/mo-cmyk/mazarbul/releases/download/${tag}"
template="$(dirname "$0")/../server.json"

# One bundle per target, in release.yml matrix order (darwin arm64 first: the
# registry has no per-package platform field, so clients that just take the
# first mcpb package get the most common desktop target).
platforms="darwin_arm64 darwin_amd64 linux_amd64 linux_arm64"

pkgs="[]"
for p in $platforms; do
	f="mazarbul_${tag}_${p}.mcpb"
	s=$(awk -v f="$f" '$2==f {print $1; exit}' "$sums")
	[ -n "$s" ] || { echo "ERROR: no checksum for $f in $sums" >&2; exit 1; }
	pkgs=$(jq -c --arg url "${base}/${f}" --arg sha "$s" \
		'. + [{registryType: "mcpb", identifier: $url, fileSha256: $sha, transport: {type: "stdio"}}]' \
		<<<"$pkgs")
done

jq --arg v "$version" --argjson pkgs "$pkgs" '.version = $v | .packages = $pkgs' "$template"
