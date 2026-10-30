# Profiling

Mazarbul is single-process and CPU/memory bound on three hot paths: ingestion, vector search, and graph traversal. Profile before optimising. Measure, don't guess.

## Expose pprof

The `maz` binary serves pprof behind a `--debug-addr` flag. Pass an empty value (default) to keep the endpoint closed.

```bash
./bin/maz --debug-addr=localhost:6060
```

Then open `http://localhost:6060/debug/pprof/` for the index, or hit the standard subpaths directly (`/debug/pprof/goroutine`, `/debug/pprof/heap`, `/debug/pprof/profile`, `/debug/pprof/allocs`).

The endpoint is never exposed in MCP daemon mode unless `--debug-addr` is explicitly set. Production users should keep it closed or bind to `localhost` only.

## CPU profile

```bash
mise run pprof:cpu
```

Captures a 30-second CPU profile from a running `maz` (which must have been started with `--debug-addr=localhost:6060`) and opens the flame graph in your browser via `go tool pprof -http=:8080`.

## Heap profile

```bash
mise run pprof:mem
```

Captures the current heap and opens it in the browser. Use this when memory usage drifts.

## Allocation profile

For benchmarks, drive the allocation profile through `go test`:

```bash
go test -benchmem -cpuprofile=cpu.prof ./internal/kernel/...
go tool pprof -http=:8080 cpu.prof
```

`-benchmem` adds per-op allocation counts to the bench output; `-cpuprofile` writes the CPU profile to disk.

## Benchmarks

Bench files live next to the code they exercise (`kernel_bench_test.go` next to `kernel.go`). Always call `b.ReportAllocs()` so the bench output includes B/op and allocs/op.

```go
func BenchmarkKernel_Append(b *testing.B) {
    b.ReportAllocs()
    k := setupKernel(b)
    event := newTestEvent()
    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        if err := k.Append(b.Context(), event); err != nil {
            b.Fatal(err)
        }
    }
}
```

Run via:

```bash
mise run bench
```

## Memory limit

`GOMEMLIMIT=1GiB` is pre-set in `.mise.toml`. The Go runtime uses this as a soft target; the GC will work harder to stay under it.

For explicit per-process control in `cmd/maz/main.go`:

```go
import "runtime/debug"

func main() {
    if v := os.Getenv("MAZ_GOMEMLIMIT"); v != "" {
        // ... parse and set
        debug.SetMemoryLimit(parsed)
    }
    cli.Execute()
}
```

For most scenarios, the env var is enough; only reach for `SetMemoryLimit` if you need to compute the limit at runtime (e.g., from a container memory cgroup).

## When to profile

- **Before optimising.** Measure first. A profile pointing at the hot 1% changes which optimisation is worth doing.
- **When ingestion regresses.** If ingest throughput drops between releases, the CPU profile + a regression test pin down the culprit.
- **When MCP latency creeps.** If `recall` response times grow, profile the request path under load.
- **Don't profile prematurely.** Pre-M1 code is not worth profiling; correctness comes first.
