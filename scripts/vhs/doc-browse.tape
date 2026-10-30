Output "scratchpad/tui-captures/doc-browse.gif"

Set Shell "bash"
Set FontSize 14
Set Width 1200
Set Height 800
Set Padding 20
Set Margin 24
Set MarginFill "#0e0e11"
Set WindowBar "Colorful"
Set WindowBarSize 48
Set BorderRadius 10
Set Theme { "name": "mazdark", "background": "#16161a", "foreground": "#e6e6ea", "black": "#101014", "red": "#f87171", "green": "#22c55e", "yellow": "#fbbf24", "blue": "#60a5fa", "magenta": "#c084fc", "cyan": "#22d3ee", "white": "#e6e6ea", "brightBlack": "#6e6e78", "brightRed": "#f87171", "brightGreen": "#22c55e", "brightYellow": "#fbbf24", "brightBlue": "#60a5fa", "brightMagenta": "#c084fc", "brightCyan": "#22d3ee", "brightWhite": "#fafafb", "cursor": "#e6e6ea", "selection": "#2c2c34" }
Set TypingSpeed 40ms
Set WaitTimeout 30s

# Off-camera: set up isolated data dir with seeded vault documents.
# Prerequisites:
#   mkdir -p /tmp/maz-doc-demo/vault
#   cp scripts/tui-smoke.sh fixture docs → /tmp/maz-doc-demo/vault/*.md
# See scripts/tui-smoke.sh seed_fixtures() for the exact files.
Hide
Env MAZ_DATA_DIR "/tmp/maz-doc-demo"
Env CLICOLOR_FORCE "1"
Env COLORTERM "truecolor"
Type "mkdir -p /tmp/maz-doc-demo/vault"
Enter
Sleep 300ms
# Write a fixture doc so the browser has something to show even without seeding.
Type `printf '# Erebor migration kickoff\n\nSam owns the RFC rollout.\n\nSee [[Ring Mk 4000 / RFC 1149]] for spec.\n' > /tmp/maz-doc-demo/vault/meeting-2026-06-01.md`
Enter
Sleep 300ms
Show

# TODO(confirm after lane lands): `maz doc browse` is the new command added by
# Lane BC. If the command is not yet in the binary this tape will show an error;
# update the command path once the lane merges.

# ── vault split-pane browser demo ─────────────────────────────────────────────

# Open doc browse
Type "maz --data-dir /tmp/maz-doc-demo doc browse"
Sleep 400ms
Enter
Sleep 3s

# j/k moves selection down/up in the list
Type "j"
Sleep 500ms
Type "j"
Sleep 500ms
Type "k"
Sleep 400ms

# Enter opens the detail pane
Enter
Sleep 1500ms

# L follows the first resolved wikilink into its entity detail
Type "L"
Sleep 1200ms

# Esc returns to the list
Escape
Sleep 600ms

# / opens the filter input; type to narrow the list
Type "/"
Sleep 300ms
Type "erebor"
Sleep 800ms
Escape
Sleep 500ms

# e would open $EDITOR (omitted — no TTY in VHS)
# n would open the new-document prompt (omitted for same reason)

# q quits
Type "q"
Sleep 500ms
