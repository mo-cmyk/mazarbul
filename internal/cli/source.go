package cli

// source.go implements the "maz source" command group — the CLI surface for the
// external-source provenance primitive (issue #207):
//
//	maz source ref <uid>   read a SourceRef by uid (getSourceRef)
//	maz source record ...  record a SourceRef (recordSourceRef)
//
// Both route through invokeNavTool, so a running daemon serves them over its MCP
// transport and a daemon-less invocation falls back to an in-process wiring —
// the same dispatch every navigation verb uses. Provenance read back is always
// labelled UNVERIFIED: a SourceRef is self-asserted and carries no
// cryptographic attestation yet.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/ingest"
	"github.com/mo-cmyk/mazarbul/internal/secret"
)

// newSourceCmd returns the "maz source" parent command.
func newSourceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "source",
		Short: "Record and read external-source provenance (SourceRefs)",
		Long: `A SourceRef records where a memory came from — the connector, stream,
external id/url, content hash, and fetch time a record was derived from
(W3C PROV wasDerivedFrom). Provenance is SELF-ASSERTED and UNVERIFIED: there
is no cryptographic attestation of the external claim yet.

Subcommands:
  add      Store the API key for a keyed connector (e.g. granola)
  revoke   Delete a connector's stored API key and verify it is gone
  connect  Connect a source and run a first backfill
  ingest   Pull the delta from a connected source
  import   Import an exported file from a connector (e.g. claude-chats)
  list     List connectors and per-source resume watermarks
  ref      Read a SourceRef by uid
  record   Record a SourceRef

Examples:
  maz source add granola
  maz source connect granola --max-items 50
  maz source connect local-git --repo . --max-items 50
  maz source ingest local-git --repo . --dry-run
  maz source import claude-chats ~/Downloads/claude-export.zip
  maz source list
  maz source revoke granola
  maz source record --subject 01ARZ3NDEKTSV4RRFFQ69G5FAV --source-type email --dedup-key gmail:msg-42
  maz source ref 01J9Z3NDEKTSV4RRFFQ69G5FAV`,
		// Bare `maz source` shows help; an unknown subcommand errors via NoArgs
		// rather than exiting 0 (a silent success that misleads scripts/agents).
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newSourceAddCmd())
	cmd.AddCommand(newSourceRevokeCmd())
	cmd.AddCommand(newSourceConnectCmd())
	cmd.AddCommand(newSourceIngestCmd())
	cmd.AddCommand(newSourceImportCmd())
	cmd.AddCommand(newSourceListCmd())
	cmd.AddCommand(newSourceRefCmd())
	cmd.AddCommand(newSourceRecordCmd())
	return cmd
}

// sourceIngestFlags holds the parsed flags shared by "connect" and "ingest".
type sourceIngestFlags struct {
	repo               string
	scope              string
	dryRun             bool
	maxItems           int
	maxExtractionCalls int
}

// buildIngestRequest assembles the connectSource/ingestSource argument body from
// a connector name and the shared flags.
func buildIngestRequest(connector string, f *sourceIngestFlags) (json.RawMessage, error) {
	in := map[string]any{
		"connector": connector,
	}
	// repoPath is the local locator for file-based connectors (local-git repo
	// path, claude-chats export path). Omit it when empty so a keyed connector
	// like granola, which needs no path, does not trip the schema's minLength.
	putIfSet(in, "repoPath", f.repo)
	putIfSet(in, "scope", f.scope)
	if f.dryRun {
		in["dryRun"] = true
	}
	if f.maxItems > 0 {
		in["maxItems"] = f.maxItems
	}
	if f.maxExtractionCalls > 0 {
		in["maxExtractionCalls"] = f.maxExtractionCalls
	}
	return json.Marshal(in)
}

// addIngestFlags registers the shared connect/ingest flags on cmd. --repo is
// not marked required: it is the source locator for file-based connectors
// (local-git, claude-chats) but unused by keyed connectors (granola), so the
// per-connector source builder validates it rather than cobra.
func addIngestFlags(cmd *cobra.Command, f *sourceIngestFlags) {
	cmd.Flags().StringVar(&f.repo, "repo", "", "source locator: local git repository path (local-git); unused by granola")
	cmd.Flags().StringVar(&f.scope, "scope", "", "scope to ingest into: personal or team (defaults to personal; project is refused)")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "preview: validate and count would-be items without writing")
	cmd.Flags().IntVar(&f.maxItems, "max-items", 0, "cap on items considered this run (backfill bound; default 500)")
	cmd.Flags().IntVar(&f.maxExtractionCalls, "max-extraction-calls", 0, "cap on episodes appended this run (default 500)")
}

