#!/bin/sh
# install.sh — install a prebuilt `maz` from GitHub Releases, the easy way.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/mo-cmyk/mazarbul/main/scripts/install.sh | sh
#   ./scripts/install.sh [options]
#
# Options:
#   --prefix DIR        install prefix (default: $HOME/.local)
#   --version vX.Y.Z    install a specific release (default: latest on the channel)
#   --channel NAME      release channel: stable (default) or nightly
#   --no-modify-path    do not touch shell rc files (just print the PATH line)
#   --no-ollama         do not install Ollama / pull models
#   --init              run `maz init` non-interactively after install
#   -h, --help          this help
#
# What it does, in order:
#   1. detect OS/arch, resolve the release, download the archive + checksums.txt,
#      VERIFY the sha256 before placing anything;
#   2. install `maz` -> <prefix>/bin and the bundled engine lib -> <prefix>/lib
#      (maz dynamically links liblbug; the rpath resolves it from ../lib — keep
#      bin/ and lib/ together), plus the bundled man pages -> <prefix>/share/man
#      so `man maz` works;
#   3. add <prefix>/bin to PATH in your shell rc (unless --no-modify-path);
#   4. ensure a local model provider: install Ollama if missing and pull the
#      embedding + chat models maz needs (unless --no-ollama; skipped if already
#      present). maz needs its OWN provider — without one, recall is disabled.
#
# Private (pre-launch) installs: set MAZ_INSTALL_TOKEN (or GITHUB_TOKEN) to a
# token with read access to the repo contents (a fine-grained PAT with
# "Contents: read" is enough). It is read from the environment only — never
# logged, written to disk, or placed on a URL — and authenticates the GitHub
# API asset fetch; it is dropped on the redirect to the signed storage URL.
#
# Env overrides (also handy for testing):
#   MAZ_VERSION, MAZ_CHANNEL (stable|nightly), MAZ_BASE_URL, MAZ_INSTALL_PREFIX,
#   MAZ_INSTALL_TOKEN / GITHUB_TOKEN (private-repo auth; MAZ_INSTALL_TOKEN wins),
#   MAZ_API_BASE (default https://api.github.com),
#   MAZ_EMBED_MODEL (default snowflake-arctic-embed2),
#   MAZ_CHAT_MODEL  (default qwen2.5:7b),
#   MAZ_NO_MODIFY_PATH=1, MAZ_NO_OLLAMA=1

set -eu

REPO="mo-cmyk/mazarbul"
PREFIX="${MAZ_INSTALL_PREFIX:-$HOME/.local}"
VERSION="${MAZ_VERSION:-}"
CHANNEL="${MAZ_CHANNEL:-stable}"
API_BASE="${MAZ_API_BASE:-https://api.github.com}"
# Auth for private (pre-launch) releases. Env only; MAZ_INSTALL_TOKEN wins over
# the more ambient GITHUB_TOKEN. Empty -> anonymous public download path.
TOKEN="${MAZ_INSTALL_TOKEN:-${GITHUB_TOKEN:-}}"
EMBED_MODEL="${MAZ_EMBED_MODEL:-snowflake-arctic-embed2}"
CHAT_MODEL="${MAZ_CHAT_MODEL:-qwen2.5:7b}"
MODIFY_PATH=yes
WITH_OLLAMA=yes
RUN_INIT=no
[ -n "${MAZ_NO_MODIFY_PATH:-}" ] && MODIFY_PATH=no
[ -n "${MAZ_NO_OLLAMA:-}" ] && WITH_OLLAMA=no

