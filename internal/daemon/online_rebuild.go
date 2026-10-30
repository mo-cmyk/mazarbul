package daemon

// online_rebuild.go is the job-side of an online rebuild: it builds a fresh
// projection in a shadow directory WHILE the daemon keeps serving, then hands
// the result to the supervisor's reload loop to swap in (see performSwap in
// supervisor.go).
//
// The shadow build replays the LIVE event log read-only. FileEventLog.Read
// walks the JSONL directly and never opens the bbolt sidecar (only Append/At
// do), so this second reader does not contend for the exclusive index.bolt OS
// lock the live daemon holds — the replay runs concurrently with live writes.
// New events that land mid-build are caught by a final delta-replay during the
// swap, when the MCP surface is briefly down and the JSONL is therefore frozen.
//
// The JSONL is never mutated in any path. On failure the shadow directory is
// removed and the original projection keeps serving untouched.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// rebuildShadowPrefix names the per-rebuild shadow directory under the data
// dir (".rebuild-<jobUID>"). The hygiene pass removes stale ones on boot.
const rebuildShadowPrefix = ".rebuild-"

// runOnlineRebuild builds a fresh projection in a shadow dir from the live
// event log, then enqueues the swap on the supervisor reload loop and waits
// for its outcome. It returns nil only when the swap committed; on any error
// the shadow dir is cleaned up and the original projection is left serving.
func runOnlineRebuild(ctx context.Context, c *rebuildCoordinator, wired *Wired, dataDir string, kind rebuildKind, strict bool, jobUID string) error {
	if wired == nil || wired.Orchestrator == nil || wired.Orchestrator.Log == nil {
		return errors.New("online rebuild: event log unavailable")
	}

	shadowDir := filepath.Join(dataDir, rebuildShadowPrefix+jobUID)
	if err := os.MkdirAll(shadowDir, 0o750); err != nil {
		return fmt.Errorf("online rebuild: mkdir shadow: %w", err)
	}
	// Clean the shadow dir on every exit path. On the swap-success path
	// performSwap renames index.db + graph.ladybug OUT to the canonical
	// location, so this only removes leftovers (or the whole partial build on
	// the failure path).
	defer func() {
		if rerr := os.RemoveAll(shadowDir); rerr != nil {
			c.logger.Warn("online rebuild: shadow cleanup", zap.String("dir", shadowDir), zap.Error(rerr))
		}
	}()

	shadowDisp, err := buildShadowProjection(ctx, c, wired, shadowDir, strict)
	if err != nil {
		return err
	}
	return enqueueAndAwaitSwap(ctx, c, kind, jobUID, shadowDir, shadowDisp)
}

// buildShadowProjection opens a fresh dispatcher in shadowDir and replays the
// live event log into it (reads only — never touches the live bbolt sidecar),
// then drains the background embeds so the swapped-in vector index is complete.
// On any error it closes the shadow dispatcher; on success it returns the OPEN
// dispatcher, whose ownership transfers to the swap path.
func buildShadowProjection(ctx context.Context, c *rebuildCoordinator, wired *Wired, shadowDir string, strict bool) (*dispatcher.Dispatcher, error) {
	shadowDisp, err := dispatcher.Open(ctx, dispatcher.Config{
		DataDir:  shadowDir,
		Embedder: wired.Orchestrator.Embedder, // same embedder; nil under embed=none
		Logger:   c.logger,
	})
	if err != nil {
		return nil, fmt.Errorf("online rebuild: open shadow dispatcher: %w", err)
	}

	// The coordinator is the JobReporter so replay progress drives the status.
	opts := []kernel.RebuildOption{kernel.WithJobReporter(c)}
	if strict {
		opts = append(opts, kernel.WithStrict())
	}
	if _, rerr := wired.Orchestrator.Log.Rebuild(ctx, shadowDisp, opts...); rerr != nil {
		closeShadowDisp(c, shadowDisp)
		return nil, fmt.Errorf("online rebuild: shadow replay: %w", rerr)
	}

	// Drain background embeds (mirrors the CLI rebuild's drain-before-close).
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reloadDeadline)
	derr := shadowDisp.WaitForBackgroundVec(drainCtx)
	cancel()
	if derr != nil {
		closeShadowDisp(c, shadowDisp)
		return nil, fmt.Errorf("online rebuild: shadow embed drain: %w", derr)
	}
	return shadowDisp, nil
}

