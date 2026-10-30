// Package privacy owns Mazarbul's fail-closed sensitive-payload
// predicate: the single place that decides whether an event must never
// reach a plaintext sync surface (a cleartext .jsonl chunk, the git
// commit message, or the human-readable rendered/ tree).
//
// The predicate is consulted at three boundaries:
//
//   - the seal boundary (chunk.EncodeWithOpts), the load-bearing gate —
//     a sensitive event bound for a plaintext, non-project scope refuses
//     the whole seal batch so no cleartext chunk is ever written;
//   - the git transport (writeBlob) and the renderer
//     (collectPlaintextEvents), as defence-in-depth mirrors that catch a
//     plaintext chunk arriving by a non-sealer path (pulled from a
//     remote, or sealed before a marker existed);
//   - the `maz doctor` runtime scan, which FAILs when a plaintext scope
//     already holds a sensitive chunk.
//
// The mechanism is feature-agnostic. Downstream features register one
// Marker each (the typed source_type taxonomy in #207, presets in #210,
// connectors in #211/#212, …) and ship one byte-level regression test;
// they never add a second gate. The two built-in seed markers below give
// the gate standalone single-user value today, before any of those land.
package privacy

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// Marker reports whether ev carries a sensitive payload that must never
// be written in cleartext. A Marker is a cheap, pure field/payload read;
// it must not block or mutate. Default to true when uncertain — this is
// a fail-closed surface.
type Marker func(ev kernel.Event) bool

type registeredMarker struct {
	name string
	fn   Marker
}

// markers is the sensitive-payload marker registry. It is pre-seeded
// with the built-in markers and grows via RegisterMarker. The RWMutex
// guards the slice because IsSensitivePayload is read concurrently on
// the seal/push/render paths while RegisterMarker may append during
// boot wiring or in tests.
//
//nolint:gochecknoglobals // process-wide sensitive-payload registry by design; mirrors chunkIDMu.
var (
	markersMu sync.RWMutex
	markers   []registeredMarker
)

// registerSeeds wires the built-in markers through the public
// RegisterMarker hook at package load. A var-initialiser sentinel (not
// func init(), which is lint-gated) keeps RegisterMarker on the reachable
// path and exercises the exact extension contract downstream features use.
//

var _ = func() struct{} {
	RegisterMarker("source.connector", sourceConnectorMarker)
	RegisterMarker("payload.explicit_flag", explicitFlagMarker)
	return struct{}{}
}()

// RegisterMarker appends m to the registry under name. Call it exactly
// once per feature, from that feature's explicit wiring (not an init()
// — production init() is lint-gated). Panics on a nil Marker: a missing
// marker is a programmer error that must surface at process start, never
// silently weaken the gate at steady state.
func RegisterMarker(name string, m Marker) {
	if m == nil {
		panic("privacy: RegisterMarker(" + name + "): nil Marker")
	}
	markersMu.Lock()
	defer markersMu.Unlock()
	markers = append(markers, registeredMarker{name: name, fn: m})
}

// IsSensitivePayload reports whether ev trips any registered marker. The
// fail-closed predicate every boundary consults. O(markers); markers are
// cheap reads.
func IsSensitivePayload(ev kernel.Event) bool {
	markersMu.RLock()
	defer markersMu.RUnlock()
	for i := range markers {
		if markers[i].fn(ev) {
			return true
		}
	}
	return false
}

// MarkerNames returns the registered marker identifiers, sorted. Used by
// the doctor check's detail string and by the registry regression test.
func MarkerNames() []string {
	markersMu.RLock()
	defer markersMu.RUnlock()
	names := make([]string, len(markers))
	for i := range markers {
		names[i] = markers[i].name
	}
	sort.Strings(names)
	return names
}

// Built-in seed markers ----------------------------------------------------

// sourceConnectorMarker treats any event pulled by an external connector
// as sensitive. It delegates to ontology.IsConnectorSource — the single
// shared definition of the "connector:" / "connector/" Source-prefix
// convention — so the privacy leak boundary and the resolver trust
// boundary (issue #211) can never drift on what counts as a connector
// source. Zero false positives on today's data: every current Source is
// tool:verb (cli:remember, mcp:fact, extractor:m1, …), none connector:*.
func sourceConnectorMarker(ev kernel.Event) bool {
	return ontology.IsConnectorSource(ev.Source)
}

// explicitFlagMarker honours an opt-in top-level "sensitive" flag in the
// event payload: a writer that knows a payload is sensitive can mark it
// directly without minting a new marker. Fail-closed: once the key is
// present, anything other than an explicit false/null counts as
// sensitive (so a stringified "true", a 1, or a non-bool shape cannot
// quietly bypass the gate). A non-object payload carries no flag and is
// left to the other markers.
func explicitFlagMarker(ev kernel.Event) bool {
	if len(ev.Payload) == 0 {
		return false
	}
	var probe struct {
		Sensitive json.RawMessage `json:"sensitive"`
	}
	if err := json.Unmarshal(ev.Payload, &probe); err != nil {
		return false
	}
	if len(probe.Sensitive) == 0 {
		return false // key absent
	}
	switch strings.TrimSpace(string(probe.Sensitive)) {
	case "false", "null":
		return false
	default:
		return true
	}
}
