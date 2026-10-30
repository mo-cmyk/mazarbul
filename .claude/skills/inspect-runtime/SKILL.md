---
name: inspect-runtime
description: Triage a runtime issue in Mazarbul end-to-end. USE WHEN user reports an error, asks "why is X slow/failing", "investigate this incident", or pastes a panic trace. Walks logs -> traces -> pprof -> storage. NOT FOR code-quality issues (use /verify, /simplify) or version-mismatch checks (use /inspect-deploy).
allowed-tools:
  - Bash(./bin/maz *)
  - Bash(go tool pprof *)
  - Bash(curl *)
  - Bash(tail *)
  - Bash(grep *)
  - Bash(jq *)
---

# Runtime triage

When something is broken, you don't yet know which layer. This skill walks the canonical layers in order so you don't skip a step.

## Triage tree (run top to bottom; stop when you have a root cause)

### 1. Logs

Mazarbul uses zap structured logging to stderr by default:

```bash
./bin/maz mcp serve 2>&1 | tail -100 | jq -r 'select(.level == "error") | .msg + "\t" + .error'
```

Watch for:
- Repeated error messages (a single bug, many invocations).
- Errors mentioning the kernel writer or scope enforcement (invariant violations).
- "context canceled" patterns (timeout cascades).

### 2. Traces (OpenTelemetry, when configured)

When OTEL is wired (M2 work), traces go to the configured collector. Stub for now.

### 3. Profile

If a code path is slow:

```bash
# Start the daemon with the pprof endpoint exposed
./bin/maz mcp serve --debug-addr=localhost:6060 &

# Capture
mise run pprof:cpu      # 30s flame graph
mise run pprof:mem      # heap profile
```

For a memory leak suspicion:

```bash
go tool pprof -http=:8080 http://localhost:6060/debug/pprof/heap
# Compare two captures 60 seconds apart.
```

### 4. Storage

If the symptom looks like data corruption or stale data, drop down to `/inspect-db`.

## Decision table

| Symptom | First check | If clean, then |
|---|---|---|
| MCP tool returns wrong shape | Logs at error level | Tool schema vs handler code |
| Recall is slow | Profile (mise run pprof:cpu) | Vector and FTS index counts |
| Daemon crashes | Logs around the crash | Run with `-race` to catch data races |
| Event log growing fast | Source-connector counts | Connector config (over-eager poll?) |
| Plugin won't load | Daemon stderr at startup | Plugin manifest validation |

## Common gotchas

- **Logs are JSON, not text**, pipe through `jq` for usable output.
- **pprof endpoint** must be enabled via `--debug-addr`; not on by default in prod-mode.
- **Race detector** has runtime overhead; only run with `-race` when diagnosing concurrency bugs.

## What this skill is NOT

- A replacement for actual telemetry. When OTEL is wired, traces give you more than logs alone.
- Storage-layer inspection, that's `/inspect-db`.
