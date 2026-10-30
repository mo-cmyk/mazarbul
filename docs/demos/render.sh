#!/usr/bin/env bash
# Render the Mazarbul demo GIFs with VHS.
#
# Requirements: vhs, ffmpeg (with ffprobe), ttyd, jq on PATH; a built ./bin/maz; python3 with PIL
# (for the title overlay); and Ollama serving qwen2.5:7b + snowflake-arctic-embed2.
# The ttyd shell inherits this script's environment, so PATH / MAZ_DATA_DIR / PS1
# flow into every tape (the tapes themselves stay path-free and portable).
#
# Three isolation guards matter:
#   1. HOME is redirected to a throwaway dir so `maz connect claude-code` never
#      touches your real ~/.claude.json or ~/.claude/settings.json. (The claude
#      scene flips HOME back to the real one for the `claude` CLI's API login,
#      but reaches maz over an MCP config file, never mutating ~/.claude.json.)
#   2. MAZ_SECRETS_DIR routes identity/signing keys to a file-backed store in a
#      throwaway dir instead of the shared OS keychain. Without it, writes under
#      a fresh HOME block forever on interactive `security -i`, and a stray
#      `maz init` could clobber your real Watchword in the login keychain.
#   3. Only one maz process may hold a data dir at a time (LadybugDB is a single
#      opener under --daemon off), so we kill stragglers before every render and
#      seed strictly sequentially.
#
# Usage: docs/demos/render.sh [hero|structured|git|obsidian|claude|all]
# ("all" excludes claude, which needs the `claude` CLI + an API login.)
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
ROOT="$PWD"  # absolute repo root; used to scope process cleanup to THIS worktree

export PATH="$PWD/bin:/opt/homebrew/bin:$PATH"
# Orange prompt (maz Accent #f97316) for any off-camera shell; the on-camera
# prompt is set per tape via `Env PS1` with bash non-printing markers.
export PS1=$'\033[38;2;249;115;22m❯\033[0m '
export CLICOLOR_FORCE=1
export COLORTERM=truecolor
# No interactive pager on the raw `git log` scenes, or VHS waits on less forever.
export GIT_PAGER=cat
export PAGER=cat
# Only surface errors on camera. The CLI's default warn level lets benign
# warn-accept logs (e.g. an extractor proposing an unregistered entity kind) leak
# to stderr mid-demo; gating to error keeps the recorded terminal clean. Command
# output (tables, recall hits) goes to stdout and is unaffected.
export MAZ_LOG_LEVEL=error

# Pick a python3 that can import PIL (needed for the title overlay). Search every
# python on PATH, not just the first, since a Homebrew python3 without Pillow can
# shadow a PIL-capable one. If none is found the title is skipped gracefully.
PY=""
while IFS= read -r _cand; do
	[ -n "$_cand" ] || continue
	if "$_cand" -c "import PIL" >/dev/null 2>&1; then PY="$_cand"; break; fi
done < <(which -a python3 python 2>/dev/null)
[ -n "$PY" ] || PY="$(command -v python3 || echo python3)"

# The claude scene needs the real HOME for the `claude` CLI's API login; capture
# it before the isolation override below.
REAL_HOME="$HOME"

# Isolate HOME so connect writes to a throwaway config, and give git an identity
# for the sync-push commits.
export HOME=/tmp/maz-demo-home
rm -rf "$HOME"; mkdir -p "$HOME"
git config --global user.name "Demo"
git config --global user.email "demo@example.com"
git config --global init.defaultBranch main

# Route signing keys to a file store, not the shared login keychain (see header).
export MAZ_SECRETS_DIR=/tmp/maz-demo-secrets
rm -rf "$MAZ_SECRETS_DIR"; mkdir -p "$MAZ_SECRETS_DIR"

command -v maz >/dev/null || { echo "maz not on PATH; run 'mise run build' first" >&2; exit 1; }

INIT=(maz init --yes --provider ollama --embed ollama --daemon off)

# Kill only stragglers from THIS worktree (daemons / `mcp serve` carry the
# absolute "$ROOT/bin/maz" in their cmdline), so a render never disturbs a maz
# daemon running from another worktree in a multi-checkout dev environment.
kill_maz() { pkill -9 -f "$ROOT/bin/maz" 2>/dev/null || true; sleep 1; }
warm() { ollama run qwen2.5:7b "ok" >/dev/null 2>&1 || true; }

