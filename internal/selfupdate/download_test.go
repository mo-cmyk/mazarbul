package selfupdate

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestDownloadFileScheme(t *testing.T) {
	t.Parallel()
	mirror := t.TempDir()
	archivePath, _, name := writeFixtureArchive(t, mirror, "v9.9.9", "darwin", "arm64")

	rel, err := ResolveRelease(context.Background(), &Options{BaseURL: "file://" + mirror, Version: "v9.9.9", OS: "darwin", Arch: "arm64"})
	if err != nil {
		t.Fatalf("ResolveRelease: %v", err)
	}
	dest := t.TempDir()
	gotArchive, gotSums, err := Download(context.Background(), &Options{BaseURL: "file://" + mirror}, rel, dest)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	orig, _ := os.ReadFile(archivePath)
	got, _ := os.ReadFile(gotArchive)
	if !bytes.Equal(orig, got) {
		t.Errorf("downloaded archive differs from source")
	}
	// End-to-end: the downloaded pair must pass verification.
	if err := VerifyChecksum(gotArchive, gotSums, name); err != nil {
		t.Errorf("VerifyChecksum after file:// download: %v", err)
	}
}

func TestDownloadStripsTokenOnCrossHostRedirect(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var assetAuth, storageAuth []string

	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		storageAuth = append(storageAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		_, _ = w.Write([]byte("BYTES"))
	}))
	defer storage.Close()

	name := archiveName("v1.0.0", "darwin", "arm64")
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/releases/assets/") {
			mu.Lock()
			assetAuth = append(assetAuth, r.Header.Get("Authorization"))
			mu.Unlock()
			http.Redirect(w, r, storage.URL+"/signed-blob", http.StatusFound)
			return
		}
		// The tags endpoint resolves the asset ids.
		_, _ = w.Write([]byte(`{"tag_name":"v1.0.0","assets":[{"id":11,"name":"` + name + `"},{"id":22,"name":"checksums.txt"}]}`))
	}))
	defer api.Close()

	opts := &Options{APIBase: api.URL, Token: "tok", Version: "v1.0.0", OS: "darwin", Arch: "arm64"}
	rel, err := ResolveRelease(context.Background(), opts)
	if err != nil {
		t.Fatalf("ResolveRelease: %v", err)
	}
	if _, _, err := Download(context.Background(), opts, rel, t.TempDir()); err != nil {
		t.Fatalf("Download: %v", err)
	}

	if len(assetAuth) == 0 {
		t.Fatal("expected the asset endpoint to be hit")
	}
	for _, a := range assetAuth {
		if a != "Bearer tok" {
			t.Errorf("asset request Authorization = %q, want Bearer tok", a)
		}
	}
	for _, a := range storageAuth {
		if a != "" {
			t.Errorf("storage request leaked Authorization = %q, want empty", a)
		}
	}
}
