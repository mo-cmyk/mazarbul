package selfupdate

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolveRelease(t *testing.T) {
	t.Parallel()

	t.Run("resolves the stable latest tag", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/repos/"+Repo+"/releases/latest" {
				t.Errorf("unexpected path %s", r.URL.Path)
			}
			fmt.Fprint(w, `{"tag_name":"v1.2.3"}`)
		}))
		defer srv.Close()
		rel, err := ResolveRelease(context.Background(), &Options{APIBase: srv.URL, Channel: ChannelStable, OS: "darwin", Arch: "arm64"})
		if err != nil {
			t.Fatalf("ResolveRelease: %v", err)
		}
		if rel.Tag != "v1.2.3" || rel.ViaAsset {
			t.Errorf("got tag=%q viaAsset=%v, want v1.2.3 false", rel.Tag, rel.ViaAsset)
		}
		want := defaultDownloadBase + "/v1.2.3/mazarbul_v1.2.3_darwin_arm64.tar.gz"
		if rel.ArchiveURL != want {
			t.Errorf("archive URL = %q, want %q", rel.ArchiveURL, want)
		}
	})

	t.Run("resolves the newest nightly entry", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/repos/"+Repo+"/releases" {
				t.Errorf("unexpected path %s", r.URL.Path)
			}
			fmt.Fprint(w, `[{"tag_name":"v1.3.0-nightly"}]`)
		}))
		defer srv.Close()
		rel, err := ResolveRelease(context.Background(), &Options{APIBase: srv.URL, Channel: ChannelNightly, OS: "linux", Arch: "amd64"})
		if err != nil {
			t.Fatalf("ResolveRelease: %v", err)
		}
		if rel.Tag != "v1.3.0-nightly" {
			t.Errorf("tag = %q, want v1.3.0-nightly", rel.Tag)
		}
	})

	t.Run("pinned version wins over channel without an API call", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			t.Errorf("pinned version must not query the API, got %s", r.URL.Path)
		}))
		defer srv.Close()
		rel, err := ResolveRelease(context.Background(), &Options{APIBase: srv.URL, Channel: ChannelNightly, Version: "v0.0.9", OS: "darwin", Arch: "arm64"})
		if err != nil {
			t.Fatalf("ResolveRelease: %v", err)
		}
		if rel.Tag != "v0.0.9" {
			t.Errorf("tag = %q, want v0.0.9", rel.Tag)
		}
	})

	t.Run("base url bypasses the API", func(t *testing.T) {
		t.Parallel()
		rel, err := ResolveRelease(context.Background(), &Options{BaseURL: "http://mirror.example/d", Version: "v1.0.0", OS: "darwin", Arch: "arm64"})
		if err != nil {
			t.Fatalf("ResolveRelease: %v", err)
		}
		if rel.ViaAsset {
			t.Errorf("base url path should not be viaAsset")
		}
		if rel.ArchiveURL != "http://mirror.example/d/mazarbul_v1.0.0_darwin_arm64.tar.gz" {
			t.Errorf("archive URL = %q", rel.ArchiveURL)
		}
		if rel.SumsURL != "http://mirror.example/d/checksums.txt" {
			t.Errorf("sums URL = %q", rel.SumsURL)
		}
	})

	t.Run("token path resolves asset ids", func(t *testing.T) {
		t.Parallel()
		name := archiveName("v1.0.0", "darwin", "arm64")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/repos/"+Repo+"/releases/tags/v1.0.0" {
				t.Errorf("unexpected path %s", r.URL.Path)
			}
			fmt.Fprintf(w, `{"tag_name":"v1.0.0","assets":[{"id":11,"name":%q},{"id":22,"name":"checksums.txt"}]}`, name)
		}))
		defer srv.Close()
		rel, err := ResolveRelease(context.Background(), &Options{APIBase: srv.URL, Token: "tok", Version: "v1.0.0", OS: "darwin", Arch: "arm64"})
		if err != nil {
			t.Fatalf("ResolveRelease: %v", err)
		}
		if !rel.ViaAsset {
			t.Errorf("token path should be viaAsset")
		}
		if rel.ArchiveURL != srv.URL+"/repos/"+Repo+"/releases/assets/11" {
			t.Errorf("archive URL = %q", rel.ArchiveURL)
		}
		if rel.SumsURL != srv.URL+"/repos/"+Repo+"/releases/assets/22" {
			t.Errorf("sums URL = %q", rel.SumsURL)
		}
	})

	t.Run("token path errors when the archive asset is missing", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"tag_name":"v1.0.0","assets":[{"id":22,"name":"checksums.txt"}]}`)
		}))
		defer srv.Close()
		_, err := ResolveRelease(context.Background(), &Options{APIBase: srv.URL, Token: "tok", Version: "v1.0.0", OS: "darwin", Arch: "arm64"})
		if err == nil {
			t.Fatal("expected error for missing archive asset")
		}
	})

	t.Run("auth failure surfaces an access hint", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
		}))
		defer srv.Close()
		_, err := ResolveRelease(context.Background(), &Options{APIBase: srv.URL, Channel: ChannelStable, OS: "darwin", Arch: "arm64"})
		if err == nil || !strings.Contains(err.Error(), "token valid") {
			t.Fatalf("error = %v, want a token/access hint", err)
		}
	})
}
