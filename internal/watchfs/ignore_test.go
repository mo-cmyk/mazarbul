package watchfs

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

func TestIgnoreFile(t *testing.T) {
	root := t.TempDir()
	// Seed a .gitignore at the root: ignore *.log and the secret/ dir.
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("*.log\nsecret/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ig := newIgnorer([]string{root}, []string{"*.tmp"}, zap.NewNop())

	cases := []struct {
		name string
		rel  string
		want bool
	}{
		{"plain source file kept", "main.go", false},
		{"nested source file kept", "pkg/util.go", false},
		{"denylisted git dir component", ".git/config", true},
		{"denylisted node_modules component", "node_modules/left-pad/index.js", true},
		{"binary extension skipped", "logo.png", true},
		{"gitignore glob match", "server.log", true},
		{"gitignore dir match", "secret/key.txt", true},
		{"extra pattern match", "scratch.tmp", true},
		{"outside root", "../elsewhere.go", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(root, tc.rel)
			if tc.rel == "../elsewhere.go" {
				path = filepath.Join(filepath.Dir(root), "elsewhere.go")
			}
			if got := ig.ignoreFile(path); got != tc.want {
				t.Errorf("ignoreFile(%q) = %v, want %v", tc.rel, got, tc.want)
			}
		})
	}
}

func TestIgnoreDir(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("generated/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ig := newIgnorer([]string{root}, nil, zap.NewNop())

	cases := []struct {
		name string
		rel  string
		want bool
	}{
		{"root itself kept", ".", false},
		{"ordinary subdir kept", "internal", false},
		{"denylisted dir name", "node_modules", true},
		{"git metadata dir", ".git", true},
		{"gitignored dir", "generated", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := root
			if tc.rel != "." {
				path = filepath.Join(root, tc.rel)
			}
			if got := ig.ignoreDir(path); got != tc.want {
				t.Errorf("ignoreDir(%q) = %v, want %v", tc.rel, got, tc.want)
			}
		})
	}
}

func TestIgnoreFileNestedGitignore(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("*.log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(root, "pkg")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, ".gitignore"), []byte("secret.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ig := newIgnorer([]string{root}, nil, zap.NewNop())

	cases := []struct {
		name string
		rel  string
		want bool
	}{
		{"root rule applies in subdir", "pkg/app.log", true},
		{"nested rule ignores its file", "pkg/secret.txt", true},
		{"nested rule scoped to its dir", "secret.txt", false},
		{"unmatched nested file kept", "pkg/app.go", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ig.ignoreFile(filepath.Join(root, tc.rel)); got != tc.want {
				t.Errorf("ignoreFile(%q) = %v, want %v", tc.rel, got, tc.want)
			}
		})
	}
}

func TestIgnoreDirNestedGitignore(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "pkg")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, ".gitignore"), []byte("gen/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ig := newIgnorer([]string{root}, nil, zap.NewNop())

	if !ig.ignoreDir(filepath.Join(pkg, "gen")) {
		t.Error("nested .gitignore rule 'gen/' should ignore pkg/gen")
	}
	if ig.ignoreDir(filepath.Join(root, "gen")) {
		t.Error("root/gen should not be ignored: the rule is scoped to pkg/")
	}
}

func TestIgnoreDirRootNotAffectedByAncestorGitignore(t *testing.T) {
	parent := t.TempDir()
	// An ancestor .gitignore (above the watched tree) that would match the
	// root's basename must not cause the watcher to skip the root itself.
	if err := os.WriteFile(filepath.Join(parent, ".gitignore"), []byte("watched/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "watched")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	ig := newIgnorer([]string{root}, nil, zap.NewNop())
	if ig.ignoreDir(root) {
		t.Error("watched root must not be ignored by an ancestor .gitignore above the tree")
	}
}

func TestIsProbablyText(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"empty is text", nil, true},
		{"ascii is text", []byte("hello world"), true},
		{"utf8 is text", []byte("héllo wörld 日本語"), true},
		{"nul byte is binary", []byte("ab\x00cd"), false},
		{"truncated trailing rune still text", append([]byte("café"), []byte("日")[:2]...), true},
		{"random invalid utf8 is binary", []byte{0xff, 0xfe, 0xfd, 0xfc, 0xfb}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isProbablyText(tc.in); got != tc.want {
				t.Errorf("isProbablyText(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestRootFor(t *testing.T) {
	roots := []string{"/data/proja", "/data/projb"}
	cases := []struct {
		name     string
		path     string
		wantRoot string
		wantRel  string
		wantOK   bool
	}{
		{"file in first root", "/data/proja/cmd/main.go", "/data/proja", "cmd/main.go", true},
		{"file in second root", "/data/projb/x.go", "/data/projb", "x.go", true},
		{"root itself", "/data/proja", "/data/proja", ".", true},
		{"outside all roots", "/data/other/x.go", "", "", false},
		{"sibling prefix not matched", "/data/projan/x.go", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, rel, ok := rootFor(roots, tc.path)
			if root != tc.wantRoot || rel != tc.wantRel || ok != tc.wantOK {
				t.Errorf("rootFor(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.path, root, rel, ok, tc.wantRoot, tc.wantRel, tc.wantOK)
			}
		})
	}
}
