# Exit Codes

`maz` uses a small, stable set of exit codes. Scripts can branch on these.

| Code | Meaning            | When used                                                          |
|------|--------------------|--------------------------------------------------------------------|
| 0    | Success            | Command completed without error.                                   |
| 1    | Expected failure   | Bad input, resource not found, or validation error — the command ran but the request could not be satisfied. |
| 2    | Unexpected failure | I/O error, daemon failure, or a bug — something the caller cannot fix by changing the request. |
| 130  | User cancelled     | Ctrl-C; the process received SIGINT and stopped early.             |

The read surface (`show`, `get`, `neighbors`, `relations`, `entities`, `blame`,
`diff`, `count`, `traverse`) follows this split deliberately: a missing UID or an
unknown entity name exits **1** (the thing you asked for isn't there), while a
daemon or storage error exits **2** (the lookup itself failed). Distinguishing the
two lets a script tell "no such entity" apart from "the query broke."

## Scripting

Every `maz` command that fails prints its error to **stderr**. The exact format
depends on where the error originates:

- **MCP-layer errors** (storage, not-found, permission) include an `mcp:` segment
  because the error travels through the MCP tool layer:
  `command: mcp: E_CODE: message`
  (for example `get: mcp: E_NOT_FOUND: entity 01HZ... not found`)

  Commands on the read surface that follow this form: `get`, `neighbors`,
  `relations`, `entities`, `traverse`.

- **Bare CLI errors** do not carry an `mcp:` segment or an E_CODE — they are
  formatted directly by the CLI handler:
  `command: message`

  On the read surface, `show` and `blame` use this form for not-found errors:
  `show: no entity named "x"` and `blame: no entity named "x"`. Scripts that
  parse the `mcp: E_CODE:` prefix will not match these messages; test the exit
  code (1) instead.

  CLI validation errors (bad flag values, out-of-range limits) also use this
  bare form:
  (for example `recall: --limit 999 out of range: must be between 1 and 50`)

Stdout carries only the command's machine-readable result, so a script can
capture stdout and inspect the exit code independently:

```sh
if out=$(maz get 01HZ... --output json 2>/dev/null); then
  echo "$out" | jq .
else
  case $? in
    1) echo "not found / bad input" ;;
    2) echo "lookup failed (daemon or storage)" ;;
    130) echo "cancelled" ;;
  esac
fi
```

Errors are **not** echoed as a JSON envelope on stdout — they always go to
stderr, regardless of `--output`. To check an exit code without seeing the
message, redirect stderr: `maz get bad-uid 2>/dev/null; echo $?`.