// newSourceConnectCmd returns "maz source connect <connector>".
func newSourceConnectCmd() *cobra.Command {
	f := &sourceIngestFlags{}
	cmd := &cobra.Command{
		Use:   "connect <connector>",
		Short: "Connect a local source and run a bounded first backfill",
		Long: `Connect a local source (currently: local-git) and run a bounded backfill
into the graph. The existing extractor + resolver populate entities and relations
from the source's prose; getContext then returns a correlated brief on the first
run. Ingested content is UNTRUSTED — facts land proposed and capped, never
superseding your own — and is tagged so it can never leak into a plaintext scope.

Use --dry-run first to preview how many items would be ingested before writing.

Examples:
  maz source connect local-git --repo . --max-items 50
  maz source connect local-git --repo /path/to/repo --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reqBody, err := buildIngestRequest(args[0], f)
			if err != nil {
				return wrapNavFatal(cmd, "source connect", err)
			}
			raw, err := invokeNavTool(cmd, "connectSource", reqBody)
			if err != nil {
				return wrapNavFatal(cmd, "source connect", err)
			}
			if handled, herr := emitNavResult(cmd, raw); handled {
				return herr
			}
			return renderIngestReportCLI(cmd, "source connect", raw)
		},
	}
	addIngestFlags(cmd, f)
	return cmd
}

// newSourceIngestCmd returns "maz source ingest <connector>".
func newSourceIngestCmd() *cobra.Command {
	f := &sourceIngestFlags{}
	cmd := &cobra.Command{
		Use:   "ingest <connector>",
		Short: "Pull the delta from a connected source (incremental)",
		Long: `Ingest the delta from a previously connected source, resuming from its
saved watermark — a cheap incremental pull. Idempotent: unchanged items are
skipped by content hash, and a changed commit/PR body produces a new revision
without duplicating entities.

Examples:
  maz source ingest local-git --repo .
  maz source ingest local-git --repo . --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reqBody, err := buildIngestRequest(args[0], f)
			if err != nil {
				return wrapNavFatal(cmd, "source ingest", err)
			}
			raw, err := invokeNavTool(cmd, "ingestSource", reqBody)
			if err != nil {
				return wrapNavFatal(cmd, "source ingest", err)
			}
			if handled, herr := emitNavResult(cmd, raw); handled {
				return herr
			}
			return renderIngestReportCLI(cmd, "source ingest", raw)
		},
	}
	addIngestFlags(cmd, f)
	return cmd
}

// newSourceImportCmd returns "maz source import <connector> <path>" — the
// file-import verb for connectors backed by an exported file rather than a live
// API (currently: claude-chats). It routes through connectSource with the path
// as the source locator.
func newSourceImportCmd() *cobra.Command {
	f := &sourceIngestFlags{}
	cmd := &cobra.Command{
		Use:   "import <connector> <path>",
		Short: "Import an exported file from a connector (e.g. claude-chats)",
		Long: `Import a one-shot exported file into the graph. Currently: claude-chats —
the Claude account-data export (Settings -> Privacy -> Export data), either a .zip
bundle or a bare conversations.json. Re-importing the same export is a no-op
(turns are deduplicated by content hash); a newer export ingests only the new
turns. Ingested content is UNTRUSTED — facts land proposed and capped, never
superseding your own — and is refused into a plaintext scope.

Examples:
  maz source import claude-chats ~/Downloads/claude-export.zip
  maz source import claude-chats ./conversations.json --dry-run`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			f.repo = args[1]
			reqBody, err := buildIngestRequest(args[0], f)
			if err != nil {
				return wrapNavFatal(cmd, "source import", err)
			}
			raw, err := invokeNavTool(cmd, "connectSource", reqBody)
			if err != nil {
				return wrapNavFatal(cmd, "source import", err)
			}
			if handled, herr := emitNavResult(cmd, raw); handled {
				return herr
			}
			return renderIngestReportCLI(cmd, "source import", raw)
		},
	}
	cmd.Flags().StringVar(&f.scope, "scope", "", "scope to import into: personal or team (defaults to personal; project is refused)")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "preview: validate and count would-be items without writing")
	cmd.Flags().IntVar(&f.maxItems, "max-items", 0, "cap on items considered this run (default 500)")
	cmd.Flags().IntVar(&f.maxExtractionCalls, "max-extraction-calls", 0, "cap on episodes appended this run (default 500)")
	return cmd
}

