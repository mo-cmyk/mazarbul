Output "scratchpad/tui-captures/recall-strip.gif"

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

# Off-camera: start an isolated daemon and export its data dir.
# The demo binary must be built with -tags=sqlite_fts5 and placed in /tmp/maz-demo.
# See scripts/tui-smoke.sh for the full isolation setup.
Hide
Env MAZ_DATA_DIR "/tmp/maz-recall-demo"
Env CLICOLOR_FORCE "1"
Env COLORTERM "truecolor"
Type "mkdir -p /tmp/maz-recall-demo"
Enter
Sleep 500ms
Show

# ── faceted recall filter strip demo ─────────────────────────────────────────

# Open recall TUI with an initial query
Type "maz --data-dir /tmp/maz-recall-demo recall --tui erebor"
Sleep 400ms
Enter
Sleep 3s

# Tab focuses the filter strip
Tab
Sleep 800ms

# Space toggles the first Kind pill (e.g. "person")
Space
Sleep 600ms

# → moves to the next pill; Space toggles it
Right
Sleep 400ms
Space
Sleep 600ms

# Tab to "Scope" bank with ←/→, toggle "team"
Tab
Sleep 400ms
Right
Sleep 400ms
Right
Sleep 400ms
Space
Sleep 800ms

# r resets all strip filters
Type "r"
Sleep 600ms

# Tab back to picker, scroll results
Tab
Sleep 400ms
Type "j"
Sleep 300ms
Type "j"
Sleep 500ms

# Enter opens detail for the selected hit
Enter
Sleep 1500ms

# q quits
Type "q"
Sleep 500ms
