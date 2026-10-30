package doctor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/kernelboot"
)

// subsystemIdentity groups checks over the self/principal identity graph.
const subsystemIdentity = "identity"

// selfAnchorUID is the local-only symbolic self anchor seeded by migration
// 0012. Mirrors kernelboot.seededSelfUID (kept private there); redeclared here
// so the doctor layer does not import the kernelboot internals just for the
// sentinel string.
const selfAnchorUID = "self"

// selfLinkPredicate is the predicate that links the self anchor to the bound
// principal. Mirrors kernelboot.selfLinkPredicate (owl:sameAs semantics).
const selfLinkPredicate = "same_as"

// newSelfPrincipalLinkCheck verifies that the local-only self anchor
// (uid="self") is connected by a live same_as relation to the bound
// principal's portable person entity. The daemon's EnsureSelfPrincipalLink
// reconciler establishes this link on every boot, so a FAIL here means the
// link is absent on an install that has a bound, registered principal distinct
// from the self anchor (e.g. an upgrade from before the reconciler existed, or
// the daemon has not booted since genesis).
//
// The check is non-fatal in every state where the link is not yet expected:
// before genesis (no principal bound), when the principal IS the self anchor
// (no separate node to link), and when the principal row is not yet projected.
// Those states return StatusOK because there is nothing to repair.
//
// Returns the concrete type (not the Check interface) so callers can reach the
// Remediator method directly and the constructor stays ireturn-clean; it still
// satisfies Check when appended to a []Check catalog.
func newSelfPrincipalLinkCheck(dataDir string, s *sharedSQLite) *selfPrincipalLinkCheck {
	return &selfPrincipalLinkCheck{dataDir: dataDir, shared: s}
}

// selfPrincipalLinkCheck is a concrete Check + Remediator. It is a struct (not
// a CheckFunc) because the remediation needs more state than a closure carries:
// it opens the SQLite writer pool and the event log to drive the kernelboot
// reconciler.
type selfPrincipalLinkCheck struct {
	dataDir string
	shared  *sharedSQLite
}

// Name returns the dotted check identifier.
func (c *selfPrincipalLinkCheck) Name() string { return "identity.self_principal_link" }

// Subsystem returns the rendering group.
func (c *selfPrincipalLinkCheck) Subsystem() string { return subsystemIdentity }

// Run probes the relations + principals projections (via the shared read-only
// SQLite handle) and the on-disk self binding to decide whether the link is
// present, not-yet-expected, or missing-and-repairable.
func (c *selfPrincipalLinkCheck) Run(ctx context.Context) (Status, string, error) {
	principalUID, err := config.SelfPrincipalUID(c.dataDir)
	if err != nil {
		return StatusFail, "", fmt.Errorf("read self binding: %w", err)
	}
	switch principalUID {
	case "":
		return StatusOK, "no bound principal yet (genesis has not run)", nil
	case selfAnchorUID:
		return StatusOK, "principal is the self anchor; nothing to link", nil
	}

	store, err := c.shared.acquire(ctx)
	if err != nil {
		return StatusFail, "", fmt.Errorf("open sqlite: %w", err)
	}
	reader := store.Reader()

	registered, err := principalProjected(ctx, reader, principalUID)
	if err != nil {
		return StatusFail, "", err
	}
	if !registered {
		return StatusOK, "principal not projected yet; link deferred to next daemon boot", nil
	}

	linked, err := selfLinkLive(ctx, reader, principalUID)
	if err != nil {
		return StatusFail, "", err
	}
	if linked {
		return StatusOK, "self anchor linked to principal " + principalUID, nil
	}
	return StatusFail,
		"self anchor not linked to principal " + principalUID + "; run `maz doctor fix`",
		nil
}

// Remediation links the self anchor to the principal via the idempotent
// kernelboot reconciler. It is safe: it appends a single genesis-style same_as
// relation at personal scope and is a no-op once the link exists.
func (c *selfPrincipalLinkCheck) Remediation() *Remediation {
	return &Remediation{
		Description: "link the local self anchor to the bound principal identity",
		Safe:        true,
		Apply:       c.repair,
	}
}

