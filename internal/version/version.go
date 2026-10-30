// Package version exposes build metadata injected via ldflags.
package version

// Build metadata, injected at link time via
// -ldflags '-X github.com/mo-cmyk/mazarbul/internal/version.<Sym>=...'.
// The CI release workflow (.github/workflows/release.yml) sets all three;
// plain `go build` leaves the defaults below (unreleased build).
var (
	// Version is the release version (e.g. "v0.3.0"). Defaults to "dev".
	Version = "dev"
	// Commit is the short git SHA the binary was built from.
	Commit = "none"
	// Date is the RFC3339 build timestamp.
	Date = "unknown"
)
