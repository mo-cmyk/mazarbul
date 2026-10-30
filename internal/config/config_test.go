package config

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestDefaultDataDir_EnvOverrideWins(t *testing.T) {
	t.Setenv("MAZ_DATA_DIR", "/explicit/override")
	t.Setenv("XDG_DATA_HOME", "/xdg/data")
	t.Setenv("HOME", "/tmp/home")

	got := defaultDataDir()
	if got != "/explicit/override" {
		t.Errorf("defaultDataDir() = %q, want /explicit/override", got)
	}
}

func TestDefaultDataDir_XDGWinsOverHome(t *testing.T) {
	t.Setenv("MAZ_DATA_DIR", "")
	t.Setenv("XDG_DATA_HOME", "/xdg/data")
	t.Setenv("HOME", "/tmp/home")

	got := defaultDataDir()
	want := "/xdg/data/mazarbul"
	if got != want {
		t.Errorf("defaultDataDir() = %q, want %q", got, want)
	}
}

func TestDefaultDataDir_PlatformDefault(t *testing.T) {
	t.Setenv("MAZ_DATA_DIR", "")
	t.Setenv("XDG_DATA_HOME", "")
	const home = "/tmp/home"
	t.Setenv("HOME", home)

	got := defaultDataDir()

	var want string
	switch runtime.GOOS {
	case "darwin":
		want = filepath.Join(home, "Library", "Application Support", "Mazarbul")
	default:
		want = filepath.Join(home, ".local", "share", "mazarbul")
	}
	if got != want {
		t.Errorf("defaultDataDir() on %s = %q, want %q", runtime.GOOS, got, want)
	}
}
