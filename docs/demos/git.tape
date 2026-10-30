Output "docs/demos/git.gif"

Set Shell "bash"
Set FontSize 18
Set Width 1500
Set Height 1000
Set Padding 24
Set Margin 30
Set MarginFill "#0e0e11"
Set WindowBar "Colorful"
Set WindowBarSize 56
Set BorderRadius 12
Set Theme { "name": "mazdark", "background": "#16161a", "foreground": "#e6e6ea", "black": "#101014", "red": "#f87171", "green": "#22c55e", "yellow": "#fbbf24", "blue": "#60a5fa", "magenta": "#c084fc", "cyan": "#22d3ee", "white": "#e6e6ea", "brightBlack": "#6e6e78", "brightRed": "#f87171", "brightGreen": "#22c55e", "brightYellow": "#fbbf24", "brightBlue": "#60a5fa", "brightMagenta": "#c084fc", "brightCyan": "#22d3ee", "brightWhite": "#fafafb", "cursor": "#e6e6ea", "selection": "#2c2c34" }
Set TypingSpeed 26ms
Set WaitTimeout 60s
Set WaitPattern /^❯ ?$/

Env PS1 "\[\e[38;2;249;115;22m\]❯\[\e[0m\] "
Env CLICOLOR_FORCE "1"
Env COLORTERM "truecolor"

Type "maz sync log"
Sleep 400ms
Enter
Wait
Sleep 2500ms

Type "maz diff HEAD~2..HEAD"
Sleep 400ms
Enter
Wait
Sleep 3500ms

Type `maz blame "Frodo Baggins"`
Sleep 400ms
Enter
Wait
Sleep 3500ms