// repair opens the writer pool + event log and runs EnsureSelfPrincipalLink,
// then rebuilds the projections so the next read observes the link. It refuses
// to run while a daemon owns the data dir: the daemon already reconciles this
// link on boot and holds exclusive locks on the event-log index and graph
// store, so opening the writer here would contend with it.
func (c *selfPrincipalLinkCheck) repair(ctx context.Context) error {
	if mazDaemonAliveForDataDir(c.dataDir) {
		return errors.New(
			"daemon is running and reconciles this link on boot; restart it, or run `maz doctor fix` with the daemon stopped")
	}

	disp, err := dispatcher.Open(ctx, dispatcher.Config{DataDir: c.dataDir})
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	defer closeDispatcher(disp)

	db, err := disp.OpenSQLite(ctx)
	if err != nil {
		return fmt.Errorf("open sqlite writer: %w", err)
	}

	log, closeLog, err := kernelboot.OpenLog(ctx, c.dataDir)
	if err != nil {
		return fmt.Errorf("open event log: %w", err)
	}
	defer closeEventLog(closeLog)

	if lerr := kernelboot.EnsureSelfPrincipalLink(ctx, log, db, c.dataDir); lerr != nil {
		return fmt.Errorf("ensure self-principal link: %w", lerr)
	}
	// EnsureSelfPrincipalLink only appends to the log; project so the next
	// `maz doctor` read (over the relations table) observes the repair.
	if _, rerr := log.Rebuild(ctx, disp); rerr != nil {
		return fmt.Errorf("rebuild after link: %w", rerr)
	}
	return nil
}

// principalProjected reports whether the principals projection holds a row for
// principalUID. Mirrors kernelboot.principalRegistered (kept private there). A
// missing row or missing table (pre-migration) yields (false, nil): there is no
// node to link yet, which the caller treats as "not actionable" rather than an
// error.
func principalProjected(ctx context.Context, db *sql.DB, principalUID string) (bool, error) {
	var got string
	err := db.QueryRowContext(ctx,
		`SELECT principal_uid FROM principals WHERE principal_uid = ?`, principalUID).Scan(&got)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil && isNoSuchTable(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("query principals: %w", err)
	}
	return got != "", nil
}

// selfLinkLive reports whether a live (not retracted/superseded) same_as edge
// connects the self anchor to principalUID. Mirrors
// kernelboot.selfPrincipalLinkExists; "live" follows the navigate layer's
// superseded_by_uid IS NULL/” convention. A missing relations table yields
// (false, nil) so the check reports a repairable FAIL rather than erroring.
func selfLinkLive(ctx context.Context, db *sql.DB, principalUID string) (bool, error) {
	const q = `
		SELECT COUNT(*) FROM relations
		WHERE subject_uid = ?
		  AND predicate = ?
		  AND object_uid = ?
		  AND (superseded_by_uid IS NULL OR superseded_by_uid = '')`
	var n int
	err := db.QueryRowContext(ctx, q, selfAnchorUID, selfLinkPredicate, principalUID).Scan(&n)
	if err != nil {
		if isNoSuchTable(err) {
			return false, nil
		}
		return false, fmt.Errorf("query self-principal link: %w", err)
	}
	return n > 0, nil
}

// isNoSuchTable reports whether err is SQLite's "no such table" error. Mirrors
// the substring form checks.go already uses for the predicates table so the
// doctor layer treats a pre-migration store consistently.
func isNoSuchTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

// closeDispatcher closes the remediation's dispatcher, logging any error to
// stderr without promoting it to a failure (the repair already committed).
func closeDispatcher(d *dispatcher.Dispatcher) {
	if err := d.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "doctor: close dispatcher: %v\n", err)
	}
}

// closeEventLog invokes the event-log closer, logging any error to stderr.
func closeEventLog(closeLog func() error) {
	if err := closeLog(); err != nil {
		fmt.Fprintf(os.Stderr, "doctor: close event log: %v\n", err)
	}
}