// connectorSecretName maps a connector to the canonical secret name holding its
// API key and reports whether the connector is keyed at all. Local-git and
// claude-chats authenticate with no stored credential and return ok=false.
func connectorSecretName(connector string) (string, bool) {
	switch connector {
	case "granola":
		return secret.NameGranolaAPIKey, true
	default:
		return "", false
	}
}

// staticSecret answers a single connector key lookup with an in-hand value so
// `maz source add` can validate a key against the live API before it is
// persisted. Any other name is ErrNotFound. It satisfies ingest.SecretReader.
type staticSecret struct {
	name  string
	value string
}

func (s staticSecret) Get(_ context.Context, name string) (string, error) {
	if name == s.name {
		return s.value, nil
	}
	return "", secret.ErrNotFound
}

// sourceAddFlags holds the parsed flags for "maz source add".
type sourceAddFlags struct {
	apiKey   string
	stdin    bool
	noVerify bool
}

// newSourceAddCmd returns "maz source add <connector>" — credential onboarding
// for connectors that authenticate with a stored API key (currently: granola).
func newSourceAddCmd() *cobra.Command {
	f := &sourceAddFlags{}
	cmd := &cobra.Command{
		Use:   "add <connector>",
		Short: "Store the API key for a keyed connector (e.g. granola)",
		Long: `Store the API key a remote connector needs to authenticate, in the OS
keychain (macOS Keychain, Linux Secret Service, Windows Credential Manager) —
never in config, the event log, or logs. Currently: granola, which requires an
API key minted on a Granola Business/Enterprise plan.

The key is read from --api-key, from stdin (--stdin or a pipe), or from a
no-echo interactive prompt. By default the key is verified against the live API
before it is stored; pass --no-verify to store without a network call (offline
or CI).

Ingestion is a separate step (maz source connect/ingest granola). Ingesting into
a plaintext scope is refused there, so a stored key never implies a leak.

Examples:
  maz source add granola
  echo "grn_..." | maz source add granola --stdin
  maz source add granola --no-verify`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSourceAdd(cmd, args[0], f)
		},
	}
	cmd.Flags().StringVar(&f.apiKey, "api-key", "", "API key value (avoid in scripts; prefer --stdin)")
	cmd.Flags().BoolVar(&f.stdin, "stdin", false, "read the API key from stdin (trailing newline trimmed)")
	cmd.Flags().BoolVar(&f.noVerify, "no-verify", false, "skip the live key-validation probe before storing")
	return cmd
}

func runSourceAdd(cmd *cobra.Command, connector string, f *sourceAddFlags) error {
	secretName, ok := connectorSecretName(connector)
	if !ok {
		return wrapNavFatal(cmd, "source add", fmt.Errorf(
			"connector %q has no API key to store (local-git and claude-chats need none); known keyed connector: granola", connector))
	}
	key, err := readSecretValue(cmd, &secretSetFlags{value: f.apiKey, stdin: f.stdin})
	if err != nil {
		return wrapNavFatal(cmd, "source add", err)
	}
	if strings.TrimSpace(key) == "" {
		return wrapNavFatal(cmd, "source add", errors.New("empty API key rejected"))
	}
	if !f.noVerify {
		if verr := verifyConnectorKey(cmd, connector, secretName, key); verr != nil {
			return wrapNavFatal(cmd, "source add", fmt.Errorf("verify key (use --no-verify to skip): %w", verr))
		}
	}
	if err := secret.PersistentChain().Set(cmd.Context(), secretName, key); err != nil {
		return wrapNavFatal(cmd, "source add", fmt.Errorf("store %s: %w", secretName, err))
	}
	_, werr := fmt.Fprintf(cmd.OutOrStdout(), "stored %s in the keychain\n", secretName)
	return werr
}

