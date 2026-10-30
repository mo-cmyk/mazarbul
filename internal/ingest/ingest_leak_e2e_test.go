package ingest

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/jobs"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
	"github.com/mo-cmyk/mazarbul/internal/sync/privacy"
)

// sentinel is a non-secret-shaped canary placed in a commit body. It survives
// scrubbing (not credential-shaped) so it lands verbatim in the episode text —
// exactly the kind of external content the #204 leak gate must keep out of any
// plaintext chunk.
const sentinel = "SENTINEL-LEAK-CANARY-9F3A2B"

// TestIngestLeak_SealRefusesConnectorEpisodeInPlaintextScope is the AC-3 leak
// E2E: an ingested episode is tagged connector:<name>, so the privacy marker
// flags it sensitive and the chunk sealer fail-closes when asked to write it
// into a plaintext (no scope key) scope. The sentinel body therefore never
// reaches a plaintext chunk; with a scope key it is encrypted, not cleartext.
func TestIngestLeak_SealRefusesConnectorEpisodeInPlaintextScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dir := initRepo(t)
	_ = commit(t, dir, "feat: add widget\n\n"+sentinel)

	app := &fakeAppender{}
	o := NewOrchestrator(app, newFakeReader(), jobs.NewRegistry(), nil)
	if _, err := o.Run(ctx, NewLocalGitSource(dir, nil, nil), kernel.ScopePersonal, Bounds{}, false); err != nil {
		t.Fatalf("Run: %v", err)
	}

	eps := app.eventsOfKind(ontology.KindEpisode)
	if len(eps) == 0 {
		t.Fatal("no episode appended")
	}
	// The episode body carries the sentinel, and the privacy marker flags it.
	var carriesSentinel, flaggedSensitive bool
	for i := range eps {
		if strings.Contains(string(eps[i].Payload), sentinel) {
			carriesSentinel = true
		}
		if privacy.IsSensitivePayload(eps[i]) {
			flaggedSensitive = true
		}
	}
	if !carriesSentinel {
		t.Fatal("episode payload does not carry the sentinel; test cannot prove the leak boundary")
	}
	if !flaggedSensitive {
		t.Fatal("ingested episode is NOT flagged sensitive; the #204 leak gate would not engage")
	}

	// Plaintext seal (no scope key) must fail-closed with ErrSensitivePayloadPlaintext.
	blob, _, err := chunk.EncodeWithOpts(eps, chunk.EncodeOpts{Scope: kernel.ScopePersonal, ScopeKey: nil})
	if !errors.Is(err, chunk.ErrSensitivePayloadPlaintext) {
		t.Fatalf("plaintext seal err = %v, want ErrSensitivePayloadPlaintext", err)
	}
	if blob != nil {
		t.Fatalf("plaintext seal returned %d bytes; must write nothing", len(blob))
	}

	// Encrypted seal succeeds, and the sentinel must not appear as cleartext.
	key := bytes.Repeat([]byte{0x42}, 32)
	enc, _, err := chunk.EncodeWithOpts(eps, chunk.EncodeOpts{Scope: kernel.ScopePersonal, ScopeKey: key})
	if err != nil {
		t.Fatalf("encrypted seal: %v", err)
	}
	if bytes.Contains(enc, []byte(sentinel)) {
		t.Fatal("sentinel leaked as cleartext into an encrypted chunk")
	}
}

// TestIngestLeak_RemoteSourcesSealRefusedInPlaintext is the AC-1 leak regression
// for the #212 remote sources: a Granola transcript and a Claude-chats turn each
// carry their verbatim body into an episode tagged connector:<name>, so the
// privacy marker flags it and a plaintext seal fail-closes — the body never
// reaches a readable chunk, and an encrypted seal does not leak it as cleartext.
func TestIngestLeak_RemoteSourcesSealRefusedInPlaintext(t *testing.T) {
	t.Parallel()

	chatsExport := `[{"uuid":"c1","name":"chat","chat_messages":[` +
		`{"uuid":"m1","sender":"human","content":[{"type":"text","text":"` + sentinel + `"}]}]}]`
	chatsPath := writeExportZip(t, map[string]string{"conversations.json": chatsExport})

	// createdAt omitted (zero time): created_after is empty on a first pull, so the
	// note is served; the body carries the sentinel via summary + transcript.
	gsrv := newGranolaServer(t, []granolaTestNote{
		{id: "not_aaaaaaaaaaaaaa", summary: sentinel, segments: []granolaSegment{granolaSeg("speaker", sentinel)}},
	})

	cases := []struct {
		name string
		src  Source
	}{
		{"claude-chats", NewClaudeChatsSource(chatsPath, fixedNow())},
		{"granola", newGranolaTestSource(gsrv)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := &fakeAppender{}
			o := NewOrchestrator(app, newFakeReader(), jobs.NewRegistry(), nil)
			if _, err := o.Run(context.Background(), tc.src, kernel.ScopePersonal, Bounds{}, false); err != nil {
				t.Fatalf("Run: %v", err)
			}
			eps := app.eventsOfKind(ontology.KindEpisode)
			if len(eps) == 0 {
				t.Fatal("no episode appended")
			}
			var carries, flagged bool
			for i := range eps {
				if strings.Contains(string(eps[i].Payload), sentinel) {
					carries = true
				}
				if privacy.IsSensitivePayload(eps[i]) {
					flagged = true
				}
			}
			if !carries {
				t.Fatal("no episode carries the sentinel body; cannot prove the leak boundary")
			}
			if !flagged {
				t.Fatal("episode not flagged sensitive; the #204 leak gate would not engage")
			}

			blob, _, err := chunk.EncodeWithOpts(eps, chunk.EncodeOpts{Scope: kernel.ScopePersonal, ScopeKey: nil})
			if !errors.Is(err, chunk.ErrSensitivePayloadPlaintext) {
				t.Fatalf("plaintext seal err = %v, want ErrSensitivePayloadPlaintext", err)
			}
			if blob != nil {
				t.Fatalf("plaintext seal wrote %d bytes; must write nothing", len(blob))
			}

			key := bytes.Repeat([]byte{0x42}, 32)
			enc, _, err := chunk.EncodeWithOpts(eps, chunk.EncodeOpts{Scope: kernel.ScopePersonal, ScopeKey: key})
			if err != nil {
				t.Fatalf("encrypted seal: %v", err)
			}
			if bytes.Contains(enc, []byte(sentinel)) {
				t.Fatal("sentinel leaked as cleartext into an encrypted chunk")
			}
		})
	}
}

// TestIngestLeak_WatermarkEventNotSensitive guards that the resume watermark
// (internal metadata, not external content) is NOT connector-tagged, so it can
// seal in any scope and never blocks a plaintext sync on its own.
func TestIngestLeak_WatermarkEventNotSensitive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := initRepo(t)
	_ = commit(t, dir, "chore: bump")

	app := &fakeAppender{}
	o := NewOrchestrator(app, newFakeReader(), jobs.NewRegistry(), nil)
	if _, err := o.Run(ctx, NewLocalGitSource(dir, nil, nil), kernel.ScopePersonal, Bounds{}, false); err != nil {
		t.Fatalf("Run: %v", err)
	}
	wms := app.eventsOfKind(ontology.KindConnectorWatermarkAdvanced)
	if len(wms) == 0 {
		t.Fatal("no watermark event appended")
	}
	for i := range wms {
		if privacy.IsSensitivePayload(wms[i]) {
			t.Errorf("watermark event %s flagged sensitive; resume metadata must not trip the leak gate", wms[i].UID)
		}
	}
}
