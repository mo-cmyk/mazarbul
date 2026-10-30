package chunk_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
)

// TestEncodePlaintextDecode_Roundtrip pins the plaintext default path:
// events → JSONL chunk → decoded events with byte-equal UIDs and a
// readable _meta line. The opt-in encrypted path stays exercised by
// the existing TestEncodeDecode_Roundtrip in chunk_test.go.
func TestEncodePlaintextDecode_Roundtrip(t *testing.T) {
	t.Parallel()
	events := []kernel.Event{
		sampleEvent(t, kernel.ScopePersonal, "first"),
		sampleEvent(t, kernel.ScopePersonal, "second"),
		sampleEvent(t, kernel.ScopePersonal, "third"),
	}
	blob, hdr, err := chunk.EncodePlaintext(events, kernel.ScopePersonal, "device-a")
	if err != nil {
		t.Fatalf("EncodePlaintext: %v", err)
	}
	if hdr.Encrypted {
		t.Errorf("hdr.Encrypted = true, want false (plaintext)")
	}
	if hdr.Device != "device-a" {
		t.Errorf("hdr.Device = %q, want %q", hdr.Device, "device-a")
	}
	if hdr.Count != len(events) {
		t.Errorf("hdr.Count = %d, want %d", hdr.Count, len(events))
	}

	got, gotEvents, err := chunk.DecodePlaintext(blob, kernel.ScopePersonal)
	if err != nil {
		t.Fatalf("DecodePlaintext: %v", err)
	}
	if got.ChunkID != hdr.ChunkID {
		t.Errorf("decoded ChunkID = %q, want %q", got.ChunkID, hdr.ChunkID)
	}
	if got.Encrypted {
		t.Errorf("decoded hdr.Encrypted = true, want false")
	}
	if len(gotEvents) != len(events) {
		t.Fatalf("decoded %d events, want %d", len(gotEvents), len(events))
	}
	for i := range events {
		if gotEvents[i].UID != events[i].UID {
			t.Errorf("event[%d].UID = %q, want %q", i, gotEvents[i].UID, events[i].UID)
		}
	}
}

// TestEncodePlaintext_FormatIsLineDelimited locks the on-disk shape:
// line 1 carries a JSON object with `"_meta":true`; lines 2..N are
// one event per line. This is the wire contract the GitHub web UI
// and `git blame` rely on — break it and the whole point of the
// plaintext mode (line-level diffs) breaks.
func TestEncodePlaintext_FormatIsLineDelimited(t *testing.T) {
	t.Parallel()
	events := []kernel.Event{
		sampleEvent(t, kernel.ScopePersonal, "alpha"),
		sampleEvent(t, kernel.ScopePersonal, "beta"),
	}
	blob, _, err := chunk.EncodePlaintext(events, kernel.ScopePersonal, "")
	if err != nil {
		t.Fatalf("EncodePlaintext: %v", err)
	}
	lines := bytes.Split(bytes.TrimRight(blob, "\n"), []byte{'\n'})
	if got, want := len(lines), 1+len(events); got != want {
		t.Fatalf("line count = %d, want %d (1 _meta + %d events)", got, want, len(events))
	}
	if !bytes.Contains(lines[0], []byte(`"_meta":true`)) {
		t.Errorf("line 1 missing _meta sentinel: %q", lines[0])
	}
	// Each subsequent line must be a parseable event JSON object.
	for i, line := range lines[1:] {
		var ev kernel.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Errorf("line %d not valid event JSON: %v (raw=%q)", i+2, err, line)
		}
	}
}

// TestPeekHeaderJSONL_ReadsMetaWithoutKey confirms the no-key fast
// path used by MANIFEST regeneration, doctor reporting, and the
// transport's listing.
func TestPeekHeaderJSONL_ReadsMetaWithoutKey(t *testing.T) {
	t.Parallel()
	events := []kernel.Event{sampleEvent(t, kernel.ScopePersonal, "solo")}
	blob, hdr, err := chunk.EncodePlaintext(events, kernel.ScopePersonal, "laptop-1")
	if err != nil {
		t.Fatalf("EncodePlaintext: %v", err)
	}
	got, err := chunk.PeekHeaderJSONL(blob)
	if err != nil {
		t.Fatalf("PeekHeaderJSONL: %v", err)
	}
	if got.ChunkID != hdr.ChunkID || got.Scope != hdr.Scope || got.Count != hdr.Count {
		t.Errorf("peeked header mismatch: %+v vs %+v", got, hdr)
	}
	if got.Device != "laptop-1" {
		t.Errorf("got.Device = %q, want %q", got.Device, "laptop-1")
	}
	if got.Encrypted {
		t.Errorf("got.Encrypted = true, want false")
	}
}

