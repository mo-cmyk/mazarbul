//go:build !windows

package daemon

import (
	"slices"
	"strings"
	"testing"
)

// TestOllamaServeEnv_BindsConfiguredHost is the Greptile P1 regression
// gate: when the user sets [providers.ollama].base_url to a non-default
// endpoint, the spawned `ollama serve` child must BIND to that same
// endpoint via OLLAMA_HOST. Without this the supervisor would launch
// Ollama on the default :11434 while the embedder/chat client talks to
// the configured :11500 — the supervisor reports healthy but the
// provider keeps failing. The env writes the bare host:port form (no
// scheme), which is what `ollama serve` accepts for its bind address.
func TestOllamaServeEnv_BindsConfiguredHost(t *testing.T) {
	got := ollamaServeEnv([]string{"PATH=/usr/bin"}, 0, "http://127.0.0.1:11500")
	want := "OLLAMA_HOST=127.0.0.1:11500"
	found := false
	for _, kv := range got {
		if kv == want {
			found = true
		}
	}
	if !found {
		t.Errorf("env missing %q (managed Ollama must bind to configured base_url); got %v", want, got)
	}
}

// TestOllamaServeEnv_NoBindHostLeavesOllamaHostUnset asserts the
// symmetric case: empty bindHost means "no base_url configured", so
// the env must NOT carry an OLLAMA_HOST override and the user's shell
// value (if any) wins.
func TestOllamaServeEnv_NoBindHostLeavesOllamaHostUnset(t *testing.T) {
	got := ollamaServeEnv([]string{"PATH=/usr/bin"}, 0, "")
	for _, kv := range got {
		if strings.HasPrefix(kv, "OLLAMA_HOST=") {
			t.Errorf("env injected %q with no bindHost; should leave user's shell value alone; got %v", kv, got)
		}
	}
}

// TestOllamaServeEnv_AlwaysAppliesF1Caps pins the freeze-fix invariant:
// regardless of the configured num_parallel value (including zero or
// negative, which mean "use default"), the child's env carries the
// resource caps. Without them a single supervised `ollama serve` will
// pin multi-GB models in unified memory, trigger macOS swap, and freeze
// the host. The model cap is 2 — exactly the default dual-model stack
// (one chat + one embed, ~6 GB resident with qwen2.5:7b + arctic-embed2),
// still a hard memory bound rather than unbounded, and required so the two
// models co-reside instead of thrashing on every embed↔chat switch.
// NUM_PARALLEL stays user-tunable above 1.
func TestOllamaServeEnv_AlwaysAppliesF1Caps(t *testing.T) {
	for _, np := range []int{-1, 0, 1, 2} {
		got := ollamaServeEnv([]string{"PATH=/usr/bin", "HOME=/x"}, np, "")
		for _, want := range []string{
			"OLLAMA_MAX_LOADED_MODELS=2",
			"OLLAMA_KEEP_ALIVE=30s",
			"OLLAMA_FLASH_ATTENTION=1",
		} {
			if !slices.Contains(got, want) {
				t.Errorf("num_parallel=%d: env missing %q; got %v", np, want, got)
			}
		}
	}
}

// TestOllamaServeEnv_AppendsParallelWhenSet asserts the OLLAMA_NUM_PARALLEL
// override is appended last so any prior value in the parent env loses.
// Go's exec uses the last value when a key repeats; appending guarantees
// the config value wins.
func TestOllamaServeEnv_AppendsParallelWhenSet(t *testing.T) {
	parent := []string{"PATH=/usr/bin", "HOME=/x"}
	got := ollamaServeEnv(parent, 2, "")
	wantTail := "OLLAMA_NUM_PARALLEL=2"
	if got[len(got)-1] != wantTail {
		t.Errorf("got[-1] = %q, want %q (override must be last so duplicates lose)",
			got[len(got)-1], wantTail)
	}
	for _, kv := range parent {
		if !slices.Contains(got, kv) {
			t.Errorf("parent env entry %q missing from result", kv)
		}
	}
}

// TestOllamaServeEnv_DefaultsParallelToOne asserts that when num_parallel
// is unset (0) or negative, the env still ends with OLLAMA_NUM_PARALLEL=1
// — the F1 cap. Users who want more parallelism opt in via the config
// field; the supervisor refuses to be silent about its default.
func TestOllamaServeEnv_DefaultsParallelToOne(t *testing.T) {
	for _, np := range []int{-1, 0, 1} {
		got := ollamaServeEnv([]string{"PATH=/usr/bin"}, np, "")
		last := got[len(got)-1]
		if last != "OLLAMA_NUM_PARALLEL=1" {
			t.Errorf("num_parallel=%d: last entry = %q, want OLLAMA_NUM_PARALLEL=1",
				np, last)
		}
	}
}

// TestOllamaServeEnv_DuplicateParentParallelLoses pins the "appended last
// wins" rule. If the user set OLLAMA_NUM_PARALLEL=8 in their shell and
// also config.num_parallel=2, the child must run with 2. exec.Cmd uses
// the last occurrence of a key when it dedupes; appending the override
// after copying the parent guarantees the config value wins.
func TestOllamaServeEnv_DuplicateParentParallelLoses(t *testing.T) {
	parent := []string{"OLLAMA_NUM_PARALLEL=8", "PATH=/usr/bin"}
	got := ollamaServeEnv(parent, 2, "")
	last := got[len(got)-1]
	if last != "OLLAMA_NUM_PARALLEL=2" {
		t.Errorf("last entry = %q, want %q (config override must shadow shell env)",
			last, "OLLAMA_NUM_PARALLEL=2")
	}
}
