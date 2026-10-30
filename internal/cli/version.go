package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
	"github.com/mo-cmyk/mazarbul/internal/version"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print maz version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := cmd.OutOrStdout()
			wr := output.From(cmd)
			if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
				return json.NewEncoder(w).Encode(map[string]string{
					"version": version.Version,
					"commit":  version.Commit,
					"date":    version.Date,
				})
			}
			// Plain text prints all three on separate lines. On a dev build
			// commit/date are their "none"/"unknown" defaults (only release
			// ldflags inject them); printing them anyway is honest and keeps
			// the format stable across build paths.
			_, err := fmt.Fprintf(w, "maz %s\ncommit %s\nbuilt %s\n", version.Version, version.Commit, version.Date)
			return err
		},
	}
}
