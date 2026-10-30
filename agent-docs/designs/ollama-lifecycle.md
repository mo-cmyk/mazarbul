# Ollama lifecycle

## Why this exists

Mazarbul ships an in-process `OllamaSupervisor` so the init wizard can
start `ollama serve` on the user's behalf and have it respawn after
crashes (issue #114, commit `6518679`). In production we discovered the
naive design freezes macOS at daemon start (issue: "starting the daemon
on mac kills my Mac").

This doc captures the failure mode and the three layered fixes (F1, F2,
F3) so the next person reading the supervisor code understands why it
looks the way it does.

## The failure mode

The original `spawnOllamaServe` executed:

```go
cmd := exec.CommandContext(ctx, "ollama", "serve")
cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
// cmd.Env unset — Ollama inherits the user's environment with no caps
```

Ollama's defaults on macOS are aggressive: `OLLAMA_MAX_LOADED_MODELS=3`
and `OLLAMA_NUM_PARALLEL=4` are typical. On a 16 GB Mac a single
unsupervised `ollama serve` can:

1. Load the embed model (e.g. `bge-m3`, ~1.5 GB) on first `recall`.
2. Load the chat model (e.g. `llama3.1:8b`, ~5 GB) on first generation.
3. Keep both resident under `OLLAMA_KEEP_ALIVE=5m` (the default).

That's >6.5 GB of unified memory pinned in the Metal allocator. Combined
with the OS, browser, IDE, and a chrome of helper processes, macOS hits
the memory compressor + swap thrashing path. The Mac becomes
unresponsive even though no individual process is doing anything wrong.

A second, more acute failure mode: port collision. If Homebrew launchd
has `ollama` running on `:11434` and the supervisor adopts a stale PID
file (or sees no PID file and tries to spawn), the new `ollama serve`
binds-fails 5× in 30s. Each short-lived attempt allocates Metal
buffers before crashing, which alone can saturate GPU memory on
M-series Macs.

## What the daemon should do

1. Open the four-layer store, register MCP tools, bind transports.
2. Probe Ollama with a bounded HTTP timeout; degrade to write-only on miss.
3. Block on signals.

It should **not** be in charge of the lifecycle of a heavyweight peer
service like `ollama serve`. That belongs to launchd on macOS.

## The three-layer fix

### F1 — Constrain the child (commit `<this PR>` `spawnOllamaServe`)

`buildOllamaServeCmd` now sets `cmd.Env` to:

```
OLLAMA_MAX_LOADED_MODELS=1
OLLAMA_NUM_PARALLEL=1
OLLAMA_KEEP_ALIVE=30s
OLLAMA_FLASH_ATTENTION=1
```

Appended **after** `os.Environ()` so Go's exec (which honours the last
occurrence of each key) applies these regardless of the user's shell
exports. Every other user variable (`OLLAMA_HOST`, `OLLAMA_MODELS`, …)
still flows through.

This alone removes the macOS-freeze mode. The other layers are
defence-in-depth.

### F2 — Health-probe, port-check, identity verification

Three new behaviours in `OllamaSupervisor`:

- **Pre-spawn HTTP probe** (`attemptRestart`). Before invoking
  `SpawnOllama`, the supervisor GETs `OllamaURL+/api/version`. If
  something already responds the supervisor enters **delegated mode**:
  `delegated=true`, `pid=0`, `state=Healthy`. No spawn. This eliminates
  the port-collision burst above.
- **Delegated-mode polling** (`tick`). In delegated mode the supervisor
  cannot use `processAlive` (no PID) — it relies on HTTPProbe. Failures
  are debounced via `wedgedFailureThreshold` so a single dropped
  packet does not cause unnecessary takeover; after three consecutive
  silent ticks the supervisor transitions to `RestartPending` and
  spawns its own child.
- **PID identity verification** (`Start`). The sidecar PID file records
  only a number. Before adopting it, the supervisor calls `PidExePath`
  (`ps -o comm=` on macOS, `/proc/<pid>/exe` on Linux) and confirms the
  binary is `ollama`. A wrong-process match is treated as a stale PID
  file rather than adopted; the original PR consciously deferred this
  check, citing PID reuse as "the same risk carried by any PID-based
  supervisor on POSIX" — true, but cheap to close.

### F3 — Move the lifecycle to launchd

`maz ollama install/uninstall/status` (macOS-only) installs
`~/Library/LaunchAgents/io.mazarbul.ollama.plist`. The plist carries
the same F1 caps in its `EnvironmentVariables` dict, plus
`KeepAlive { SuccessfulExit = false }` and `ThrottleInterval = 10` so
launchd does the restart-on-crash work the in-process supervisor was
shipped to provide.

There is **no config-schema change**. `providers.ollama.managed = true`
still works after install: the in-process supervisor's pre-spawn HTTP
probe (F2) detects launchd's Ollama and enters delegated mode
automatically. Existing configs continue to function; the only
difference is who restarts Ollama after a crash (launchd instead of
the Mazarbul daemon).

Linux's `systemd --user` analogue is a follow-up.

## Operator playbook

| Symptom | Action |
|---|---|
| Mac freezes when daemon starts | Confirm F1 caps in process env: `ps eww -p $(cat ~/.mazarbul/ollama-serve.pid) \| grep OLLAMA` |
| `maz doctor` says `ollama.supervisor.state = healthy` but recall fails | Ollama is wedged; F2 wedged-detection should kick in within ~15s. If it doesn't, file a bug. |
| Two `ollama serve` processes running | F2's pre-spawn probe must have raced a concurrent spawn. Kill both, rerun `maz daemon start` — the second start adopts the survivor via delegated mode. |
| Want launchd-managed Ollama | `maz ollama install`; in-process supervisor will auto-delegate on next daemon start. |
| Roll back to in-process | `maz ollama uninstall` and restart the daemon. |

## What's still deferred (follow-up tickets)

- Wedged-detection on **owned** children (currently only delegated
  mode debounces HTTP failures; an owned child that hangs is detected
  only by signal-0).
- Wizard step in `maz init` that offers launchd install on macOS.
- A `managed = "launchd"` config value that explicitly disables the
  in-process supervisor (the current auto-delegate path works, but an
  explicit value is clearer in `maz doctor` output).
- Linux `systemd --user` equivalent of F3.
- Log rotation for `ollama-serve.log` / `ollama-launchd.log`.

## Files

- `internal/daemon/ollama_supervisor.go` — supervisor state machine + F1 env caps + F2 logic.
- `internal/daemon/ollama_http_probe.go` — F2 HTTPProbe + URL resolver + isOllamaExe.
- `internal/daemon/ollama_pidexe_{darwin,linux,other}.go` — F2 platform-specific identity check.
- `internal/daemon/launchd_{darwin,other}.go` — F3 plist + launchctl wrappers.
- `internal/cli/ollama.go` — F3 `maz ollama install/uninstall/status`.