// enqueueAndAwaitSwap hands the shadow build to the supervisor reload loop and
// waits for the swap outcome. The send is unbuffered: a successful send
// transfers ownership of shadowDisp to performSwap (which closes it). If ctx is
// cancelled before the handoff, the shadow dispatcher is closed here.
func enqueueAndAwaitSwap(ctx context.Context, c *rebuildCoordinator, kind rebuildKind, jobUID, shadowDir string, shadowDisp *dispatcher.Dispatcher) error {
	c.setState(jobUID, rebuildStateSwapping, "swapping in rebuilt projection")
	done := make(chan error, 1)
	req := reloadRequest{kind: kind, jobUID: jobUID, shadowDir: shadowDir, shadowDisp: shadowDisp, done: done}
	select {
	case c.reloadCh <- req:
		// ownership transferred to performSwap
	case <-ctx.Done():
		closeShadowDisp(c, shadowDisp)
		return fmt.Errorf("online rebuild: enqueue swap: %w", ctx.Err())
	}

	select {
	case serr := <-done:
		if serr != nil {
			return fmt.Errorf("online rebuild: swap: %w", serr)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("online rebuild: await swap: %w", ctx.Err())
	}
}

// closeShadowDisp closes a shadow dispatcher, logging any error. Shared by the
// build and enqueue error paths.
func closeShadowDisp(c *rebuildCoordinator, d *dispatcher.Dispatcher) {
	if cerr := d.Close(); cerr != nil {
		c.logger.Warn("online rebuild: close shadow dispatcher", zap.Error(cerr))
	}
}

// rebuildCommitSentinel is written under the data dir for the duration of the
// file rename. Its presence on boot means a swap was interrupted between the
// two artifact renames — the projection pair may be inconsistent (the JSONL is
// always intact), so the daemon warns and the operator can `maz rebuild`.
const rebuildCommitSentinel = ".rebuild-commit"

// performSwap runs the quiesce + swap on the supervisor's reload goroutine
// after runOnlineRebuild hands over the shadow build. It ALWAYS closes the live
// wiring (the loop re-wires from scratch regardless of outcome) and signals the
// job result on req.done. The shadow dispatcher is closed exactly once here.
func performSwap(ctx context.Context, opts *SupervisorOptions, srv mcp.Server, wired *Wired, req reloadRequest) {
	shadowClosed := false
	closeShadow := func() {
		if shadowClosed {
			return
		}
		shadowClosed = true
		if cerr := req.shadowDisp.Close(); cerr != nil {
			opts.Logger.Warn("swap: close shadow dispatcher", zap.Error(cerr))
		}
	}

	err := func() error {
		// 1. Drain in-flight tool calls + close the listeners. After a clean
		//    shutdown no MCP/admin write can land, so the JSONL is frozen for
		//    the final delta-replay.
		if serr := shutdownServerErr(ctx, opts, srv); serr != nil {
			closeShadow()
			return fmt.Errorf("swap: server shutdown: %w", serr)
		}
		// 2. Final delta-replay: apply any events that landed during the shadow
		//    build into the shadow (the live log is still open). HasEvent skips
		//    everything already applied, so only the (seconds-of-writes) delta
		//    lands.
		if rerr := finalDeltaReplay(ctx, wired, req.shadowDisp); rerr != nil {
			closeShadow()
			return rerr
		}
		// 3. Flush + unlock the shadow projection before renaming its files.
		closeShadow()
		// 4. Release the live store so its index.db + graph.ladybug are unlocked.
		closeWiredGrace(ctx, opts, wired)
		// 5. Rename the rebuilt artifacts into the canonical location.
		return swapStorePaths(opts.DataDir, req.shadowDir, opts.Logger)
	}()

	// Defensive: ensure the shadow + live wiring are closed on every path
	// (both are idempotent), so the loop always re-wires from a clean slate.
	closeShadow()
	closeWiredGrace(ctx, opts, wired)
	req.done <- err
}

// shutdownServerErr drains and stops srv, returning the error for the swap path
// to act on (an undrained shutdown means in-flight writes may not have landed,
// so the swap aborts rather than risk losing them from the rebuilt projection).
func shutdownServerErr(ctx context.Context, opts *SupervisorOptions, srv mcp.Server) error {
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace(opts.ShutdownTimeout))
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// finalDeltaReplay re-applies the live log into the shadow projection to catch
// events appended during the (online) shadow build. Reuses the live,
// fully-configured event log; it is still open at this point (the wiring is
// closed only after this returns).
func finalDeltaReplay(ctx context.Context, wired *Wired, shadow shadowStore) error {
	if wired == nil || wired.Orchestrator == nil || wired.Orchestrator.Log == nil {
		return nil
	}
	deltaCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reloadDeadline)
	defer cancel()
	if _, rerr := wired.Orchestrator.Log.Rebuild(deltaCtx, shadow); rerr != nil {
		return fmt.Errorf("swap: final delta replay: %w", rerr)
	}
	if derr := shadow.WaitForBackgroundVec(deltaCtx); derr != nil {
		return fmt.Errorf("swap: final embed drain: %w", derr)
	}
	return nil
}