// verifyConnectorKey builds the connector's Source with the in-hand key and runs
// its preflight probe, so a rejected key (or a missing entitlement) surfaces
// before the key is persisted rather than at first ingest.
func verifyConnectorKey(cmd *cobra.Command, connector, secretName, key string) error {
	src, err := ingest.NewSource(cmd.Context(), connector, ingest.Config{
		Secrets: staticSecret{name: secretName, value: key},
	})
	if err != nil {
		return err
	}
	return src.Validate(cmd.Context())
}

// newSourceRevokeCmd returns "maz source revoke <connector>" — delete a stored
// connector API key and prove it no longer resolves on the read path.
func newSourceRevokeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "revoke <connector>",
		Short: "Delete a connector's stored API key and verify it is gone",
		Long: `Delete the API key stored for a connector (currently: granola) from the
persistent keychain, then verify the key no longer resolves on the read path.

If an environment override (MAZ_SECRET_*) still shadows the deleted key, this
fails loudly and names the variable to unset: deleting the keychain entry alone
would leave the key live, because the environment layer wins on read.

Examples:
  maz source revoke granola`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSourceRevoke(cmd, args[0])
		},
	}
	return cmd
}

func runSourceRevoke(cmd *cobra.Command, connector string) error {
	secretName, ok := connectorSecretName(connector)
	if !ok {
		return wrapNavFatal(cmd, "source revoke", fmt.Errorf(
			"connector %q has no stored API key (local-git and claude-chats need none); known keyed connector: granola", connector))
	}
	ctx := cmd.Context()

	// Delete from the persistent store only (PersistentChain bypasses the
	// read-only env layer). A missing entry is not an error here: we still run
	// the read-path verification below, because an override may resolve the key
	// even with nothing in the keychain.
	deleteErr := secret.PersistentChain().Delete(ctx, secretName)
	if deleteErr != nil && !errors.Is(deleteErr, secret.ErrNotFound) {
		return wrapNavFatal(cmd, "source revoke", fmt.Errorf("delete %s: %w", secretName, deleteErr))
	}

	// Prove the key is gone on the READ path (env in front of keychain). A value
	// that still resolves means a MAZ_SECRET_* override shadows the deletion.
	_, gerr := secret.DefaultChain().Get(ctx, secretName)
	switch {
	case gerr == nil:
		envVar := secret.CanonicalEnvName(secretName)
		return wrapNavFatal(cmd, "source revoke", fmt.Errorf(
			"deleted the keychain entry but %s still resolves from the environment; unset %s to fully revoke", secretName, envVar))
	case !errors.Is(gerr, secret.ErrNotFound):
		return wrapNavFatal(cmd, "source revoke", fmt.Errorf("verify %s deleted: %w", secretName, gerr))
	}

	msg := "revoked " + secretName + " (verified absent)"
	if errors.Is(deleteErr, secret.ErrNotFound) {
		msg = "no stored key for " + connector + " (nothing to revoke; verified absent)"
	}
	_, werr := fmt.Fprintln(cmd.OutOrStdout(), msg)
	return werr
}

// newSourceListCmd returns "maz source list".
func newSourceListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List connectors and per-source resume watermarks with freshness",
		Long: `List the external source connectors available to ingest from and the
resume watermark of each source connected so far (connector, stream, items
ingested, freshness status, last advance time). Read-only.

The status column classifies each source: live (advanced within a day), stale
(over a day), or down (over a week, or never advanced). The JSON output adds
status and ageSeconds per watermark so scripts can alert on staleness; the same
classification powers the maz watch [5] sources pane and maz doctor.

Examples:
  maz source list
  maz source list --output json
  maz source list --output json | jq '.watermarks[] | select(.status != "live")'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			raw, err := invokeNavTool(cmd, "listSources", json.RawMessage(`{}`))
			if err != nil {
				return wrapNavFatal(cmd, "source list", err)
			}
			if handled, herr := emitNavResult(cmd, raw); handled {
				return herr
			}
			return renderSourceListCLI(cmd, raw)
		},
	}
	return cmd
}

// newSourceRefCmd returns "maz source ref <uid>".
func newSourceRefCmd() *cobra.Command {
	var scopes []string
	cmd := &cobra.Command{
		Use:   "ref <uid>",
		Short: "Fetch an external-source provenance record by uid",
		Long: `Fetch a SourceRef by uid (or its sourceRefs/{uid} resource name). The
