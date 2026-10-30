package cli

import (
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/mo-cmyk/mazarbul/internal/cli/output"
)

func newDebugCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "debug",
		Short: "Diagnostic commands (pprof, runtime stats)",
	}
	cmd.AddCommand(newDebugTopCmd())
	cmd.AddCommand(newDebugPprofCmd())
	return cmd
}

func newDebugTopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "top",
		Short: "Snapshot of runtime stats",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			stats := map[string]any{
				"goroutines": runtime.NumGoroutine(),
				"heapAlloc":  m.HeapAlloc,
				"heapInuse":  m.HeapInuse,
				"sysBytes":   m.Sys,
				"numGC":      m.NumGC,
			}
			w := cmd.OutOrStdout()
			wr := output.From(cmd)
			if wr.Format() == output.FormatJSON || wr.Format() == output.FormatNDJSON {
				return json.NewEncoder(w).Encode(stats)
			}
			_, err := fmt.Fprintf(w, "goroutines=%d heap_alloc=%d heap_inuse=%d sys=%d gc=%d\n",
				stats["goroutines"], stats["heapAlloc"], stats["heapInuse"], stats["sysBytes"], stats["numGC"])
			return err
		},
	}
}

func newDebugPprofCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pprof",
		Short: "Print pprof endpoint instructions",
		RunE: func(cmd *cobra.Command, _ []string) error {
			addr, _ := cmd.Root().PersistentFlags().GetString("debug-addr")
			w := cmd.OutOrStdout()
			if addr == "" {
				_, err := fmt.Fprintln(w, "pass --debug-addr=localhost:6060 to expose /debug/pprof/")
				return err
			}
			_, err := fmt.Fprintf(w, "pprof endpoints available at http://%s/debug/pprof/\n", addr)
			return err
		},
	}
}