say() { echo "==> $*"; }
warn() { echo "install.sh: $*" >&2; }
die() { echo "install.sh: $*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# GET a GitHub REST endpoint as JSON, authenticated when $TOKEN is set. Two
# branches (not a built header list) so the anonymous path never carries an
# Authorization header.
gh_api_get() {
	if [ -n "$TOKEN" ]; then
		curl -fsSL \
			-H "Accept: application/vnd.github+json" \
			-H "X-GitHub-Api-Version: 2022-11-28" \
			-H "Authorization: Bearer $TOKEN" \
			"$1"
	else
		curl -fsSL -H "Accept: application/vnd.github+json" "$1"
	fi
}

# Plain download to a file (public release CDN or a MAZ_BASE_URL mirror/file://).
fetch_url() {
	curl -fsSL -o "$2" "$1" || die "download failed: $1"
}

# Download a private release asset by numeric id via the API octet-stream
# endpoint. The API answers 302 to the asset's signed storage URL; -L follows it
# and DROPS the Authorization header on the cross-host hop (curl >= 7.83), so the
# token never reaches storage. Do NOT add --location-trusted — that would forward
# the token to the storage host.
fetch_asset() {
	curl -fsSL -o "$2" \
		-H "Accept: application/octet-stream" \
		-H "Authorization: Bearer $TOKEN" \
		"${API_BASE}/repos/${REPO}/releases/assets/$1" \
		|| die "asset download failed (id $1)"
}

# Extract the numeric asset id for a given asset name from a release JSON blob.
# Prefer jq when available — it matches on the structured field and is immune to
# field ordering. Without jq, fall back to a token-stream pairing: grep -o emits
# each "id"/"name" token on its own line in document order (robust to compact
# JSON) and awk prints the most-recent id when the matching name appears (GitHub
# lists an asset's "id" before its "name"). Portable across macOS BSD + GNU.
# Either way the caller digit-guards the result, so a parse slip fails loud.
asset_id() {
	# $1 = release json, $2 = target asset name
	if have jq; then
		printf '%s' "$1" | jq -r --arg n "$2" '.assets[] | select(.name == $n) | .id' 2>/dev/null
		return
	fi
	printf '%s' "$1" \
		| grep -Eo '"(id|name)"[[:space:]]*:[[:space:]]*"?[^",}]*"?' \
		| awk -v want="$2" '
			/"id"/ { id = $0; sub(/.*:[[:space:]]*/, "", id); gsub(/[^0-9]/, "", id) }
			/"name"/ {
				n = $0
				sub(/.*:[[:space:]]*"?/, "", n)
				sub(/".*/, "", n)
				if (n == want) { print id; exit }
			}
		'
}

while [ $# -gt 0 ]; do
	case "$1" in
		--prefix) PREFIX="${2:?--prefix needs a value}"; shift 2 ;;
		--version) VERSION="${2:?--version needs a value}"; shift 2 ;;
		--channel) CHANNEL="${2:?--channel needs a value}"; shift 2 ;;
		--no-modify-path) MODIFY_PATH=no; shift ;;
		--no-ollama) WITH_OLLAMA=no; shift ;;
		--init) RUN_INIT=yes; shift ;;
		# Print the leading comment block (line 2 to the first non-comment line)
		# so the help text can't drift out of sync with a fixed line range.
		-h|--help) awk 'NR==1{next} /^#/{print; next} {exit}' "$0"; exit 0 ;;
		*) die "unknown argument: $1 (try --help)" ;;
	esac
done

case "$CHANNEL" in
	stable|nightly) ;;
	*) die "unknown channel: $CHANNEL (stable|nightly)" ;;
esac

have curl || die "curl is required"
have tar || die "tar is required"

# --- detect platform -------------------------------------------------------
os=$(uname -s)
arch=$(uname -m)
case "$os" in
	Darwin) os=darwin ;;
	Linux)  os=linux ;;
	*) die "unsupported OS: $os (prebuilt binaries: darwin, linux)" ;;
esac
case "$arch" in
	x86_64|amd64)  arch=amd64 ;;
	arm64|aarch64) arch=arm64 ;;
	*) die "unsupported architecture: $arch" ;;
esac

# --- resolve version -------------------------------------------------------
# A pinned MAZ_VERSION / --version always wins, regardless of channel.
if [ -z "$VERSION" ]; then
	case "$CHANNEL" in
		stable)
			# Highest published non-prerelease, non-draft release.
			api="${API_BASE}/repos/${REPO}/releases/latest" ;;
		nightly)
			# TODO(#258): the nightly tag scheme is not finalized. As a forward-
			# compatible placeholder, take the newest release entry (index 0,
			# which includes pre-releases) so a pre-release-tagged nightly
			# resolves without changing this script.
			api="${API_BASE}/repos/${REPO}/releases?per_page=1" ;;
	esac
	# Capture the API response in its own step so curl's exit code is checked
	# directly. Piping straight into grep/sed would mask a failed request (401
	# bad token, 403 no access, network error): sed exits 0 on empty input, so
	# the auth failure would surface as the misleading "no release found" below.
	release_meta=$(gh_api_get "$api") \
		|| die "could not query the ${CHANNEL} release (token valid? repo access?)"
	VERSION=$(printf '%s' "$release_meta" \
		| grep '"tag_name"' | head -1 | sed -E 's/.*"tag_name" *: *"([^"]+)".*/\1/')
	[ -n "$VERSION" ] || die "no ${CHANNEL} release found (set MAZ_VERSION to install a specific tag)"
fi

ARCHIVE="mazarbul_${VERSION}_${os}_${arch}.tar.gz"

say "${REPO} ${VERSION} (${CHANNEL}, ${os}/${arch}) -> ${PREFIX}"