response includes the sourceType, external id/url, content hash, fetch time, and
a trustState that always reads "unverified": the provenance is self-asserted and
not cryptographically attested.

Examples:
  maz source ref 01J9Z3NDEKTSV4RRFFQ69G5FAV
  maz source ref sourceRefs/01J9Z3NDEKTSV4RRFFQ69G5FAV --output json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateScopeFlag(cmd, "source ref", scopes); err != nil {
				return err
			}
			name := args[0]
			if !strings.Contains(name, "/") {
				name = "sourceRefs/" + name
			}
			in := map[string]any{"name": name}
			if len(scopes) > 0 {
				in["scopes"] = scopes
			}
			reqBody, err := json.Marshal(in)
			if err != nil {
				return wrapNavFatal(cmd, "source ref", err)
			}
			raw, err := invokeNavTool(cmd, "getSourceRef", reqBody)
			if err != nil {
				return wrapNavFatal(cmd, "source ref", err)
			}
			if handled, herr := emitNavResult(cmd, raw); handled {
				return herr
			}
			return renderSourceRefCLI(cmd, raw)
		},
	}
	cmd.Flags().StringArrayVar(&scopes, "scope", nil, "scope(s) to read from: project, personal, team (repeatable)")
	return cmd
}

// sourceRecordFlags holds the parsed flag values for "maz source record".
type sourceRecordFlags struct {
	subject     string
	sourceType  string
	dedupKey    string
	connector   string
	stream      string
	externalID  string
	externalURL string
	contentHash string
	fetchTime   string
	scope       string
}

// newSourceRecordCmd returns "maz source record".
func newSourceRecordCmd() *cobra.Command {
	f := &sourceRecordFlags{}
	cmd := &cobra.Command{
		Use:   "record",
		Short: "Record an external-source provenance reference (SourceRef)",
		Long: `Record where a memory came from. Give the subject's uid, a sourceType
(open taxonomy), and a dedupKey for idempotency; optionally a connector, stream,
external id/url, and content hash. Recording the same external item twice (same
--dedup-key) collapses to one SourceRef. Prints the new sourceRefs/{uid} name.

This records provenance only — it does not fetch or dereference the source, and
the recorded provenance is self-asserted (unverified).

Examples:
  maz source record --subject 01ARZ3NDEKTSV4RRFFQ69G5FAV --source-type email --dedup-key gmail:msg-42
  maz source record --subject 01ARZ... --source-type slack_message --connector slack \
    --stream C0123 --external-id 1700000000.0001 --dedup-key slack:C0123:1700000000.0001`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in := map[string]any{
				"subjectUid": f.subject,
				"sourceType": f.sourceType,
				"dedupKey":   f.dedupKey,
			}
			putIfSet(in, "connector", f.connector)
			putIfSet(in, "stream", f.stream)
			putIfSet(in, "externalId", f.externalID)
			putIfSet(in, "externalUrl", f.externalURL)
			putIfSet(in, "contentHash", f.contentHash)
			putIfSet(in, "fetchTime", f.fetchTime)
			putIfSet(in, "scope", f.scope)

			reqBody, err := json.Marshal(in)
			if err != nil {
				return wrapNavFatal(cmd, "source record", err)
			}
			raw, err := invokeNavTool(cmd, "recordSourceRef", reqBody)
			if err != nil {
				return wrapNavFatal(cmd, "source record", err)
			}
			if handled, herr := emitNavResult(cmd, raw); handled {
				return herr
			}
			return renderSourceRecordCLI(cmd, raw)
		},
	}
	cmd.Flags().StringVar(&f.subject, "subject", "", "uid of the record this source was derived from (required)")
	cmd.Flags().StringVar(&f.sourceType, "source-type", "", "kind of external source, e.g. email, slack_message (required)")
	cmd.Flags().StringVar(&f.dedupKey, "dedup-key", "", "idempotency key; re-recording the same key collapses to one SourceRef (required)")
	cmd.Flags().StringVar(&f.connector, "connector", "", "connector that produced the reference, e.g. gmail, slack")
	cmd.Flags().StringVar(&f.stream, "stream", "", "sub-channel within the connector, e.g. a mailbox label or channel id")
	cmd.Flags().StringVar(&f.externalID, "external-id", "", "source-system opaque identifier for the item")
	cmd.Flags().StringVar(&f.externalURL, "external-url", "", "dereferenceable location of the source item")
	cmd.Flags().StringVar(&f.contentHash, "content-hash", "", "SHA-256 hex of the fetched content")
	cmd.Flags().StringVar(&f.fetchTime, "fetch-time", "", "RFC 3339 instant the content was fetched (defaults to now)")
	cmd.Flags().StringVar(&f.scope, "scope", "", "scope to record into: personal, project, team (defaults to personal)")
	//nolint:errcheck // MarkFlagRequired only errors on an unknown flag name, which cannot happen for a flag declared one line above
	cmd.MarkFlagRequired("subject")
	//nolint:errcheck // see above
	cmd.MarkFlagRequired("source-type")
	//nolint:errcheck // see above
	cmd.MarkFlagRequired("dedup-key")
	return cmd
}

