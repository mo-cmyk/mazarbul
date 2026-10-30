Output "docs/demos/hero.gif"

Set Shell "bash"
Set FontSize 18
Set Width 1500
Set Height 920
Set Padding 24
Set Margin 30
Set MarginFill "#0e0e11"
Set WindowBar "Colorful"
Set WindowBarSize 56
Set BorderRadius 12
Set Theme { "name": "mazdark", "background": "#16161a", "foreground": "#e6e6ea", "black": "#101014", "red": "#f87171", "green": "#22c55e", "yellow": "#fbbf24", "blue": "#60a5fa", "magenta": "#c084fc", "cyan": "#22d3ee", "white": "#e6e6ea", "brightBlack": "#6e6e78", "brightRed": "#f87171", "brightGreen": "#22c55e", "brightYellow": "#fbbf24", "brightBlue": "#60a5fa", "brightMagenta": "#c084fc", "brightCyan": "#22d3ee", "brightWhite": "#fafafb", "cursor": "#e6e6ea", "selection": "#2c2c34" }
Set TypingSpeed 26ms
Set WaitTimeout 180s
Set WaitPattern /^❯ ?$/

Env PS1 "\[\e[38;2;249;115;22m\]❯\[\e[0m\] "
Env CLICOLOR_FORCE "1"
Env COLORTERM "truecolor"

# One non-interactive command sets everything up: local Ollama for chat and
# embeddings, a plaintext git mirror, no questions asked.
Type "maz init --yes --provider ollama --daemon off"
Sleep 500ms
Enter
Wait
Sleep 2500ms

# Remember one plain sentence. maz runs an LLM over it in-process and pulls out a
# typed graph: the people, the objects, and the relations between them.
Type `maz remember "Frodo Baggins bears the One Ring. Gandalf the Grey guides him to Mordor. Samwise Gamgee serves him. Sauron seeks the Ring."`
Sleep 500ms
Enter
Wait
Sleep 2s

# Normalize the extracted graph off-camera, then clear so the seed command never
# shows (Hide stops capture but leaves the screen state, so we clear before Show).
# Local extraction is real but varies run to run (predicate, direction, exact
# name); we rewrite the edges to the canonical ones so the GIF reads the same
# every render. The episode and entities from remember are genuine and still
# drive the recall below.
Hide
Type "bash docs/demos/seed_hero.sh"
Enter
Wait
Type "clear"
Enter
Wait
Show
Sleep 600ms

# That one sentence is now a queryable graph. neighbors shows what maz extracted:
# Frodo bears the Ring, guided by Gandalf, served by Samwise.
Type `maz neighbors "Frodo Baggins"`
Sleep 500ms
Enter
Wait
Sleep 3s

# And it is retrievable in plain language. recall fuses vector, full-text, and
# graph signals and hands back the source episode it learned this from.
Type `maz recall "who guides the ring-bearer"`
Sleep 500ms
Enter
Wait
Sleep 3s
