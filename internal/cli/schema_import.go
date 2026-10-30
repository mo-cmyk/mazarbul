package cli

// schema_import.go implements "maz schema import <vocab>" — imports types
// from an external vocabulary snapshot into the Mazarbul kind taxonomy.
//
// Supported vocabularies: schema.org, foaf, dublincore, skos, wikidata.
//
// Write pathway: kernelboot.OpenLog → appendExtensionKindEvent per generated
// event (identical to the install pipeline).
//
// Exit codes:
//
//	0   import complete (events appended, or dry-run listed)
//	1   expected failure: unknown vocab, bad flags, validation error
//	2   unexpected failure: storage or wiring error

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/config"
	"github.com/mo-cmyk/mazarbul/internal/kernelboot"
	"github.com/mo-cmyk/mazarbul/internal/logging"
	"github.com/mo-cmyk/mazarbul/internal/ontology/vocab"
	"github.com/mo-cmyk/mazarbul/internal/ontology/vocab/dublincore"
	"github.com/mo-cmyk/mazarbul/internal/ontology/vocab/foaf"
	"github.com/mo-cmyk/mazarbul/internal/ontology/vocab/schemaorg"
	"github.com/mo-cmyk/mazarbul/internal/ontology/vocab/skos"
	"github.com/mo-cmyk/mazarbul/internal/ontology/vocab/wikidata"
)

// supportedVocabs lists the vocabularies with embedded snapshots.
//
//nolint:gochecknoglobals // read-only lookup, never mutated
var supportedVocabs = []string{"schema.org", "foaf", "dublincore", "skos", "wikidata"}

// schemaImportFlags holds parsed flags for "maz schema import".
type schemaImportFlags struct {
	types    []string // --types a,b,c
	dryRun   bool
	fromFile string // --from-file <path>
}