// putIfSet adds key=value to m only when value is non-empty, so omitted optional
// flags stay off the request body (and pick up the tool's defaults).
func putIfSet(m map[string]any, key, value string) {
	if value != "" {
		m[key] = value
	}
}

// sourceRefView is the subset of the getSourceRef response the text renderer
// prints. NDJSON/JSON modes pass the raw response through untouched.
type sourceRefView struct {
	Name        string `json:"name"`
	SubjectUID  string `json:"subjectUid"`
	SourceType  string `json:"sourceType"`
	Connector   string `json:"connector"`
	Stream      string `json:"stream"`
	ExternalID  string `json:"externalId"`
	ExternalURL string `json:"externalUrl"`
	ContentHash string `json:"contentHash"`
	FetchTime   string `json:"fetchTime"`
	DedupKey    string `json:"dedupKey"`
	Scope       string `json:"scope"`
	TrustState  string `json:"trustState"`
}

// renderSourceRefCLI prints a getSourceRef response. NDJSON mode emits the raw
// object on one line; text mode prints aligned field rows, leading with the
// UNVERIFIED provenance label so the trust caveat is unmissable.
func renderSourceRefCLI(cmd *cobra.Command, raw json.RawMessage) error {
	if output.From(cmd).Format() == output.FormatNDJSON {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return err
	}
	var v sourceRefView
	if err := json.Unmarshal(raw, &v); err != nil {
		return wrapNavFatal(cmd, "source ref", fmt.Errorf("parse response: %w", err))
	}
	w := cmd.OutOrStdout()
	//nolint:errcheck // best-effort stdout; tabwriter.Flush surfaces any real write error
	fmt.Fprintf(w, "provenance: UNVERIFIED (self-asserted, not signed)\n\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	rows := []struct{ k, v string }{
		{"name", v.Name},
		{"subjectUid", v.SubjectUID},
		{"sourceType", v.SourceType},
		{"connector", v.Connector},
		{"stream", v.Stream},
		{"externalId", v.ExternalID},
		{"externalUrl", v.ExternalURL},
		{"contentHash", v.ContentHash},
		{"fetchTime", v.FetchTime},
		{"dedupKey", v.DedupKey},
		{"scope", v.Scope},
	}
	for _, r := range rows {
		if r.v == "" {
			continue
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\n", r.k, r.v); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// renderSourceRecordCLI prints a recordSourceRef response. NDJSON mode emits the
// raw object on one line; text mode prints the new resource name.
func renderSourceRecordCLI(cmd *cobra.Command, raw json.RawMessage) error {
	if output.From(cmd).Format() == output.FormatNDJSON {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return err
	}
	var v struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return wrapNavFatal(cmd, "source record", fmt.Errorf("parse response: %w", err))
	}
	_, err := fmt.Fprintln(cmd.OutOrStdout(), v.Name)
	return err
}

// ingestStreamView is one stream's outcome in an ingest report.
type ingestStreamView struct {
	Stream   string `json:"stream"`
	Sequence uint64 `json:"sequence"`
	Applied  int    `json:"applied"`
	Skipped  int    `json:"skipped"`
	Err      string `json:"err"`
}

// ingestReportView is the subset of the connect/ingest report the text renderer
// prints. NDJSON/JSON modes pass the raw response through untouched.
type ingestReportView struct {
	Connector        string             `json:"connector"`
	JobUID           string             `json:"jobUid"`
	DryRun           bool               `json:"dryRun"`
	ItemsApplied     int                `json:"itemsApplied"`
	ItemsSkipped     int                `json:"itemsSkipped"`
	EpisodesAppended int                `json:"episodesAppended"`
	Streams          []ingestStreamView `json:"streams"`
}

// renderIngestReportCLI prints a connectSource/ingestSource report. NDJSON mode
// emits the raw object on one line; text mode prints a summary and per-stream
// rows, leading with a DRY RUN banner when nothing was written.
func renderIngestReportCLI(cmd *cobra.Command, op string, raw json.RawMessage) error {
	if output.From(cmd).Format() == output.FormatNDJSON {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return err
	}
	var v ingestReportView
	if err := json.Unmarshal(raw, &v); err != nil {
		return wrapNavFatal(cmd, op, fmt.Errorf("parse response: %w", err))
	}
	w := cmd.OutOrStdout()
	if v.DryRun {
		//nolint:errcheck // best-effort stdout; tabwriter.Flush surfaces any real write error
		fmt.Fprintf(w, "DRY RUN — nothing written\n\n")
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	rows := []struct{ k, v string }{
		{"connector", v.Connector},
		{"applied", strconv.Itoa(v.ItemsApplied)},
		{"skipped", strconv.Itoa(v.ItemsSkipped)},
		{"episodes", strconv.Itoa(v.EpisodesAppended)},
		{"job", v.JobUID},
	}
	for _, r := range rows {
		if r.v == "" {
			continue
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\n", r.k, r.v); err != nil {
			return err
		}
	}
	for i := range v.Streams {
		s := v.Streams[i]
		detail := fmt.Sprintf("applied=%d skipped=%d seq=%d", s.Applied, s.Skipped, s.Sequence)
		if s.Err != "" {
			detail += " skipped-stream: " + s.Err
		}
		if _, err := fmt.Fprintf(tw, "stream %s\t%s\n", s.Stream, detail); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// sourceListView is the listSources response subset the text renderer prints.
type sourceListView struct {
	Connectors []string `json:"connectors"`
	Watermarks []struct {
		Connector   string `json:"connector"`
		Stream      string `json:"stream"`
		Sequence    uint64 `json:"sequence"`
		AdvanceTime string `json:"advanceTime"`
		Status      string `json:"status"`
	} `json:"watermarks"`
}

// sourceAdvanceLabel renders a watermark's last-advance time as a compact
// human delta ("21m ago", "1d ago"), or "never" when it has not advanced.
func sourceAdvanceLabel(advanceTime string) string {
	if advanceTime == "" {
		return "never"
	}
	t, err := time.Parse(time.RFC3339Nano, advanceTime)
	if err != nil {
		return advanceTime
	}
	return RelativeTime(t)
}

// renderSourceListCLI prints a listSources response. NDJSON mode emits the raw
// object on one line; text mode prints the available connectors and a table of
// per-(connector, stream) resume watermarks.
func renderSourceListCLI(cmd *cobra.Command, raw json.RawMessage) error {
	if output.From(cmd).Format() == output.FormatNDJSON {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return err
	}
	var v sourceListView
	if err := json.Unmarshal(raw, &v); err != nil {
		return wrapNavFatal(cmd, "source list", fmt.Errorf("parse response: %w", err))
	}
	w := cmd.OutOrStdout()
	//nolint:errcheck // best-effort stdout
	fmt.Fprintf(w, "connectors: %s\n\n", strings.Join(v.Connectors, ", "))
	if len(v.Watermarks) == 0 {
		_, err := fmt.Fprintln(w, "no sources connected yet")
		return err
	}
	c := cmdColorizer(cmd)
	tbl := output.NewTable(output.From(cmd), c, []output.Column{
		{Title: "connector"},
		{Title: "stream"},
		{Title: "items", Align: output.AlignRight},
		{Title: "status"},
		{Title: "last advance"},
	})
	for i := range v.Watermarks {
		wm := v.Watermarks[i]
		tbl.Row(wm.Connector, wm.Stream, strconv.FormatUint(wm.Sequence, 10), wm.Status, sourceAdvanceLabel(wm.AdvanceTime))
	}
	return tbl.Render()
}