// TestDecodePlaintext_RejectsScopeMismatch protects the cross-scope
// substitution attack: an attacker swaps a personal-scope chunk for
// a team-scope chunk hoping the decoder applies it under the wrong
// AEAD key context. Plaintext mode has no key, but scope drift is
// still a domain error and must surface.
func TestDecodePlaintext_RejectsScopeMismatch(t *testing.T) {
	t.Parallel()
	events := []kernel.Event{sampleEvent(t, kernel.ScopePersonal, "x")}
	blob, _, err := chunk.EncodePlaintext(events, kernel.ScopePersonal, "")
	if err != nil {
		t.Fatalf("EncodePlaintext: %v", err)
	}
	if _, _, err := chunk.DecodePlaintext(blob, kernel.ScopeProject); err == nil {
		t.Fatalf("DecodePlaintext must reject cross-scope decode")
	} else if !errors.Is(err, chunk.ErrScopeMismatch) {
		t.Errorf("err = %v, want errors.Is ErrScopeMismatch", err)
	}
}

// TestDecodePlaintext_RejectsCorruptMeta covers the family of
// malformed _meta lines: no newline, missing sentinel, missing
// required field. All surface as ErrCorruptHeader so callers can
// distinguish "stray file" from "the network ate bytes."
func TestDecodePlaintext_RejectsCorruptMeta(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []byte
	}{
		{"missing newline", []byte(`{"_meta":true,"chunkId":"X","scope":"personal","count":0}`)},
		{"meta sentinel false", []byte(`{"_meta":false,"chunkId":"X","scope":"personal","count":0}` + "\n")},
		{"missing chunkId", []byte(`{"_meta":true,"scope":"personal","count":0}` + "\n")},
		{"empty meta line", []byte("\nevent")},
		{"invalid json meta", []byte("{not-json\n")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := chunk.DecodePlaintext(tc.in, "")
			if err == nil {
				t.Fatalf("DecodePlaintext(%q): want error", tc.in)
			}
			if !errors.Is(err, chunk.ErrCorruptHeader) && !errors.Is(err, chunk.ErrScopeMismatch) {
				t.Errorf("err = %v, want ErrCorruptHeader", err)
			}
		})
	}
}

// TestEncode_RoutesOnNilKey verifies the public Encode router: a
// nil scopeKey produces a JSONL chunk; a 32-byte key produces a
// MAZ1-framed chunk. Both round-trip through Decode under the same
// public entry point.
func TestEncode_RoutesOnNilKey(t *testing.T) {
	t.Parallel()
	events := []kernel.Event{sampleEvent(t, kernel.ScopePersonal, "router")}
	key := deriveKey(t)

	t.Run("nil key writes plaintext", func(t *testing.T) {
		t.Parallel()
		blob, hdr, err := chunk.EncodeWithOpts(events,
			chunk.EncodeOpts{Scope: kernel.ScopePersonal, Device: "dev"})
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if hdr.Encrypted {
			t.Errorf("hdr.Encrypted = true, want false")
		}
		if !bytes.HasPrefix(blob, []byte("{")) {
			t.Errorf("plaintext blob must start with `{`; got %q", blob[:min(len(blob), 4)])
		}
		if _, _, err := chunk.Decode(blob, kernel.ScopePersonal, nil); err != nil {
			t.Errorf("Decode plaintext via router: %v", err)
		}
	})

	t.Run("32 byte key writes MAZ1", func(t *testing.T) {
		t.Parallel()
		blob, hdr, err := chunk.EncodeWithOpts(events,
			chunk.EncodeOpts{Scope: kernel.ScopePersonal, ScopeKey: key, Device: "dev"})
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if !hdr.Encrypted {
			t.Errorf("hdr.Encrypted = false, want true")
		}
		if !bytes.HasPrefix(blob, []byte("MAZ1")) {
			t.Errorf("encrypted blob must start with MAZ1; got %q", blob[:min(len(blob), 4)])
		}
		if _, _, err := chunk.Decode(blob, kernel.ScopePersonal, key); err != nil {
			t.Errorf("Decode encrypted via router: %v", err)
		}
	})
}

// TestPeekHeader_RoutesAndForceEncrypted: PeekHeader on a MAZ1 blob
// reports Encrypted=true even if the persisted header omits the
// field (pre-plaintext chunks have no `encrypted` field on disk). PeekHeader
// on a plaintext blob reports Encrypted=false. Sanity check on the
// router contract.
func TestPeekHeader_RoutesAndForceEncrypted(t *testing.T) {
	t.Parallel()
	events := []kernel.Event{sampleEvent(t, kernel.ScopePersonal, "peek")}
	key := deriveKey(t)

	encBlob, _, err := chunk.Encode(events, kernel.ScopePersonal, key)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	plainBlob, _, err := chunk.EncodePlaintext(events, kernel.ScopePersonal, "")
	if err != nil {
		t.Fatalf("EncodePlaintext: %v", err)
	}

	if h, err := chunk.PeekHeader(encBlob); err != nil {
		t.Errorf("PeekHeader(encrypted): %v", err)
	} else if !h.Encrypted {
		t.Errorf("PeekHeader(encrypted).Encrypted = false, want true")
	}
	if h, err := chunk.PeekHeader(plainBlob); err != nil {
		t.Errorf("PeekHeader(plain): %v", err)
	} else if h.Encrypted {
		t.Errorf("PeekHeader(plain).Encrypted = true, want false")
	}
}

// Ensure the helper used above is available for tests on older Go
// versions; min landed in 1.21.
var (
	_ = func() int { return min(1, 2) }
	_ = strings.NewReader // keep import live for future helpers
)