// newSchemaImportCmd returns the "maz schema import <vocab>" cobra command.
func newSchemaImportCmd() *cobra.Command {
	flags := &schemaImportFlags{}
	cmd := &cobra.Command{
		Use:   "import <vocab>",
		Short: "Import types from an external vocabulary snapshot",
		Long: `Import entity kinds from an external vocabulary snapshot into the
Mazarbul kind taxonomy by appending entityKindRegistered events.

Supported vocabularies:
  schema.org   Curated offline snapshot of 30 common schema.org types.
               For a full import, convert a schema.org dump to the types.json
               format and pass it via --from-file.
  foaf         FOAF (Friend of a Friend) vocabulary — 10 classes including
               Agent, Person, Organization, Group, Document.
  dublincore   Dublin Core (DCMI) — 8 type classes including Agent,
               BibliographicResource, Collection, Dataset, Event, Image.
  skos         SKOS core — 4 classes: Concept, ConceptScheme, Collection,
               OrderedCollection.
  wikidata     25 curated Wikidata entity classes (Q-IDs). For a broader
               import, convert a SPARQL CONSTRUCT export to the types.json
               format and pass it via --from-file.

Flags:
  --types a,b,c     Comma-separated list of type names to import (default: all)
  --dry-run         Print the kinds that would register without appending events
  --from-file <p>   Load the vocabulary snapshot from a local JSON file instead
                    of the embedded snapshot. Works for every vocabulary; the
                    imported kinds keep the vocabulary's namespace and core-kind
                    crosswalk. The file must be a JSON array of type entries in
                    the internal types.json format:
                    [{"id":"<IRI>","label":"<Label>","comment":"<desc>","subClassOf":"<IRI>"}]
                    Pass "-" to read from stdin.

Exit codes:
  0   import complete
  1   expected failure: unknown vocab, bad flags
  2   unexpected failure: storage error

Examples:
  maz schema import schema.org
  maz schema import schema.org --types Person,Organization,Place
  maz schema import schema.org --dry-run
  maz schema import schema.org --from-file ./my-schemaorg-types.json
  maz schema import wikidata --from-file ./my-wikidata-types.json
  maz schema import foaf --dry-run
  maz schema import dublincore --dry-run
  maz schema import skos --dry-run
  maz schema import wikidata --dry-run
  maz schema import schema.org --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSchemaImport(cmd, args[0], flags)
		},
	}
	cmd.Flags().StringSliceVar(&flags.types, "types", nil, "Comma-separated type names to import (default: all)")
	cmd.Flags().BoolVar(&flags.dryRun, "dry-run", false, "list kinds that would register without appending events")
	cmd.Flags().StringVar(&flags.fromFile, "from-file", "", "load vocabulary snapshot from a local JSON file in the internal types.json format (works for every vocabulary)")
	return cmd
}

// importResult is the JSON output shape for maz schema import.
type importResult struct {
	Vocab      string    `json:"vocab"`
	Imported   []string  `json:"imported"`
	Skipped    []string  `json:"skipped"`
	DryRun     bool      `json:"dryRun"`
	ImportedAt time.Time `json:"importedAt,omitempty"`
}

// runSchemaImport validates the vocab argument, loads the importer, and
// appends the generated events (or prints them in dry-run mode).
func runSchemaImport(cmd *cobra.Command, vocabArg string, flags *schemaImportFlags) error {
	ctx := cmd.Context()
	logger := logging.FromContext(ctx)

	// ---- validate vocab argument ----
	known := false
	for _, s := range supportedVocabs {
		if strings.EqualFold(vocabArg, s) {
			known = true
			break
		}
	}
	if !known {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(),
			"schema import: unknown vocabulary %q; supported: %s\n",
			vocabArg, strings.Join(supportedVocabs, ", "))
		return silentError{code: 1, err: fmt.Errorf("unknown vocabulary: %s", vocabArg)}
	}

	// ---- load importer ----
	importer, err := loadVocabImporter(vocabArg, flags.fromFile)
	if err != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "schema import: load vocabulary: %v\n", err)
		return silentError{code: 1, err: err}
	}

	// ---- run import ----
	importOpts := vocab.ImportOptions{
		Types:   flags.types,
		AddedAt: time.Now().UTC(),
	}
	ir, err := importer.Import(importOpts)
	if err != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "schema import: %v\n", err)
		return silentError{code: 1, err: err}
	}

	res := &importResult{
		Vocab:   vocabArg,
		DryRun:  flags.dryRun,
		Skipped: ir.Skipped,
	}
	for _, ev := range ir.Events {
		res.Imported = append(res.Imported, ev.Name)
	}

	if flags.dryRun {
		return renderImportResult(cmd, res)
	}

	// ---- append events ----
	dataDir, _ := cmd.Root().PersistentFlags().GetString("data-dir")
	if dataDir == "" {
		dataDir = config.Defaults().DataDir
	}

	// No importVocabulary MCP tool exists, so when a daemon holds the store lock
	// the only safe path is to refuse cleanly rather than race its exclusive
	// event-log lock (the in-process kernelboot.OpenLog below would block/fail).
	if refused, rerr := refuseSchemaWriteUnderDaemon(cmd, dataDir, "schema import"); refused {
		return rerr
	}

	log, closeFn, err := kernelboot.OpenLog(ctx, dataDir)
	if err != nil {
		return wrapImportFatal(cmd, fmt.Errorf("open eventlog: %w", err))
	}
	defer func() {
		if cerr := closeFn(); cerr != nil {
			logger.Warn("schema import: close eventlog", zap.Error(cerr))
		}
	}()

	source := "cli:schema:import:" + strings.ReplaceAll(strings.ToLower(vocabArg), ".", "_")
	if serr := appendExtensionKindEvents(ctx, log, ir.Events, source); serr != nil {
		return wrapImportFatal(cmd, fmt.Errorf("append kinds: %w", serr))
	}

	res.ImportedAt = time.Now().UTC()
	return renderImportResult(cmd, res)
}

// loadVocabImporter returns a vocab.Importer for the named vocabulary, loaded
// from the embedded snapshot, or — when fromFile is non-empty — from a
// user-supplied types.json file. The file format is identical for every
// vocabulary, so --from-file works for all five: the entries are parsed once
// and routed to the vocabulary's importer, which keeps its own namespace and
// core-kind crosswalk.
func loadVocabImporter(vocabArg, fromFile string) (*vocab.Importer, error) {
	var entries []vocab.TypeEntry
	if fromFile != "" {
		data, err := readFileBytes(fromFile)
		if err != nil {
			return nil, fmt.Errorf("read --from-file %q: %w", fromFile, err)
		}
		entries, err = vocab.ParseTypeEntries(data)
		if err != nil {
			return nil, err
		}
		// JSON "null" parses to a nil slice; reject it here so a malformed
		// replacement file errors out instead of falling through the
		// entries != nil checks below into a full embedded-snapshot import.
		if entries == nil {
			return nil, fmt.Errorf(
				"--from-file %q contains no type entries; expected a JSON array in the internal types.json format", fromFile)
		}
	}

	switch strings.ToLower(vocabArg) {
	case "schema.org":
		if entries != nil {
			return vocab.NewSchemaOrgImporter(entries), nil
		}
		return schemaorg.NewImporter()
	case "foaf":
		if entries != nil {
			return vocab.NewFoafImporter(entries), nil
		}
		return foaf.NewImporter()
	case "dublincore":
		if entries != nil {
			return vocab.NewDublinCoreImporter(entries), nil
		}
		return dublincore.NewImporter()
	case "skos":
		if entries != nil {
			return vocab.NewSkosImporter(entries), nil
		}
		return skos.NewImporter()
	case "wikidata":
		if entries != nil {
			return vocab.NewWikidataImporter(entries), nil
		}
		return wikidata.NewImporter()
	default:
		return nil, fmt.Errorf("unknown vocabulary: %s", vocabArg)
	}
}

// readFileBytes reads bytes from a file path or from stdin when path is "-".
func readFileBytes(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

// renderImportResult renders the import result as JSON or CLI text.
func renderImportResult(cmd *cobra.Command, res *importResult) error {
	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	return renderImportCLI(w, res)
}

// renderImportCLI prints a human-readable import summary.
func renderImportCLI(w io.Writer, res *importResult) error {
	prefix := "Imported"
	if res.DryRun {
		prefix = "Would import"
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintf(tw, "Vocabulary\t%s\n", res.Vocab); err != nil {
		return err
	}
	if res.DryRun {
		if _, err := fmt.Fprintf(tw, "Mode\tdry-run (no events appended)\n"); err != nil {
			return err
		}
	} else if !res.ImportedAt.IsZero() {
		if _, err := fmt.Fprintf(tw, "Imported at\t%s\n", res.ImportedAt.Format(time.RFC3339)); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if err := writeBulletList(w, fmt.Sprintf("\n%s %d kind(s):\n", prefix, len(res.Imported)), res.Imported); err != nil {
		return err
	}
	if err := writeSkippedList(w, res.Skipped); err != nil {
		return err
	}
	if !res.DryRun && len(res.Imported) > 0 {
		if _, err := fmt.Fprintf(w, "\nNext steps:\n  maz schema kinds list\n"); err != nil {
			return err
		}
	}
	return nil
}

// writeSkippedList prints the skipped types section. It uses a "-" bullet
// (distinct from the "+" bullet of writeBulletList) to signal filtered items.
func writeSkippedList(w io.Writer, items []string) error {
	if len(items) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(w, "\nSkipped %d type(s) (filtered by --types):\n", len(items)); err != nil {
		return err
	}
	for _, item := range items {
		if _, err := fmt.Fprintf(w, "  - %s\n", item); err != nil {
			return err
		}
	}
	return nil
}

// wrapImportFatal prints the error to stderr and returns silentError(2).
func wrapImportFatal(cmd *cobra.Command, err error) error {
	return respondFatal(cmd, "schema import", err)
}