# wait_ollama blocks until /api/tags answers within 1s AND lists the chat model,
# matching exactly what `maz init`'s 1s provider probe sees. Without this, a slow
# probe under load makes the wizard fall back to the Anthropic path (an extra
# provider screen plus a blocking API-key prompt), desyncing the recorded Enters.
wait_ollama() {
	local i
	for i in $(seq 1 20); do
		if curl -fsS --max-time 1 http://localhost:11434/api/tags 2>/dev/null | grep -q '"qwen2.5:7b"'; then
			return 0
		fi
		sleep 1
	done
	echo "wait_ollama: ollama not reporting qwen2.5:7b within 1s; wizard may desync" >&2
}

# polish_gif adds a centered macOS-style window title (VHS draws the traffic-light
# dots but has no title text, and this ffmpeg build lacks drawtext, so we overlay
# a PIL-rendered transparent PNG) and, when asked, collapses the single longest
# frozen stretch (the ~30-100s in-process LLM extraction, which renders as a
# static screen) down to ~2s. Both happen in one re-encode to avoid double
# palette quantization. Args: <gif> <title> [trim]
polish_gif() {
	local gif="$1" title="$2" trim="${3:-}"
	local base raw titlepng W H
	base="$(basename "$gif" .gif)"
	raw="/tmp/${base}-raw.gif"; titlepng="/tmp/${base}-title.png"
	cp "$gif" "$raw"
	W=$(ffprobe -v error -select_streams v:0 -show_entries stream=width -of csv=p=0 "$raw")
	H=$(ffprobe -v error -select_streams v:0 -show_entries stream=height -of csv=p=0 "$raw")
	# Window bar centre is Margin(30) + WindowBarSize(56)/2 = 58, constant across tapes.
	if ! "$PY" docs/demos/titlebar.py --width "$W" --height "$H" --bar-y 58 --title "$title" --out "$titlepng" 2>/dev/null; then
		echo "polish_gif: titlebar render failed; leaving title off $gif" >&2
		titlepng=""
	fi
	# Optional dead-air trim: find the longest frozen [start,end] window, keep
	# everything before start+2s and from end on. Dynamic, so it survives
	# run-to-run latency variation.
	local selexpr=""
	if [ "$trim" = "trim" ]; then
		# Collapse only the single longest dead-air stretch (>5s): the in-process
		# LLM extraction during `maz remember`. Short reading dwells stay intact.
		local win
		win=$(ffmpeg -i "$raw" -vf "freezedetect=n=-55dB:d=3" -map 0:v -f null - 2>&1 |
			awk '/freeze_start/{s=$NF} /freeze_duration/{d=$NF} /freeze_end/{e=$NF; if(d>maxd){maxd=d; bs=s; be=e}} END{if(maxd>5) printf "%.2f %.2f", bs+2.0, be}')
		if [ -n "$win" ]; then
			local keepuntil keepfrom
			keepuntil=${win% *}; keepfrom=${win#* }
			selexpr="select='lt(t,${keepuntil})+gte(t,${keepfrom})',setpts=N/FRAME_RATE/TB,"
			echo "polish_gif: collapsing freeze, keep t<${keepuntil}s + t>=${keepfrom}s" >&2
		fi
	elif [ "$trim" = "trimall" ]; then
		# Collapse EVERY freeze (>=3s) down to ~1.5s. The Claude session has many
		# think/tool pauses plus an idle tail; single-freeze trimming would leave
		# most of them. A freeze that runs to EOF has no freeze_end, so drop it
		# open-ended with gte().
		selexpr=$(ffmpeg -i "$raw" -vf "freezedetect=n=-55dB:d=3" -map 0:v -f null - 2>&1 |
			awk '
				/freeze_start/{s=$NF; pending=1}
				/freeze_duration/{d=$NF}
				/freeze_end/{e=$NF; pending=0; a=s+1.5; if(e>a){ if(x!="")x=x"+"; x=x"between(t\\,"a"\\,"e")"} }
				END{ if(pending){ a=s+1.5; if(x!="")x=x"+"; x=x"gte(t\\,"a")" }
				     if(x!="") printf "select=\x27not(%s)\x27,setpts=N/FRAME_RATE/TB,", x }')
		[ -n "$selexpr" ] && echo "polish_gif: collapsing all freezes (trimall)" >&2
	fi
	if [ -n "$titlepng" ]; then
		ffmpeg -i "$raw" -i "$titlepng" -filter_complex \
			"[0:v]${selexpr}fps=13[v];[v][1:v]overlay=0:0[ov];[ov]split[a][b];[a]palettegen=max_colors=144[p];[b][p]paletteuse=dither=none[o]" \
			-map "[o]" -loop 0 "$gif" -y >/dev/null 2>&1
	else
		ffmpeg -i "$raw" -filter_complex \
			"[0:v]${selexpr}fps=13,split[a][b];[a]palettegen=max_colors=144[p];[b][p]paletteuse=dither=none[o]" \
			-map "[o]" -loop 0 "$gif" -y >/dev/null 2>&1
	fi
}

# render_hero shows the fast on-ramp: one non-interactive `maz init --yes`, then
# the wow loop, remember a sentence, see the typed graph maz extracted from it
# (neighbors), recall it back. Runs as a subshell so the HOME override does not
# leak into later scenes. The init data-dir validator rejects /tmp, so the hero
# uses a non-/tmp throwaway HOME and its default data dir lands under it. The
# tape's Hide block runs seed_hero.sh to normalize the (genuinely extracted but
# run-to-run variable) edges. remember blocks ~30-100s on in-process extraction;
# trim collapses that dead air.
render_hero() (
	kill_maz; warm; wait_ollama
	export HOME="$REAL_HOME/.mazarbul-home"
	rm -rf "$HOME"; mkdir -p "$HOME"
	# Clean the throwaway home however this subshell exits: under set -e a vhs or
	# polish_gif failure would otherwise skip a trailing rm and leave debris inside
	# the real home (this is the one scene whose HOME is not a /tmp dir).
	trap 'rm -rf "$HOME"' EXIT
	git config --global user.name "Demo"
	git config --global user.email "demo@example.com"
	git config --global init.defaultBranch main
	unset MAZ_DATA_DIR
	vhs docs/demos/hero.tape
	polish_gif docs/demos/hero.gif "maz" trim
	kill_maz
)

render_structured() {
	kill_maz
	export MAZ_DATA_DIR=/tmp/maz-structured
	rm -rf "$MAZ_DATA_DIR"
	"${INIT[@]}" >/dev/null 2>&1
	vhs docs/demos/structured.tape
	polish_gif docs/demos/structured.gif "maz · schema" trim
}

# render_git seeds a three-push Lord of the Rings narrative so the semantic
# diff/blame read richly on camera: additions (green) across three pushes, a
# forgotten relation (red, Saruman's betrayal corrects the record), and a fact
# with history (Frodo's road, the Shire -> Rivendell, visible in blame). All to
# a PLAINTEXT remote, which is what makes git diff/blame/log work; encryption is
# never required to sync.
render_git() {
	kill_maz
	export MAZ_DATA_DIR=/tmp/maz-git
	rm -rf "$MAZ_DATA_DIR"
	"${INIT[@]}" >/dev/null 2>&1
	maz schema kinds add artifact --display-name "Artifact" --description "An object of power" >/dev/null 2>&1 || true
	maz schema predicates add bears --description "carries an object of power" >/dev/null 2>&1 || true

	# Push 1: the Ring-bearer sets out. relate wires entities by name.
	maz entity create person "Frodo Baggins" --attributes '{"race":"hobbit","role":"Ring-bearer"}' >/dev/null
	maz entity create person "Gandalf" --attributes '{"race":"wizard","title":"the Grey"}' >/dev/null
	maz entity create artifact "The One Ring" --attributes '{"forged_in":"Mount Doom"}' >/dev/null
	maz relate "Frodo Baggins" bears "The One Ring" >/dev/null
	maz relate "Frodo Baggins" mentored_by "Gandalf" >/dev/null
	maz fact set "Frodo Baggins" location "the Shire" >/dev/null 2>&1 || true
	maz sync push >/dev/null 2>&1

	# Push 2: the Fellowship forms, and one relation we will later retract.
	maz entity create organization "The Fellowship of the Ring" --attributes '{"purpose":"destroy the One Ring"}' >/dev/null
	maz entity create person "Aragorn" --attributes '{"race":"man","title":"heir of Isildur"}' >/dev/null
	maz entity create person "Samwise Gamgee" --attributes '{"race":"hobbit"}' >/dev/null
	maz entity create person "Saruman" --attributes '{"race":"wizard","title":"the White"}' >/dev/null
	maz relate "Frodo Baggins" member_of "The Fellowship of the Ring" >/dev/null
	maz relate "Aragorn" member_of "The Fellowship of the Ring" >/dev/null
	maz relate "Samwise Gamgee" sworn_to "Frodo Baggins" >/dev/null
	# Saruman the White was trusted, so he was first recorded among the Fellowship.
	# The uid is only needed to forget the edge below (guarded), so a missing jq or
	# an empty pipeline must not abort the scene under `set -euo pipefail`.
	wrong=$(maz relate "Saruman" member_of "The Fellowship of the Ring" --json 2>/dev/null | jq -r '.uid // .name' | sed 's#relations/##') || wrong=""
	maz fact set "Frodo Baggins" location "Rivendell" >/dev/null 2>&1 || true   # supersedes the Shire
	maz sync push >/dev/null 2>&1

	# Push 3: correct the record. Forget Saruman's false allegiance (red) and
	# record the betrayal that came to light (green).
	[ -n "${wrong:-}" ] && maz forget "relations/${wrong}" >/dev/null 2>&1 || true
	maz entity create person "Sauron" --attributes '{"race":"maia","title":"the Dark Lord"}' >/dev/null
	maz relate "Saruman" serves "Sauron" >/dev/null 2>&1 || true
	maz sync push >/dev/null 2>&1

	vhs docs/demos/git.tape
	polish_gif docs/demos/git.gif "maz · sync" trim
}

# render_obsidian seeds a small interlinked markdown vault, then the tape shows
# import, full-text/semantic search across the bodies, and wikilinks resolved
# into the entity graph (doc links + neighbors).
render_obsidian() {
	kill_maz
	export MAZ_DATA_DIR=/tmp/maz-obsidian
	rm -rf "$MAZ_DATA_DIR" /tmp/notes-demo
	"${INIT[@]}" >/dev/null 2>&1
	mkdir -p /tmp/notes-demo
	printf '# The Fellowship of the Ring\n\nNine companions set out from [[Rivendell]] to destroy the [[One Ring]] in [[Mordor]].\nGuided by [[Gandalf]], guarded by [[Aragorn]].\n' >/tmp/notes-demo/fellowship.md
	printf '# Frodo Baggins\n\nThe Ring-bearer of the [[Fellowship of the Ring]]. A hobbit of the [[Shire]], he carries the [[One Ring]] toward [[Mordor]].\n' >/tmp/notes-demo/frodo.md
	printf '# Gandalf\n\nWizard who guides the [[Fellowship of the Ring]]. Mentor to [[Frodo Baggins]], and wary of [[Saruman]].\n' >/tmp/notes-demo/gandalf.md
	vhs docs/demos/obsidian.tape
	polish_gif docs/demos/obsidian.gif "maz · docs" trim
}

# render_claude records two real Claude Code sessions against the same maz memory:
# a headless `claude -p` one-shot, then an interactive session. Both reach maz
# over MCP and reason across the typed LOTR graph (bearer -> guide; threat ->
# betrayal) with no tool list and no on-camera flags. It needs the `claude` CLI
# and a working API login, so it is intentionally NOT part of `all` (it cannot run
# headless in CI). Run it explicitly: docs/demos/render.sh claude.
#
# Two config files give the on-camera `claude` its wired-up feel without touching
# your real config (the tape aliases them in off-camera):
#   maz-mcp.json   - points Claude at this checkout's maz over stdio MCP. No data
#                    dir here; the server inherits MAZ_DATA_DIR from the ttyd shell
#                    (set in the tape), so the config reads like a real user's.
#   maz-allow.json - pre-approves the maz tool namespace (mcp__mazarbul__*) so the
#                    session runs with no permission prompts, exactly as it would
#                    after `maz connect claude-code` wired auto-approve.
render_claude() {
	kill_maz
	# claude reads its API login from the real home, not the throwaway one.
	export HOME="$REAL_HOME"
	export MAZ_DATA_DIR=/tmp/maz-claude
	# Re-seed fresh every run so the graph always matches the current seed script.
	rm -rf "$MAZ_DATA_DIR"
	"${INIT[@]}" >/dev/null 2>&1
	bash docs/demos/seed_graph.sh >/dev/null 2>&1
	cat > /tmp/maz-mcp.json <<-EOF
		{"mcpServers":{"mazarbul":{"command":"$PWD/bin/maz","args":["mcp","serve"]}}}
	EOF
	cat > /tmp/maz-allow.json <<-'EOF'
		{"permissions":{"allow":["mcp__mazarbul__*"]}}
	EOF
	vhs docs/demos/claude.tape
	polish_gif docs/demos/claude.gif "claude" trimall
}

case "${1:-all}" in
	hero) render_hero ;;
	structured) render_structured ;;
	git) render_git ;;
	obsidian) render_obsidian ;;
	claude) render_claude ;;
	all) render_hero; render_structured; render_git; render_obsidian ;;
	*) echo "usage: $0 [hero|structured|git|obsidian|claude|all]" >&2; exit 2 ;;
esac
echo "done"
