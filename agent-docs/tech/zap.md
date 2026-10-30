# Zap

Mazarbul uses `go.uber.org/zap` for structured logging via the `internal/logging/` wrapper.

## Get the logger from context

Always pass `context.Context` and pull the logger out:

```go
import "github.com/mo-cmyk/mazarbul/internal/logging"

func DoThing(ctx context.Context, id string) error {
    log := logging.FromContext(ctx)
    log.Info("starting thing",
        zap.String("id", id),
    )
    // ...
}
```

`logging.FromContext` always returns a non-nil logger. If the context does not carry one, it returns the zap nop logger so callers never panic.

## Preferred field types

Use the strongly-typed field constructors. The encoder is faster and the output is consistent.

| Use | When |
|---|---|
| `zap.String(key, val)` | Any string scalar |
| `zap.Int(key, val)`, `zap.Int64(key, val)` | Integer counts, IDs |
| `zap.Bool(key, val)` | Flags |
| `zap.Duration(key, d)` | Timings (uses `d.String()` formatting) |
| `zap.Time(key, t)` | Absolute times (RFC 3339 in production output) |
| `zap.Error(err)` | Errors (named `error` field, with stack trace if enabled) |
| `zap.Stringer(key, s)` | Any `fmt.Stringer` |
| `zap.Any(key, v)` | Last resort; slow, escapes through reflection |

Field keys are lowercase snake_case (`event_id`, `request_id`, `data_dir`). Match the AIP camelCase convention for JSON payloads but **not** for log field names; log keys follow Unix conventions.

## Levels

| Level | When |
|---|---|
| `Debug` | High-volume tracing useful only when debugging. Off in production. |
| `Info` | Significant events: command started, ingestion completed, sync pushed N events. |
| `Warn` | Anomalies that did not break anything: retried request, fell back to default. |
| `Error` | Failures the caller should know about. Always with `zap.Error(err)`. |

**Never** use `Fatal` or `Panic`. The logger should not exit the process; let `main` decide via the return code from `cli.Execute()`.

## Log message style

- Capitalize the first word of a log message: `"Inscribed event"`, `"Failed to acquire lock"`.
- Lowercase error strings returned from functions (`fmt.Errorf("could not parse: %w", err)`).
- Keep messages short and descriptive. The structured fields carry the detail.
- **No em-dashes** in log messages.
- **No themed language** in log messages. Log messages are mechanical.

## What never to log

Passwords, API keys, tokens, encryption keys, raw Watchword words, raw event payloads containing sensitive content. Field names should make clear what is being logged; if a value is sensitive, omit it or hash it.

## File destinations

Foreground `maz` commands stream JSON Lines to **stderr** by default, in line with 12-factor principle XI (apps emit log streams, not files; the user pipes them).

The daemon is the explicit exception: once detached from the controlling terminal its stderr is unreachable, so it writes to a rotated file. `logging.Config` carries optional file-sink fields:

```go
logger, err := logging.NewLogger(logging.Config{
    Level:      logging.LevelInfo,
    Format:     logging.FormatJSON,
    FilePath:   config.DaemonLogFile(), // ~/.cache/mazarbul/daemon.log
    MaxSizeMB:  10,                     // default; rotate at this size
    MaxBackups: 5,                      // default; historical files retained
})
```

When `FilePath` is empty the logger writes to `Config.Output` (or `os.Stderr` if unset). When `FilePath` is set, the parent directory is created with mode `0700` and the file with mode `0600`; rotation runs through [`lumberjack.v2`](https://pkg.go.dev/gopkg.in/natefinch/lumberjack.v2). No time-based rotation in M1.

Read the live file with `maz daemon logs --follow`.
