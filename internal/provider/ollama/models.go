package ollama

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// ErrModelMissing is returned (wrapped) by New when the configured embed
// model is not installed on the Ollama daemon. The wrapped error's message
// contains the exact `ollama pull <model>` command the user must run, so
// callers can surface the message verbatim:
//
//	if errors.Is(err, ollama.ErrModelMissing) {
//	    fmt.Println(err)
//	    os.Exit(1)
//	}
var ErrModelMissing = errors.New("ollama: embedding model not installed")

// ErrInvalidConfig is returned (wrapped) by New when the configured
// Options fail static validation BEFORE any network probe runs — most
// commonly a malformed BaseURL. The distinction matters for the daemon
// wiring layer: a transient probe failure (Ollama daemon down) is
// recoverable as degraded write-only mode, but a config error is the
// operator's fault and must surface as a hard daemon failure so the
// operator fixes config.toml rather than discovering recall is silently
// disabled.
//
//	if errors.Is(err, ollama.ErrInvalidConfig) {
//	    // Operator must edit config.toml; recall would silently disappear.
//	}
var ErrInvalidConfig = errors.New("ollama: invalid configuration")

// tagsResponse mirrors the JSON shape returned by Ollama's GET /api/tags.
// Only the fields the client consumes are decoded; everything else
// (modified_at, size, digest, details, ...) Is intentionally dropped.
type tagsResponse struct {
	Models []struct {
		Name  string `json:"name"`
		Model string `json:"model"`
	} `json:"models"`
}

// ListModels returns the names of every model installed on the Ollama
// daemon, in the order the daemon reports them. Empty list (no models
// installed) is a valid response, not an error.
//
// Consumed by:
//   - maz init   (#47): show installed models in the wizard.
//   - maz doctor (#52): report Ollama daemon health.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	var resp tagsResponse
	if err := c.doJSON(ctx, http.MethodGet, "/api/tags", nil, &resp); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(resp.Models))
	for _, m := range resp.Models {
		// Prefer Name (e.g. "bge-m3:latest"); some Ollama versions only
		// populate Model. Fall back so callers always get a usable string.
		switch {
		case m.Name != "":
			out = append(out, m.Name)
		case m.Model != "":
			out = append(out, m.Model)
		}
	}
	return out, nil
}

// modelInstalled reports whether any of installed match want.
//
// Match rules:
//   - Untagged want (e.g. "bge-m3") matches any installed tag of the same
//     model: "bge-m3", "bge-m3:latest", "bge-m3:f16" all qualify. This is
//     the common case — users configure the model name and let Ollama pick
//     the tag.
//   - Tagged want (e.g. "bge-m3:f16") requires an exact match. A configured
//     "bge-m3:f16" must not falsely accept an installed "bge-m3:latest";
//     Embed would then send the f16 tag to Ollama and fail late instead of
//     surfacing ErrModelMissing during the probe.
func modelInstalled(installed []string, want string) bool {
	wantHasTag := strings.Contains(want, ":")
	for _, name := range installed {
		if wantHasTag {
			if name == want {
				return true
			}
			continue
		}
		if stripTag(name) == want {
			return true
		}
	}
	return false
}

// stripTag returns name without the trailing `:tag` suffix, if any.
func stripTag(name string) string {
	if i := strings.IndexByte(name, ':'); i >= 0 {
		return name[:i]
	}
	return name
}
