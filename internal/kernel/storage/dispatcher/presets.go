package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// applyPresetInstalled upserts one row into the presets projection (#210). The
// row carries the verbatim instructions body so the getContext brief can read
// active presets without re-scanning the log. Idempotent under replay via
// ON CONFLICT(name): re-installing or replaying the same event converges to the
// same row, so `maz rebuild` reconstructs the projection deterministically.
func (d *Dispatcher) applyPresetInstalled(ctx context.Context, ev kernel.Event) error {
	var p ontology.PresetInstalledEvent
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return fmt.Errorf("decode presetInstalled payload: %w", err)
	}
	if _, err := d.sqlite.Writer().ExecContext(ctx,
		`INSERT INTO presets (name, version, description, instructions, added_at, source_url, pinned_ref, checksum)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
			version      = excluded.version,
			description  = excluded.description,
			instructions = excluded.instructions,
			added_at     = excluded.added_at,
			source_url   = excluded.source_url,
			pinned_ref   = excluded.pinned_ref,
			checksum     = excluded.checksum`,
		p.Name,
		p.Version,
		p.Description,
		p.Instructions,
		p.AddedAt.UTC().Format(time.RFC3339Nano),
		p.SourceURL,
		p.PinnedRef,
		p.Checksum,
	); err != nil {
		return fmt.Errorf("upsert preset %q: %w", p.Name, err)
	}
	return nil
}

// applyPresetDeactivated DELETEs the preset's row from the projection — a clean
// cut, no inactive flag. The kinds and predicates the install seeded are NOT
// touched (they live in entity_kinds/predicates as extension ontology), so a
// preset's vocabulary stays resolvable after uninstall. Idempotent: deleting an
// absent row is a no-op, so a replayed or duplicate deactivate converges.
func (d *Dispatcher) applyPresetDeactivated(ctx context.Context, ev kernel.Event) error {
	var p ontology.PresetDeactivatedEvent
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return fmt.Errorf("decode presetDeactivated payload: %w", err)
	}
	if _, err := d.sqlite.Writer().ExecContext(ctx,
		`DELETE FROM presets WHERE name = ?`, p.Name); err != nil {
		return fmt.Errorf("delete preset %q: %w", p.Name, err)
	}
	return nil
}
