package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/ingest"
	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/secret"
)

// sourceIngestInputSchema is the shared argument schema for connectSource and
// ingestSource. Project scope is deliberately absent from the enum: ingested
// external content is sensitive (tagged connector:<name>) and project scope is
// plaintext/public, so it is refused.
const sourceIngestInputSchema = `{
  "type": "object",
  "properties": {
    "connector":          {"type": "string", "minLength": 1, "description": "Source connector name. Supported: local-git, claude-chats, granola."},
    "repoPath":           {"type": "string", "minLength": 1, "description": "Source locator for file-based connectors: a local git repository path (local-git), or an export file path (claude-chats: a .zip bundle or a conversations.json). Omit for granola, which authenticates with a keychain API key (set it via 'maz source add granola')."},
    "scope":              {"type": "string", "enum": ["personal", "team"], "description": "Scope to ingest into. Defaults to personal. Project scope is refused: ingested external content is sensitive and project is plaintext."},
    "dryRun":             {"type": "boolean", "description": "Preview only: validate the source and count would-be items WITHOUT writing anything (the consent gate)."},
    "maxItems":           {"type": "integer", "minimum": 1, "description": "Cap on items considered this run (backfill bound). Defaults to 500."},
    "maxExtractionCalls": {"type": "integer", "minimum": 1, "description": "Cap on episodes appended this run, each one downstream extraction. Defaults to 500."}
  },
  "required": ["connector"],
  "additionalProperties": false
}`

const sourceIngestOutputSchema = `{
  "type": "object",
  "properties": {
    "connector":        {"type": "string"},
    "jobUid":           {"type": "string", "description": "Job UID; pass to listJobs / cancelJob to track or cancel a long backfill."},
    "dryRun":           {"type": "boolean"},
    "itemsApplied":     {"type": "integer", "description": "Items turned into episodes (or, in a dry run, that WOULD be)."},
    "itemsSkipped":     {"type": "integer", "description": "Items skipped because their content hash was unchanged (idempotent re-ingest)."},
    "episodesAppended": {"type": "integer"},
    "streams": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "stream":   {"type": "string"},
          "sequence": {"type": "integer"},
          "applied":  {"type": "integer"},
          "skipped":  {"type": "integer"},
          "err":      {"type": "string", "description": "Set when a stream was soft-skipped (e.g. PR fetch failed) without failing the job."}
        }
      }
    }
  },
  "required": ["connector", "itemsApplied", "itemsSkipped", "episodesAppended"],
  "additionalProperties": true
}`

type sourceIngestInput struct {
	Connector          string `json:"connector"`
	RepoPath           string `json:"repoPath"`
	Scope              string `json:"scope,omitempty"`
	DryRun             bool   `json:"dryRun,omitempty"`
	MaxItems           int    `json:"maxItems,omitempty"`
	MaxExtractionCalls int    `json:"maxExtractionCalls,omitempty"`
}

// ScopePlaintextGuard reports whether scope is configured to sync as plaintext
// (a remote is bound to it and encryption is off). External-source content is
// tagged connector:<name> and is sensitive, so the #204 leak gate refuses to
// seal it into a plaintext chunk — but that refusal fires at seal time, leaving
// the events stuck pending. Checking up front turns that silent stall into a
// clean, actionable refusal at connect/ingest. A nil guard disables the check
// (tests and daemon-less paths that do not sync).
type ScopePlaintextGuard func(scope kernel.Scope) bool

// sourceIngestor is the shared engine behind connectSource and ingestSource. The
// two tools differ only in name and description (the onboarding vs delta mental
// model); both build the source from the registry and drive Orchestrator.Run,
// which resumes from the persisted watermark and is idempotent.
type sourceIngestor struct {
	orch           *ingest.Orchestrator
	secrets        secret.Provider
	plaintextGuard ScopePlaintextGuard
	logger         *zap.Logger
}

func (s *sourceIngestor) run(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	var in sourceIngestInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewValidationError(fmt.Sprintf("decode arguments: %v", err), nil)
	}
	if in.Connector == "" {
		return nil, mcp.NewValidationError("connector is required", nil)
	}
	// repoPath is validated per-connector by the source builder below: local-git
	// and claude-chats require it (a repo path / an export path), granola does not
	// (it authenticates with a keychain API key).
	scope, serr := parseIngestScope(in.Scope)
	if serr != nil {
		return nil, serr
	}
	if s.plaintextGuard != nil && s.plaintextGuard(scope) {
		return nil, mcp.NewValidationError(fmt.Sprintf(
			"scope %q syncs as plaintext: external-source content is sensitive and would be refused when sealed into a chunk. Enable encryption for this scope before ingesting (see `maz doctor`).", scope), nil)
	}

	src, err := ingest.NewSource(ctx, in.Connector, ingest.Config{
		RepoPath: in.RepoPath,
		Secrets:  s.secrets,
	})
	if err != nil {
		return nil, mcp.NewValidationError(err.Error(), nil)
	}
	// Pre-validate so a bad repo path is a clean E_VALIDATION rather than an
	// E_INTERNAL surfaced from Run. Run validates again (cheap git rev-parse).
	if verr := src.Validate(ctx); verr != nil {
		return nil, mcp.NewValidationError(verr.Error(), nil)
	}

	report, rerr := s.orch.Run(ctx, src, scope, ingest.Bounds{
		MaxItems:           in.MaxItems,
		MaxExtractionCalls: in.MaxExtractionCalls,
	}, in.DryRun)
	if rerr != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("ingest run: %w", rerr))
	}

	body, merr := json.Marshal(report)
	if merr != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal report: %w", merr))
	}
	return body, nil
}

