package document

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// CleanVaultPath normalises a caller-supplied document path to the canonical
// vault-relative POSIX form used as the documents-table key, and rejects
// anything that could escape the vault root when the projector later joins it
// onto the vault directory. A missing extension defaults to .md so agents can
// address notes by name ("ideas/inbox" → "ideas/inbox.md").
func CleanVaultPath(p string) (string, error) {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" {
		return "", errors.New("document path is empty")
	}
	if path.IsAbs(p) {
		return "", fmt.Errorf("document path %q must be vault-relative, not absolute", p)
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("document path %q escapes the vault root", p)
	}
	if path.Ext(clean) == "" {
		clean += ".md"
	}
	if !isMarkdownExt(path.Ext(clean)) {
		return "", fmt.Errorf("document path %q: only markdown documents (.md, .markdown) are supported", p)
	}
	return clean, nil
}