# --- download archive + checksums ------------------------------------------
# Every branch lands an identical $tmp/$ARCHIVE + $tmp/checksums.txt, then falls
# into the unchanged sha256 verify below — the gate doesn't care how they arrived.
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
if [ -n "${MAZ_BASE_URL:-}" ]; then
	# Explicit base (local file:// testing or a mirror): no auth, no API.
	fetch_url "${MAZ_BASE_URL}/${ARCHIVE}"    "$tmp/$ARCHIVE"
	fetch_url "${MAZ_BASE_URL}/checksums.txt" "$tmp/checksums.txt"
elif [ -n "$TOKEN" ]; then
	# Private-repo path: the browser releases/download URL won't serve private
	# assets, so resolve each asset's numeric id via the REST API and pull it
	# from the asset endpoint (302 -> signed storage URL).
	release_json=$(gh_api_get "${API_BASE}/repos/${REPO}/releases/tags/${VERSION}") \
		|| die "could not query release ${VERSION} (token valid? repo access?)"
	archive_id=$(asset_id "$release_json" "$ARCHIVE")
	sums_id=$(asset_id "$release_json" "checksums.txt")
	case "$archive_id" in ''|*[!0-9]*) die "release ${VERSION} has no asset ${ARCHIVE}" ;; esac
	case "$sums_id"    in ''|*[!0-9]*) die "release ${VERSION} has no asset checksums.txt" ;; esac
	fetch_asset "$archive_id" "$tmp/$ARCHIVE"
	fetch_asset "$sums_id"    "$tmp/checksums.txt"
else
	# Public path: browser release CDN download (no token, no API rate limit).
	base="https://github.com/${REPO}/releases/download/${VERSION}"
	fetch_url "${base}/${ARCHIVE}"    "$tmp/$ARCHIVE"
	fetch_url "${base}/checksums.txt" "$tmp/checksums.txt"
fi

# --- verify sha256 BEFORE placing anything ---------------------------------
# Literal field match, not a grep pattern: ${ARCHIVE} contains dots that a
# regex would treat as "any char", so a tampered checksums.txt could map a
# look-alike name to the wrong hash before the equality check below. Compare
# the filename column ($2) exactly.
expected=$(awk -v f="$ARCHIVE" '$2==f {print $1; exit}' "$tmp/checksums.txt")
[ -n "$expected" ] || die "no checksum entry for ${ARCHIVE} in checksums.txt"
if have sha256sum; then
	actual=$(sha256sum "$tmp/$ARCHIVE" | awk '{print $1}')
elif have shasum; then
	actual=$(shasum -a 256 "$tmp/$ARCHIVE" | awk '{print $1}')
else
	die "need sha256sum or shasum to verify the download"
fi
[ "$expected" = "$actual" ] || die "checksum mismatch for ${ARCHIVE} (expected ${expected}, got ${actual})"
say "checksum verified"

# --- extract + install -----------------------------------------------------
# The archive wraps everything in a single top-level dir (mazarbul_<ver>_<os>_<arch>/)
# so a manual `tar xf` doesn't pollute the cwd; --strip-components=1 drops it
# so files land directly in $tmp/x. Supported by both GNU tar and macOS bsdtar.
mkdir -p "$tmp/x"
tar -xzf "$tmp/$ARCHIVE" -C "$tmp/x" --strip-components=1
[ -f "$tmp/x/maz" ] || die "archive did not contain maz"
[ -d "$tmp/x/lib" ] || die "archive did not contain the engine lib"
# Guard the `cp -P .../lib/*` glob below: an empty lib/ would otherwise fail
# with an opaque shell glob error instead of this clear, consistent message.
ls "$tmp/x/lib/"liblbug.* >/dev/null 2>&1 || die "archive lib/ contains no liblbug.* engine library"

mkdir -p "$PREFIX/bin" "$PREFIX/lib"
cp "$tmp/x/maz" "$PREFIX/bin/maz"
chmod 0755 "$PREFIX/bin/maz"
# Preserve symlinks (liblbug.dylib -> liblbug.0.dylib -> liblbug.0.N.dylib).
cp -P "$tmp/x/lib/"* "$PREFIX/lib/"

# man pages (best-effort: bundled in release archives, absent in older builds).
# man auto-derives <prefix>/share/man from a <prefix>/bin that is on PATH, so
# `man maz` works once the PATH step below runs — no MANPATH edit needed.
if [ -d "$tmp/x/man/man1" ]; then
	mkdir -p "$PREFIX/share/man/man1"
	cp "$tmp/x/man/man1/"*.1 "$PREFIX/share/man/man1/" 2>/dev/null || true
	say "installed man pages -> $PREFIX/share/man/man1 (try: man maz)"
fi

