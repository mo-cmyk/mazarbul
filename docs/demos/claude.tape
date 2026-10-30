Output "docs/demos/claude.gif"

Set Shell "bash"
Set FontSize 18
Set Width 1500
Set Height 1040
Set Padding 24
Set Margin 30
Set MarginFill "#0e0e11"
Set WindowBar "Colorful"
Set WindowBarSize 56
Set BorderRadius 12
Set Theme { "name": "mazdark", "background": "#16161a", "foreground": "#e6e6ea", "black": "#101014", "red": "#f87171", "green": "#22c55e", "yellow": "#fbbf24", "blue": "#60a5fa", "magenta": "#c084fc", "cyan": "#22d3ee", "white": "#e6e6ea", "brightBlack": "#6e6e78", "brightRed": "#f87171", "brightGreen": "#22c55e", "brightYellow": "#fbbf24", "brightBlue": "#60a5fa", "brightMagenta": "#c084fc", "brightCyan": "#22d3ee", "brightWhite": "#fafafb", "cursor": "#e6e6ea", "selection": "#2c2c34" }
Set TypingSpeed 24ms
Set WaitTimeout 180s
Set WaitPattern /^❯ ?$/

Env PS1 "\[\e[38;2;249;115;22m\]❯\[\e[0m\] "
Env CLICOLOR_FORCE "1"
Env COLORTERM "truecolor"
# Off-camera isolation: pin the seeded graph for the MCP server the agent spawns.
Env MAZ_DATA_DIR "/tmp/maz-claude"

# After `maz connect claude-code` (see the on-ramp), the MCP server and its
# auto-approve are already wired into Claude Code. We reproduce that wired state
# off-camera with an alias so the demo never touches your real ~/.claude.json:
# the isolated MCP config points Claude at maz, and the settings file pre-approves
# the maz tools so there are no permission prompts. On camera, you just run claude.
#
# --setting-sources user loads only user-level settings, so this checkout's own
# project hooks (a golangci-lint Stop gate, a dev-session banner, prompt routing)
# never fire in the demo. That keeps the session a clean agent-with-memory, not a
# Mazarbul contributor shell, and it still uses the real keychain login for auth.
Hide
Type `alias claude='claude --setting-sources user --mcp-config /tmp/maz-mcp.json --settings /tmp/maz-allow.json --strict-mcp-config'`
Enter
Type "clear"
Enter
Show
Sleep 800ms

# Headless: one question, one answer. No tool list, no flags: Claude decides on
# its own to consult maz, traverses the graph, and answers in a single line.
Type `claude -p "Who bears the One Ring, and who guides the bearer? Answer in one line."`
Sleep 800ms
Enter
Wait
Sleep 3s

# Interactive: the same memory, live in the Claude Code session. It reaches into
# the graph to connect the threat to the betrayal, not just retrieve text.
Type "claude"
Sleep 600ms
Enter
Sleep 11s

Type "Who threatens the Ring-bearer, and who has betrayed the Fellowship? Keep it tight."
Sleep 800ms
Enter
Sleep 95s
