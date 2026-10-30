package cli

// watch_test.go pins the no-daemon behaviour: `maz watch` must stream the
// daemon's real events when one is serving the data dir, and when none is it
// must say so and exit — never silently fabricate synthetic events (that flooded
// agents with fake data and hung on dead sockets). Synthetic data stays opt-in
// via --demo.

import (
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/config"
)

func TestWatchNoDaemonNotice(t *testing.T) {
	t.Run("explicit --demo never probes or warns", func(t *testing.T) {
		prev := probeDaemonListening
		t.Cleanup(func() { probeDaemonListening = prev })
		probed := false
		probeDaemonListening = func(string) bool { probed = true; return false }

		if notice := watchNoDaemonNotice("/some/dir", true); notice != "" {
			t.Errorf("notice = %q, want empty when --demo is set", notice)
		}
		if probed {
			t.Error("probeDaemonListening was called for --demo (should short-circuit)")
		}
	})

	t.Run("live daemon streams real events (no notice)", func(t *testing.T) {
		prev := probeDaemonListening
		t.Cleanup(func() { probeDaemonListening = prev })
		probeDaemonListening = func(string) bool { return true }

		if notice := watchNoDaemonNotice("/some/dir", false); notice != "" {
			t.Errorf("notice = %q, want empty when a daemon is listening", notice)
		}
	})

	t.Run("no daemon yields an actionable notice (not fake data)", func(t *testing.T) {
		prev := probeDaemonListening
		t.Cleanup(func() { probeDaemonListening = prev })
		probeDaemonListening = func(string) bool { return false }

		notice := watchNoDaemonNotice("/some/dir", false)
		if notice == "" {
			t.Fatal("notice = empty, want guidance when no daemon serves the dir")
		}
		for _, want := range []string{"/some/dir", "maz daemon start", "--demo"} {
			if !strings.Contains(notice, want) {
				t.Errorf("notice %q missing %q", notice, want)
			}
		}
	})
}

func TestFormatProvidersLine(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.UserConfig
		want string
	}{
		{
			name: "ollama chat and embed with distinct models",
			cfg: config.UserConfig{
				Defaults: config.DefaultsTable{Chat: "ollama", Embed: "ollama"},
				Providers: map[string]config.ProviderTable{
					"ollama": {ChatModel: "qwen2.5:7b", EmbedModel: "snowflake-arctic-embed2"},
				},
			},
			want: "chat ollama qwen2.5:7b · embed ollama snowflake-arctic-embed2",
		},
		{
			name: "anthropic single model slot for chat",
			cfg: config.UserConfig{
				Defaults: config.DefaultsTable{Chat: "anthropic", Embed: "ollama"},
				Providers: map[string]config.ProviderTable{
					"anthropic": {Model: "claude-opus-4-7"},
					"ollama":    {EmbedModel: "nomic-embed-text"},
				},
			},
			want: "chat anthropic claude-opus-4-7 · embed ollama nomic-embed-text",
		},
		{
			name: "embed disabled when default embed empty",
			cfg: config.UserConfig{
				Defaults:  config.DefaultsTable{Chat: "ollama"},
				Providers: map[string]config.ProviderTable{"ollama": {ChatModel: "qwen2.5:7b"}},
			},
			want: "chat ollama qwen2.5:7b · embed (disabled)",
		},
		{
			name: "provider absent from config falls back to bare name",
			cfg: config.UserConfig{
				Defaults: config.DefaultsTable{Chat: "ollama", Embed: "ollama"},
			},
			want: "chat ollama · embed ollama",
		},
		{
			name: "chat unconfigured when default chat empty",
			cfg:  config.UserConfig{},
			want: "chat (unconfigured) · embed (disabled)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatProvidersLine(&tt.cfg); got != tt.want {
				t.Errorf("formatProvidersLine() = %q, want %q", got, tt.want)
			}
		})
	}
}