"$PREFIX/bin/maz" --version >/dev/null 2>&1 \
	|| die "installed maz failed to run (engine lib not resolved?)"
say "installed $("$PREFIX/bin/maz" --version)"

# --- PATH ------------------------------------------------------------------
configure_path() {
	bindir="$PREFIX/bin"
	case ":$PATH:" in
		*":$bindir:"*) say "$bindir is already on your PATH"; return 0 ;;
	esac
	if [ "$MODIFY_PATH" = no ]; then
		say "add maz to PATH:  export PATH=\"$bindir:\$PATH\""
		return 0
	fi
	marker="# added by maz install.sh"
	line="export PATH=\"$bindir:\$PATH\""
	rc=""
	case "$(basename "${SHELL:-/bin/sh}")" in
		zsh)  rc="${ZDOTDIR:-$HOME}/.zshrc" ;;
		bash) if [ "$os" = darwin ]; then rc="$HOME/.bash_profile"; else rc="$HOME/.bashrc"; fi ;;
		fish) rc="$HOME/.config/fish/conf.d/maz.fish"; line="fish_add_path \"$bindir\""; mkdir -p "$(dirname "$rc")" ;;
		*)    rc="$HOME/.profile" ;;
	esac
	if [ -f "$rc" ] && grep -qF "$marker" "$rc" 2>/dev/null; then
		say "PATH already configured in $rc"
	else
		printf '\n%s\n%s\n' "$marker" "$line" >> "$rc"
		say "added $bindir to PATH in $rc"
	fi
	say "open a new terminal (or 'exec \$SHELL'); for this session:  export PATH=\"$bindir:\$PATH\""
}
configure_path

# --- provider (Ollama) -----------------------------------------------------
ensure_models() {
	have ollama || return 0
	for m in "$EMBED_MODEL" "$CHAT_MODEL"; do
		# Literal match, not regex: model names carry metacharacters (the '.'
		# in qwen2.5:7b would match any char). ollama stores an untagged name
		# as "<name>:latest", so accept an exact name or a "<name>:" tag prefix.
		if ollama list 2>/dev/null | awk -v m="$m" '$1==m || index($1, m":")==1 {found=1} END{exit !found}'; then
			say "model present: $m"
		else
			say "pulling model: $m (this can be a large download)"
			ollama pull "$m" || warn "failed to pull $m — pull it later with: ollama pull $m"
		fi
	done
}

ensure_ollama() {
	if [ "$WITH_OLLAMA" = no ]; then
		say "skipping Ollama (--no-ollama). maz needs a provider: install Ollama, or run 'maz init --provider anthropic --api-key ...'"
		return 0
	fi
	if have ollama; then
		say "Ollama already installed ($(ollama --version 2>/dev/null | head -1))"
	else
		say "installing Ollama (maz's default local model provider)"
		if have brew; then
			brew install ollama || { warn "brew install ollama failed — see https://ollama.com/download"; return 0; }
		elif [ "$os" = linux ]; then
			# Ollama's only official Linux install is their hosted script. We
			# cannot checksum-verify it the way we verify the maz archive, so
			# the trust boundary is explicit: this pipes a third-party script
			# to sh. Skip with --no-ollama (install Ollama yourself), or use a
			# remote provider via 'maz init --provider anthropic --api-key ...'.
			warn "Ollama has no checksummed release; its official Linux install"
			warn "runs an unverified third-party script:"
			warn "    curl -fsSL https://ollama.com/install.sh | sh"
			warn "proceeding (skip next time with --no-ollama / MAZ_NO_OLLAMA=1)."
			curl -fsSL https://ollama.com/install.sh | sh \
				|| { warn "Ollama install failed — install it from https://ollama.com/download"; return 0; }
		else
			warn "install Ollama from https://ollama.com/download (or 'brew install ollama'), then re-run with --init"
			return 0
		fi
	fi
	ensure_models
}
ensure_ollama

# --- optional: finish setup ------------------------------------------------
if [ "$RUN_INIT" = yes ]; then
	say "running maz init"
	if [ "$WITH_OLLAMA" = no ]; then
		"$PREFIX/bin/maz" init --yes || warn "maz init did not complete"
	else
		"$PREFIX/bin/maz" init --yes --provider ollama --chat-model "$CHAT_MODEL" \
			--embed ollama --embed-model "$EMBED_MODEL" || warn "maz init did not complete"
	fi
else
	echo ""
	say "Next: finish setup with"
	if [ "$WITH_OLLAMA" = no ]; then
		echo "    maz init"
	else
		echo "    maz init --yes --provider ollama --chat-model $CHAT_MODEL --embed ollama"
	fi
	echo "    (or re-run this installer with --init)"
fi