// swapStorePaths replaces the live index.db + graph.ladybug with the shadow's,
// bracketed by a commit sentinel so an interrupted swap is detectable on boot.
// The JSONL is never touched.
func swapStorePaths(dataDir, shadowDir string, logger *zap.Logger) error {
	sentinel := filepath.Join(dataDir, rebuildCommitSentinel)
	if werr := os.WriteFile(sentinel, []byte(shadowDir+"\n"), 0o600); werr != nil {
		return fmt.Errorf("write commit sentinel: %w", werr)
	}
	for _, base := range []string{"index.db", "graph.ladybug"} {
		if rerr := replaceStoreArtifact(dataDir, shadowDir, base); rerr != nil {
			return rerr
		}
	}
	if rerr := os.Remove(sentinel); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		logger.Warn("swap: remove commit sentinel", zap.Error(rerr))
	}
	return nil
}

// replaceStoreArtifact removes the live <base>* artifacts and moves the
// shadow's <base>* into place. The glob covers SQLite's -wal/-shm sidecars and
// works whether graph.ladybug is a file or a directory. Os.Rename within the
// same data dir is atomic per artifact.
func replaceStoreArtifact(dataDir, shadowDir, base string) error {
	liveMatches, err := filepath.Glob(filepath.Join(dataDir, base+"*"))
	if err != nil {
		return fmt.Errorf("glob live %s: %w", base, err)
	}
	for _, p := range liveMatches {
		if rerr := os.RemoveAll(p); rerr != nil {
			return fmt.Errorf("remove live %s: %w", filepath.Base(p), rerr)
		}
	}
	shadowMatches, err := filepath.Glob(filepath.Join(shadowDir, base+"*"))
	if err != nil {
		return fmt.Errorf("glob shadow %s: %w", base, err)
	}
	for _, src := range shadowMatches {
		dst := filepath.Join(dataDir, filepath.Base(src))
		if rerr := os.Rename(src, dst); rerr != nil {
			return fmt.Errorf("rename %s: %w", filepath.Base(src), rerr)
		}
	}
	return nil
}
