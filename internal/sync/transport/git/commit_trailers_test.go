package git

// commit_trailers_test.go pins the authorship trailers (Workstream A): the
// Principal:/Device:/Agent:/Co-authored-by:/Mazarbul-Identity: lines that make
// a sync commit attribute to the real user and name peer contributors.

import (
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
)

func trailerSummary() pushSummary {
	return pushSummary{
		Scope:    kernel.ScopePersonal,
		DeviceID: "macbook-7f3a",
		Tally:    pushTally{plaintext: 1},
		Heads: []chunk.Header{
			{ChunkID: "01KT8X4KCDE22Z5RBJZVX626EM", Scope: kernel.ScopePersonal, Count: 1, Encrypted: false},
		},
		PrincipalName:  "Frodo Baggins",
		PrincipalEmail: "12345+frodo@users.noreply.github.com",
		CommitMode:     "noreply",
	}
}

func TestWriteTrailers_SinglePrincipal(t *testing.T) {
	s := trailerSummary()
	s.Agents = []string{"connector:granola", "mcp"}
	msg := buildCommitMessage(s)

	for _, want := range []string{
		"Principal: Frodo Baggins <12345+frodo@users.noreply.github.com>",
		"Device: macbook-7f3a",
		"Agent: connector:granola, mcp",
		"Mazarbul-Identity: noreply",
		commitSchemaTrailer,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("commit message missing trailer %q\n---\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "Co-authored-by:") {
		t.Errorf("single-principal push must not emit Co-authored-by\n%s", msg)
	}
}

func TestWriteTrailers_MultiPrincipalCoAuthors(t *testing.T) {
	s := trailerSummary()
	s.CoAuthors = []string{
		"Sam Gamgee <67890+sam@users.noreply.github.com>",
		"Pippin Took <11111+pippin@users.noreply.github.com>",
	}
	msg := buildCommitMessage(s)

	for _, want := range []string{
		"Co-authored-by: Sam Gamgee <67890+sam@users.noreply.github.com>",
		"Co-authored-by: Pippin Took <11111+pippin@users.noreply.github.com>",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("commit message missing %q\n---\n%s", want, msg)
		}
	}
}

func TestWriteTrailers_EncryptedSuppressesPeerAndAgent(t *testing.T) {
	// An encrypted push carries no decoded payloads, so the privacy contract
	// forbids peer/agent attribution — but the committing user is still named.
	s := pushSummary{
		Scope:          kernel.ScopeTeam,
		DeviceID:       "macbook-7f3a",
		Tally:          pushTally{encrypted: 1},
		Heads:          []chunk.Header{{ChunkID: "01KT8X4KCDE22Z5RBJZVX626EM", Scope: kernel.ScopeTeam, Encrypted: true}},
		PrincipalName:  "Frodo Baggins",
		PrincipalEmail: "12345+frodo@users.noreply.github.com",
		CommitMode:     "noreply",
		// Details/Agents/CoAuthors deliberately nil (encrypted contract).
	}
	msg := buildCommitMessage(s)

	if !strings.Contains(msg, "Principal: Frodo Baggins <12345+frodo@users.noreply.github.com>") {
		t.Errorf("encrypted push must still name the committing principal\n%s", msg)
	}
	if !strings.Contains(msg, "Device: macbook-7f3a") {
		t.Errorf("encrypted push must keep the Device trailer\n%s", msg)
	}
	if strings.Contains(msg, "Co-authored-by:") || strings.Contains(msg, "Agent:") {
		t.Errorf("encrypted push must not leak peer/agent attribution\n%s", msg)
	}
}

func TestCollectAuthorship_DistinctPeersAndSourcesExcludingSelf(t *testing.T) {
	sources := map[string]struct{}{}
	peers := map[string]struct{}{}
	events := []kernel.Event{
		{UID: "1", Source: "mcp", Author: kernel.EventAuthor{PrincipalUID: "self"}},
		{UID: "2", Source: "mcp", Author: kernel.EventAuthor{PrincipalUID: "peer-a"}},
		{UID: "3", Source: "connector:granola", Author: kernel.EventAuthor{PrincipalUID: "peer-a"}},
		{UID: "4", Source: "", Author: kernel.EventAuthor{PrincipalUID: "peer-b"}},
	}
	collectAuthorship(sources, peers, events, "self")

	if _, ok := peers["self"]; ok {
		t.Errorf("self principal must be excluded from peers")
	}
	// peers: peer-a, peer-b. sources: mcp, connector:granola (blank skipped).
	if len(peers) != 2 {
		t.Errorf("peers = %v; want exactly {peer-a, peer-b}", peers)
	}
	if len(sources) != 2 {
		t.Errorf("sources = %v; want exactly {mcp, connector:granola}", sources)
	}
	if _, ok := sources[""]; ok {
		t.Errorf("blank source must be skipped")
	}
}
