package cli

// preset_package.go implements "maz preset package <dir>": a read-only,
// daemon-free validator that checks a bundle and prints its canonical checksum
// so an author can publish a verifiable artifact (#224). It reuses the #210
// schemapack.ParseDir validators verbatim — an invalid manifest/kind/predicate
// or an over-cap instructions.md is rejected with the same actionable message a
// local install would surface.

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/plugin/schemapack"
)

// newPresetPackageCmd returns the "maz preset package <dir>" cobra command.
func newPresetPackageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "package <dir>",
		Short: "Validate a preset bundle and print its canonical checksum",
		Long: `Validate the bundle in <dir> (manifest, kinds, predicates, and the
instructions.md byte cap) and print its canonical sha256 checksum plus a
summary. Publish the printed checksum so installers can pin it:

  maz preset install <git-url> --checksum sha256:...

This command is read-only: it appends no events and needs no daemon.

Exit codes:
  0   valid bundle; checksum printed
  1   invalid bundle (bad manifest/kind/predicate, over-cap instructions, or
      not a directory) — the message names the offending file`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("preset package: accepts 1 argument, got %d", len(args))
			}
			return runPresetPackage(cmd, args[0])
		},
	}
	return cmd
}

// presetPackageResult is the JSON/CLI output shape for "maz preset package".
type presetPackageResult struct {
	Name              string   `json:"name"`
	Version           string   `json:"version"`
	Description       string   `json:"description"`
	Checksum          string   `json:"checksum"`
	IsPreset          bool     `json:"isPreset"`
	InstructionsBytes int      `json:"instructionsBytes"`
	Kinds             []string `json:"kinds"`
	Predicates        []string `json:"predicates"`
}

func runPresetPackage(cmd *cobra.Command, dir string) error {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		e := fmt.Errorf("%q is not a directory", dir)
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "preset package: %v\n", e)
		return silentError{code: 1, err: e}
	}
	pack, perr := schemapack.ParseDir(dir)
	if perr != nil {
		//nolint:errcheck // best-effort stderr
		fmt.Fprintf(cmd.ErrOrStderr(), "preset package: %v\n", perr)
		return silentError{code: 1, err: perr}
	}
	sum, cerr := schemapack.BundleChecksum(os.DirFS(dir), ".")
	if cerr != nil {
		return wrapPresetFatal(cmd, fmt.Errorf("checksum bundle: %w", cerr))
	}

	res := presetPackageResult{
		Name:              pack.Manifest.Name,
		Version:           pack.Manifest.Version,
		Description:       pack.Manifest.Description,
		Checksum:          sum,
		IsPreset:          pack.IsPreset(),
		InstructionsBytes: len(pack.Instructions),
		Kinds:             make([]string, 0, len(pack.Kinds)),
		Predicates:        make([]string, 0, len(pack.Predicates)),
	}
	for i := range pack.Kinds {
		res.Kinds = append(res.Kinds, pack.Kinds[i].Name)
	}
	for i := range pack.Predicates {
		res.Predicates = append(res.Predicates, pack.Predicates[i].Predicate)
	}
	return renderPresetPackageResult(cmd, &res)
}

func renderPresetPackageResult(cmd *cobra.Command, res *presetPackageResult) error {
	w := cmd.OutOrStdout()
	wr := output.From(cmd)
	if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	//nolint:errcheck // best-effort tabwriter; flush error returned below
	fmt.Fprintf(tw, "Bundle\t%s@%s\n", res.Name, res.Version)
	//nolint:errcheck // best-effort tabwriter
	fmt.Fprintf(tw, "Checksum\t%s\n", res.Checksum)
	//nolint:errcheck // best-effort tabwriter
	fmt.Fprintf(tw, "Preset\t%t\n", res.IsPreset)
	//nolint:errcheck // best-effort tabwriter
	fmt.Fprintf(tw, "Kinds\t%d\n", len(res.Kinds))
	//nolint:errcheck // best-effort tabwriter
	fmt.Fprintf(tw, "Predicates\t%d\n", len(res.Predicates))
	if res.IsPreset {
		//nolint:errcheck // best-effort tabwriter
		fmt.Fprintf(tw, "Instructions\t%d bytes\n", res.InstructionsBytes)
	}
	if ferr := tw.Flush(); ferr != nil {
		return ferr
	}
	if !res.IsPreset {
		//nolint:errcheck // best-effort stdout
		fmt.Fprintln(w, "\nNote: no instructions.md — this is a plain schema pack, not a preset.")
	}
	_, werr := fmt.Fprintf(w,
		"\nPublish this checksum so installers can pin it:\n  maz preset install <git-url> --checksum %s\n", res.Checksum)
	return werr
}
