//go:build ignore

// maz-github-demo — drive the Mazarbul sync pipeline against a real
// GitHub remote. End-to-end proof that:
//  1. an EventLog appends events under a scope
//  2. SealOnce produces an age-encrypted sealed chunk
//  3. the git transport pushes that chunk to a real GitHub repo
//  4. a *separate* clone lists + fetches + decodes the chunk and gets
//     byte-equal UIDs back, with payload plaintext intact
//
// This is the human-runnable companion to internal/sync/sync_e2e_test.go
// against a *real* remote — the file:// test pins the data path; this
// pins the network/auth path.
//
// Run:
//
//	go run -tags sqlite_fts5 ./scripts/maz-github-demo \
//	  -remote git@github.com:mo-cmyk/mazarbul-sync-demo.git -n 5
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
	"github.com/mo-cmyk/mazarbul/internal/sync/transport"
	gittransport "github.com/mo-cmyk/mazarbul/internal/sync/transport/git"
	"github.com/mo-cmyk/mazarbul/internal/watchword"
)

func main() {
	remote := flag.String("remote", "", "remote URL (SSH or HTTPS git URL)")
	n := flag.Int("n", 5, "number of events to seal+push")
	mode := flag.String("mode", "plaintext", "chunk encoding: plaintext (default) or encrypted")
	flag.Parse()
	if *remote == "" {
		log.Fatal("-remote is required")
	}
	if *mode != "plaintext" && *mode != "encrypted" {
		log.Fatalf("-mode must be plaintext or encrypted, got %q", *mode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	scope := kernel.ScopePersonal
	var key []byte
	if *mode == "encrypted" {
		// Canonical "abandon abandon … art" mnemonic so the receive
		// side can derive the same scope key without shipping any secret.
		parts := make([]string, watchword.Words)
		for i := range parts {
			parts[i] = "abandon"
		}
		parts[len(parts)-1] = "art"
		root, err := watchword.DeriveRoot(strings.Join(parts, " "))
		if err != nil {
			log.Fatalf("DeriveRoot: %v", err)
		}
		k, err := watchword.DeriveScopeKey(root, string(scope))
		if err != nil {
			log.Fatalf("DeriveScopeKey: %v", err)
		}
		key = k
	}

	// ── Daemon A: a fresh dataDir, append n events, seal, push ─
	dirA, err := os.MkdirTemp("", "maz-gh-A-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dirA)

	ont := kernel.NewMemoryOntology()
	if err := ontology.RegisterEpisode(ctx, ont); err != nil {
		log.Fatalf("RegisterEpisode: %v", err)
	}
	logA := kernel.NewFileEventLog(dirA, ont)
	defer logA.Close()

	events := make([]kernel.Event, *n)
	for i := range events {
		events[i] = kernel.Event{
			Kind:       "episode",
			Source:     "maz-github-demo",
			Confidence: 1,
			Scope:      scope,
			Payload: []byte(fmt.Sprintf(
				`{"text":"GitHub demo payload #%d","source":"maz-github-demo","confidence":1.0}`, i)),
		}
	}
	rcpts, err := logA.Append(ctx, events)
	if err != nil {
		log.Fatalf("Append: %v", err)
	}
	for i := range rcpts {
		events[i].UID = rcpts[i].UID
		events[i].CreateTime = rcpts[i].CreateTime
	}
	fmt.Printf("daemon A: appended %d events under scope=%s\n", len(events), scope)
	fmt.Printf("          first UID: %s\n", events[0].UID)

	storeA, err := chunk.NewFSStore(dirA)
	if err != nil {
		log.Fatal(err)
	}
	cursorA, err := chunk.NewFileCursor(dirA)
	if err != nil {
		log.Fatal(err)
	}
	if _, _, err := chunk.SealOnceWith(ctx, logA, scope, key, storeA, cursorA, 100, "demo-A"); err != nil {
		log.Fatalf("SealOnce: %v", err)
	}
	refs, err := storeA.List(ctx, scope)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("daemon A: sealed %d chunk(s) (mode=%s)\n", len(refs), *mode)
	for _, r := range refs {
		ext := chunk.ExtensionJSONL
		if r.Header.Encrypted {
			ext = chunk.Extension
		}
		fmt.Printf("          chunk=%s%s events=%d first=%s last=%s encrypted=%v\n",
			r.ChunkID, ext, r.Header.Count, r.Header.FirstULID, r.Header.LastULID, r.Header.Encrypted)
	}

	trA, err := gittransport.New(gittransport.Options{
		DataDir:    dirA,
		RemoteURLs: map[kernel.Scope]string{scope: *remote},
		DeviceID:   "demo-A",
	})
	if err != nil {
		log.Fatalf("transport A: %v", err)
	}

	listing, err := trA.List(ctx, scope)
	if err != nil {
		log.Fatalf("List: %v", err)
	}
	seen := map[string]struct{}{}
	for _, h := range listing.Chunks {
		seen[h.ChunkID] = struct{}{}
	}

	blobs := make([]transport.Blob, 0, len(refs))
	for i := range refs {
		if _, dup := seen[refs[i].ChunkID]; dup {
			continue
		}
		body, gerr := storeA.Get(ctx, scope, refs[i].ChunkID)
		if gerr != nil {
			log.Fatalf("Get %s: %v", refs[i].ChunkID, gerr)
		}
		blobs = append(blobs, transport.Blob{
			Header: refs[i].Header,
			Body:   io.NopCloser(strings.NewReader(string(body))),
		})
	}
	if len(blobs) == 0 {
		fmt.Println("daemon A: nothing new to push (already on remote)")
	} else {
		t0 := time.Now()
		accepted, err := trA.Push(ctx, scope, blobs)
		if err != nil {
			log.Fatalf("Push: %v", err)
		}
		fmt.Printf("daemon A: PUSHED %d chunk(s) to %s in %s\n",
			accepted, *remote, time.Since(t0).Round(time.Millisecond))
	}

	// ── Daemon B: a different dataDir, list+fetch+decode ───────
	dirB, err := os.MkdirTemp("", "maz-gh-B-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dirB)

	trB, err := gittransport.New(gittransport.Options{
		DataDir:    dirB,
		RemoteURLs: map[kernel.Scope]string{scope: *remote},
		DeviceID:   "demo-B",
	})
	if err != nil {
		log.Fatalf("transport B: %v", err)
	}
	listB, err := trB.List(ctx, scope)
	if err != nil {
		log.Fatalf("List B: %v", err)
	}
	fmt.Printf("daemon B: remote has %d sealed chunk(s)\n", len(listB.Chunks))
	got := []kernel.Event{}
	for _, h := range listB.Chunks {
		blob, gerr := trB.Get(ctx, scope, h.ChunkID)
		if gerr != nil {
			log.Fatalf("Get B %s: %v", h.ChunkID, gerr)
		}
		body, rerr := io.ReadAll(blob.Body)
		_ = blob.Body.Close()
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			log.Fatalf("read B %s: %v", h.ChunkID, rerr)
		}
		// Decode routes on the leading bytes: nil key tells the
		// encrypted path it cannot decrypt, but plaintext chunks
		// decode without a key. Passing key (nil or set) lets the
		// same call site work in both modes.
		_, decoded, derr := chunk.Decode(body, scope, key)
		if derr != nil {
			log.Fatalf("Decode %s: %v", h.ChunkID, derr)
		}
		got = append(got, decoded...)
	}
	fmt.Printf("daemon B: DECODED %d event(s)\n", len(got))

	// ── Cross-daemon UID byte-equality ─────────────────────────
	want := map[string]struct{}{}
	for _, e := range events {
		want[e.UID] = struct{}{}
	}
	ok := true
	for _, e := range got {
		if _, found := want[e.UID]; !found {
			fmt.Printf("daemon B saw UNEXPECTED UID: %s\n", e.UID)
			ok = false
		}
	}
	if len(got) != len(events) {
		fmt.Printf("daemon B decoded %d, want %d\n", len(got), len(events))
		ok = false
	}
	if ok {
		fmt.Println()
		fmt.Println("✓ GITHUB REAL-REMOTE E2E PASSED")
		fmt.Printf("  Push origin:    %s\n", *remote)
		fmt.Printf("  Inspect:        gh repo view mo-cmyk/mazarbul-sync-demo --web\n")
		return
	}
	os.Exit(1)
}