// parseIngestScope validates the optional scope for ingest. Empty defaults to
// personal. Project scope is refused (the leak boundary); team is allowed.
func parseIngestScope(raw string) (kernel.Scope, error) {
	switch raw {
	case "":
		return kernel.ScopePersonal, nil
	case string(kernel.ScopePersonal), string(kernel.ScopeTeam):
		return kernel.Scope(raw), nil
	case string(kernel.ScopeProject):
		return "", mcp.NewValidationError(
			"scope 'project' is refused for ingest: ingested external content is sensitive and project scope is plaintext/public; use personal or team", nil)
	default:
		return "", mcp.NewValidationError(
			fmt.Sprintf("invalid scope %q (allowed: personal, team)", raw), nil)
	}
}

// ConnectSource is the onboarding verb: point at a local source and run the
// first bounded backfill. Functionally it is one Orchestrator.Run; dryRun gives
// the consent preview before any write.
type ConnectSource struct{ *sourceIngestor }

// NewConnectSource builds the connectSource tool. Guard may be nil to disable
// the up-front plaintext-scope refusal.
func NewConnectSource(orch *ingest.Orchestrator, secrets secret.Provider, guard ScopePlaintextGuard, logger *zap.Logger) *ConnectSource {
	return &ConnectSource{&sourceIngestor{orch: orch, secrets: secrets, plaintextGuard: guard, logger: logger}}
}

// Name implements mcp.Tool.
func (*ConnectSource) Name() string { return "connectSource" }

// Description implements mcp.Tool.
func (*ConnectSource) Description() string {
	return "Connect a source and run a bounded first backfill into the graph. Give a connector (local-git, claude-chats, or granola) and, for the file-based ones, repoPath (granola uses a stored API key instead); optionally scope (personal|team, default personal), dryRun (preview + count without writing), maxItems, and maxExtractionCalls. Ingested content is UNTRUSTED: facts land proposed and capped, never superseding your own. Returns a run report with itemsApplied/Skipped and a jobUid. Refuses project scope."
}

// InputSchema implements mcp.Tool.
func (*ConnectSource) InputSchema() json.RawMessage { return json.RawMessage(sourceIngestInputSchema) }

// OutputSchema implements mcp.Tool.
func (*ConnectSource) OutputSchema() json.RawMessage {
	return json.RawMessage(sourceIngestOutputSchema)
}

// Invoke implements mcp.Tool.
func (c *ConnectSource) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	return c.run(ctx, req)
}

// SelfTiming implements mcp.SelfTimingTool: connect runs the same pull engine
// as ingestSource (it does the first delta pull), so it owns its own time
// budget rather than being truncated by the adapter's default deadline.
func (*ConnectSource) SelfTiming() bool { return true }

// IngestSource is the recurring verb: pull the delta since the last run from the
// persisted watermark. Same inputs and engine as connectSource.
type IngestSource struct{ *sourceIngestor }

// NewIngestSource builds the ingestSource tool. Guard may be nil to disable the
// up-front plaintext-scope refusal.
func NewIngestSource(orch *ingest.Orchestrator, secrets secret.Provider, guard ScopePlaintextGuard, logger *zap.Logger) *IngestSource {
	return &IngestSource{&sourceIngestor{orch: orch, secrets: secrets, plaintextGuard: guard, logger: logger}}
}

// Name implements mcp.Tool.
func (*IngestSource) Name() string { return "ingestSource" }

// Description implements mcp.Tool.
func (*IngestSource) Description() string {
	return "Ingest the delta from a previously connected source, resuming from its saved watermark (cheap incremental pull). Give a connector (local-git, claude-chats, or granola) and, for the file-based ones, repoPath (granola uses a stored API key instead); optionally scope (personal|team), dryRun, maxItems, and maxExtractionCalls. Idempotent: unchanged items are skipped by content hash. Returns a run report with a jobUid. Refuses project scope."
}

// InputSchema implements mcp.Tool.
func (*IngestSource) InputSchema() json.RawMessage { return json.RawMessage(sourceIngestInputSchema) }

// OutputSchema implements mcp.Tool.
func (*IngestSource) OutputSchema() json.RawMessage { return json.RawMessage(sourceIngestOutputSchema) }

// Invoke implements mcp.Tool.
func (i *IngestSource) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	return i.run(ctx, req)
}

// SelfTiming implements mcp.SelfTimingTool: a delta pull runs for as long as
// the source legitimately takes (a large repo or chat export can outlast the
// adapter's default per-invocation deadline), so it owns its own time budget
// via maxItems/maxExtractionCalls rather than being truncated mid-ingest.
func (*IngestSource) SelfTiming() bool { return true }
