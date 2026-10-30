Output "scratchpad/tui-captures/watch-sources.gif"

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

# --demo uses FakeEventStreamer — no live daemon needed.  The sources pane is
# populated by FakeEventStreamer.Sources() which returns the deterministic
# four-row mix (live · stale · down · not-connected) defined in feeds.go.
Hide
Env CLICOLOR_FORCE "1"
Env COLORTERM "truecolor"
Show

# ── maz watch sources pane (2×3 grid) demo ───────────────────────────────────

# Open watch with synthetic data
Type "maz watch --demo"
Sleep 400ms
Enter
Sleep 3s

# The 2×3 grid shows panes 1-4 (events, extraction, embedding, sync)
# Press 1-4 to cycle through existing panes first.
Type "1"
Sleep 700ms
Type "2"
Sleep 700ms
Type "3"
Sleep 700ms
Type "4"
Sleep 700ms

# Press 5 to focus the new sources pane
Type "5"
Sleep 1500ms

# Pause event stream to hold the current view
Type "p"
Sleep 800ms

# Tab cycles to the next pane
Tab
Sleep 700ms
Tab
Sleep 700ms

# ? opens the keyboard reference overlay
Type "?"
Sleep 1200ms
Type "?"
Sleep 600ms

# r reloads all panes from the streamer
Type "r"
Sleep 1000ms

# q quits
Type "q"
Sleep 500ms
