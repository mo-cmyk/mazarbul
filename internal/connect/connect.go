package connect

import (
	"context"
	"errors"
	"fmt"
)

// Service orchestrates the registered Client adapters. The CLI, the
// init wizard's Connect step, and the Settings · Integrations panel
// all share a single Service instance.
type Service struct {
	clients []Client
	dataDir string
}

// NewService constructs a Service with the given clients + the
// resolved data directory (passed to MAZ_DATA_DIR when it differs
// from default).
func NewService(dataDir string, cs ...Client) *Service {
	return &Service{clients: cs, dataDir: dataDir}
}

// Clients returns the registered adapters in registration order.
func (s *Service) Clients() []Client { return s.clients }

// Find returns the adapter with the given ID and true, or zero+false
// if not registered. Returning the (Client, bool) tuple sidesteps the
// ireturn linter while keeping the call site ergonomic.
func (s *Service) Find(id string) (Client, bool) { //nolint:ireturn // factory shape is intentional.
	for _, c := range s.clients {
		if c.ID() == id {
			return c, true
		}
	}
	return nil, false
}

// DetectAll runs Detect on every registered adapter concurrently is
// not yet needed — clients each touch one file, so serial detection
// stays under 50 ms even with seven adapters.
func (s *Service) DetectAll(ctx context.Context) []DetectionResult {
	out := make([]DetectionResult, 0, len(s.clients))
	for _, c := range s.clients {
		st, err := c.Detect(ctx, s.dataDir)
		out = append(out, DetectionResult{
			Client: c, Status: st, Err: err,
		})
	}
	return out
}

// DetectionResult is one row of DetectAll's table.
type DetectionResult struct {
	Client Client
	Status Status
	Err    error
}

// Apply registers Mazarbul on a single client. Returns the writer's
// report regardless of error so the CLI can render a row per attempt.
func (s *Service) Apply(ctx context.Context, id string, scope Scope, projectRoot string, opts ApplyOptions) (WriteReport, error) {
	c, ok := s.Find(id)
	if !ok {
		return WriteReport{}, fmt.Errorf("unknown client %q", id)
	}
	if !scopeSupported(c, scope) {
		return WriteReport{ClientID: id, Scope: scope}, fmt.Errorf("client %s does not support scope %s", id, scope)
	}
	if opts.Capture || opts.WarmStart {
		// The generic adapter satisfies HookConfigurer for every client, so the
		// real gate is SupportsHooks (true only when the descriptor declares a
		// HookSpec). A bare client that doesn't implement the interface at all
		// (e.g. a test stub) is rejected too.
		if hc, ok := c.(HookConfigurer); !ok || !hc.SupportsHooks() {
			return WriteReport{ClientID: id, Scope: scope},
				fmt.Errorf("client %s does not support lifecycle hooks (--capture/--warm-start)", id)
		}
	}
	entry, err := ResolveEntry(s.dataDir)
	if err != nil {
		return WriteReport{ClientID: id, Scope: scope}, err
	}
	return c.Apply(ctx, scope, projectRoot, entry, opts)
}

// Uninstall removes Mazarbul from a single client.
func (s *Service) Uninstall(ctx context.Context, id string, scope Scope, projectRoot string) (WriteReport, error) {
	c, ok := s.Find(id)
	if !ok {
		return WriteReport{}, fmt.Errorf("unknown client %q", id)
	}
	if !scopeSupported(c, scope) {
		return WriteReport{ClientID: id, Scope: scope}, fmt.Errorf("client %s does not support scope %s", id, scope)
	}
	return c.Uninstall(ctx, scope, projectRoot)
}

func scopeSupported(c Client, scope Scope) bool {
	for _, s := range c.SupportedScopes() {
		if s == scope {
			return true
		}
	}
	return false
}

// ErrUnknownClient is returned by Find when the requested client is
// not registered. Exported so callers can distinguish missing-adapter
// from real Apply/Uninstall failures.
var ErrUnknownClient = errors.New("connect: unknown client")
