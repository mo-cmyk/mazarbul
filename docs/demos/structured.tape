Output "docs/demos/structured.gif"

Set Shell "bash"
Set FontSize 18
Set Width 1500
Set Height 980
Set Padding 24
Set Margin 30
Set MarginFill "#0e0e11"
Set WindowBar "Colorful"
Set WindowBarSize 56
Set BorderRadius 12
Set Theme { "name": "mazdark", "background": "#16161a", "foreground": "#e6e6ea", "black": "#101014", "red": "#f87171", "green": "#22c55e", "yellow": "#fbbf24", "blue": "#60a5fa", "magenta": "#c084fc", "cyan": "#22d3ee", "white": "#e6e6ea", "brightBlack": "#6e6e78", "brightRed": "#f87171", "brightGreen": "#22c55e", "brightYellow": "#fbbf24", "brightBlue": "#60a5fa", "brightMagenta": "#c084fc", "brightCyan": "#22d3ee", "brightWhite": "#fafafb", "cursor": "#e6e6ea", "selection": "#2c2c34" }
Set TypingSpeed 26ms
Set WaitTimeout 90s
Set WaitPattern /^❯ ?$/

Env PS1 "\[\e[38;2;249;115;22m\]❯\[\e[0m\] "
Env CLICOLOR_FORCE "1"
Env COLORTERM "truecolor"

Type "maz schema kinds add artifact --display-name 'Artifact' --description 'An object of power'"
Sleep 400ms
Enter
Wait
Sleep 1500ms

Type "maz schema predicates add bears --description 'carries an object of power'"
Sleep 400ms
Enter
Wait
Sleep 1500ms

Type `maz entity create artifact "The One Ring" --attributes '{"forged_in":"Mount Doom","power":"invisibility"}'`
Sleep 400ms
Enter
Wait
Sleep 1500ms

Type `maz entity create person "Frodo Baggins" --attributes '{"race":"hobbit"}'`
Sleep 400ms
Enter
Wait
Sleep 1500ms

Type `maz relate "Frodo Baggins" bears "The One Ring"`
Sleep 400ms
Enter
Wait
Sleep 1500ms

Type `maz get "The One Ring"`
Sleep 400ms
Enter
Wait
Sleep 2500ms

Type `maz neighbors "Frodo Baggins"`
Sleep 400ms
Enter
Wait
Sleep 2500ms

Type `maz recall "who carries the ring"`
Sleep 400ms
Enter
Wait
Sleep 3s
