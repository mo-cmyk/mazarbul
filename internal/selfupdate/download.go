package selfupdate

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
)

// Download fetches the release archive and its checksums.txt into destDir and
// returns their paths. It dispatches on how the release was resolved: a file://
// mirror is read directly; a private asset endpoint carries the bearer token
// (dropped on the cross-host redirect to signed storage by the client's
// CheckRedirect); the public CDN is a plain GET. Nothing is verified here, the
// caller runs VerifyChecksum before placing anything.
func Download(ctx context.Context, opts *Options, rel Release, destDir string) (archivePath, sumsPath string, err error) {
	archivePath = filepath.Join(destDir, rel.ArchiveName)
	sumsPath = filepath.Join(destDir, "checksums.txt")
	if derr := opts.fetchToFile(ctx, rel.ArchiveURL, archivePath, rel.ViaAsset, archiveBodyCap); derr != nil {
		return "", "", fmt.Errorf("selfupdate: download archive: %w", derr)
	}
	// checksums.txt is a few dozen bytes; cap it small so a hostile server cannot
	// stream a huge body in its place (the archive cap is far too generous here).
	if derr := opts.fetchToFile(ctx, rel.SumsURL, sumsPath, rel.ViaAsset, jsonBodyCap); derr != nil {
		return "", "", fmt.Errorf("selfupdate: download checksums: %w", derr)
	}
	return archivePath, sumsPath, nil
}

// fetchToFile writes the contents of rawURL to dest, failing if the body exceeds
// maxBytes. A file:// URL is copied from disk (Go's transport does not handle
// file://). An asset fetch sends the octet-stream Accept and bearer token; all
// other HTTP fetches are anonymous.
func (o *Options) fetchToFile(ctx context.Context, rawURL, dest string, viaAsset bool, maxBytes int64) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse url %q: %w", rawURL, err)
	}
	if u.Scheme == "file" {
		return copyLocalFile(u.Path, dest, maxBytes)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if viaAsset {
		req.Header.Set("Accept", acceptOctet)
		req.Header.Set("X-Github-Api-Version", apiVersionHeader)
		if o.Token != "" {
			req.Header.Set("Authorization", "Bearer "+o.Token)
		}
	}
	resp, err := o.httpClient(downloadTimeout).Do(req)
	if err != nil {
		return fmt.Errorf("request %s: %w", rawURL, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only response body
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, jsonBodyCap)) //nolint:errcheck // best-effort error detail
		return fmt.Errorf("GET %s: status %d: %s", rawURL, resp.StatusCode, trim(body))
	}
	return writeCapped(dest, resp.Body, maxBytes)
}

// copyLocalFile copies a file:// mirror source to dest. The source path is
// operator-supplied (MAZ_BASE_URL), so it is trusted input.
func copyLocalFile(src, dest string, maxBytes int64) error {
	f, err := os.Open(src) // #nosec G304 -- src is an operator-supplied file:// mirror path
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer f.Close()
	return writeCapped(dest, f, maxBytes)
}

// writeCapped streams r into dest, failing if the body exceeds maxBytes so a
// misbehaving server cannot fill the disk. It reads one byte past the cap to
// distinguish "exactly at cap" from "over cap". The file is created 0600: it may
// be replaced into the install, and is never meant to be world-readable.
func writeCapped(dest string, r io.Reader, maxBytes int64) error {
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 -- dest is under a caller-created temp dir
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	n, err := io.Copy(f, io.LimitReader(r, maxBytes+1))
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", dest, err)
	}
	if n > maxBytes {
		_ = f.Close()
		_ = os.Remove(dest) //nolint:errcheck // best-effort cleanup of the oversize partial
		return fmt.Errorf("download exceeds %d bytes", maxBytes)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dest, err)
	}
	return nil
}
