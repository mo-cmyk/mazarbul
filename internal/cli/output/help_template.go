package output

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
)

// Command-tier annotation. Commands tagged advanced are hidden from the
// curated default help listing and surface only with `maz help --all`. This
// is a help-presentation flag, distinct from cobra's Hidden (which suppresses
// a command from --all too). Keys live on cobra.Command.Annotations.
const (
	TierAnnotation = "maz.tier"
	TierAdvanced   = "advanced"
)

// helpTemplate is the clig.dev-aligned help template applied to every
// cobra command via cmd.SetHelpTemplate on the root. It renders the
// USAGE, DESCRIPTION, EXAMPLES, FLAGS, and SEE ALSO sections in a
// clean fixed-width format with no colour escapes so it works in both
// TTY and piped contexts.
//
// Template variables mirror cobra's built-in helper set:
//   - .UseLine()         — "maz entities list [flags]"
//   - .Short             — one-line description
//   - .Long              — multi-paragraph description
//   - .Example           — pre-formatted example block
//   - .LocalFlags()      — FlagSet for this command
//   - .InheritedFlags()  — FlagSet from parents
//   - .Commands()        — slice of *cobra.Command (for groups)
//   - .HasAvailableSubCommands()
//   - .HasExample()
const helpTemplate = `USAGE
  {{.UseLine}}
{{- if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]
{{- end}}
{{- if .Short}}

DESCRIPTION
  {{wrap .Short 80 2}}
{{- end}}
{{- if .Long}}

  {{wrap .Long 80 2}}
{{- end}}
{{- if .HasExample}}

EXAMPLES
{{trimRight .Example}}
{{- end}}
{{- if or .HasAvailableLocalFlags .HasAvailableInheritedFlags}}

FLAGS
{{localFlagUsages .}}{{inheritedFlagUsages .}}
{{- end}}
{{- if .HasAvailableSubCommands}}
{{- if hasGroups .}}

{{groupedCommands .}}
{{- else}}

AVAILABLE COMMANDS
{{subcommandList .}}
{{- end}}
{{- end}}
{{- if .HasAvailableSubCommands}}

Use "{{.CommandPath}} [command] --help" for more information about a command.
{{- end}}
{{- if .HasParent}}

SEE ALSO
  {{seeAlso .}}
{{- end}}
`

// usageTemplate is the shorter usage-on-error form.
const usageTemplate = `Usage:
  {{.UseLine}}
{{- if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]
{{- end}}

Run "{{.CommandPath}} --help" for usage.
`

// ApplyHelpTemplate installs the clig.dev-aligned help and usage
// templates on root and all of its descendants. Call this once after
// the full command tree has been built, before cmd.Execute().
//
//nolint:gocognit // template FuncMap + recursive apply; splitting would obscure the intent
func ApplyHelpTemplate(root *cobra.Command) {
	funcs := template.FuncMap{
		"hasGroups": func(cmd *cobra.Command) bool { return len(cmd.Groups()) > 0 },
		"groupedCommands": func(cmd *cobra.Command) string {
			// Read --all directly off the command being rendered. hasGroups
			// gates this to root, which owns the flag, and every entry point
			// sets it before Help() runs (`maz --all` / `maz --help --all` parse
			// it; `maz help --all` forwards it in newHelpCmd). Reading it here
			// keeps the renderer free of shared mutable state — no data race.
			all, _ := cmd.Flags().GetBool("all")
			return renderGroupedCommands(cmd, all)
		},
		"wrap": wrapText,
		"trimRight": func(s string) string {
			return strings.TrimRight(s, "\n")
		},
		"localFlagUsages": func(cmd *cobra.Command) string {
			return flagUsages(cmd.LocalFlags())
		},
		"inheritedFlagUsages": func(cmd *cobra.Command) string {
			fs := cmd.InheritedFlags()
			if !fs.HasAvailableFlags() {
				return ""
			}
			return "  (global)\n" + flagUsages(fs)
		},
		"subcommandList": func(cmd *cobra.Command) string {
			var b bytes.Buffer
			for _, sub := range cmd.Commands() {
				if sub.Hidden {
					continue
				}
				b.WriteString("  ")
				b.WriteString(padRight(sub.Name(), 20))
				b.WriteString(sub.Short)
				b.WriteByte('\n')
			}
			return b.String()
		},
		"seeAlso": func(cmd *cobra.Command) string {
			var parts []string
			if cmd.HasParent() {
				parts = append(parts, cmd.Parent().CommandPath())
			}
			for _, sib := range cmd.Parent().Commands() {
				if sib == cmd || sib.Hidden {
					continue
				}
				parts = append(parts, sib.CommandPath())
				if len(parts) >= 4 { // cap at a few siblings
					break
				}
			}
			return strings.Join(parts, "\n  ")
		},
	}

	t := template.Must(template.New("help").Funcs(funcs).Parse(helpTemplate))

	var apply func(*cobra.Command)
	apply = func(cmd *cobra.Command) {
		cmd.SetHelpFunc(func(c *cobra.Command, args []string) {
			var buf bytes.Buffer
			if err := t.Execute(&buf, c); err != nil {
				// Fallback: let cobra handle it.
				c.Usage() //nolint:errcheck // best-effort help rendering
				return
			}
			c.OutOrStdout().Write(buf.Bytes()) //nolint:errcheck // best-effort help
		})
		cmd.SetUsageTemplate(usageTemplate)
		for _, sub := range cmd.Commands() {
			apply(sub)
		}
	}
	apply(root)
}

// renderGroupedCommands renders cmd's subcommands as labelled task sections,
// in the group order registered via AddGroup. Unless showAll is set, commands
// tagged with the advanced tier are omitted and a `maz help --all` hint is
// appended. Commands whose GroupID matches no registered group are listed
// under a trailing MORE section so nothing is ever silently dropped. The
// returned string has no leading/trailing newline; the template supplies the
// surrounding blank lines.
func renderGroupedCommands(cmd *cobra.Command, showAll bool) string {
	var sections []string
	hidden := 0
	for _, g := range cmd.Groups() {
		section, h := groupSection(cmd, g.ID, g.Title, showAll)
		hidden += h
		if section != "" {
			sections = append(sections, section)
		}
	}
	if orphans := orphanSection(cmd); orphans != "" {
		sections = append(sections, orphans)
	}

	out := strings.Join(sections, "\n\n")
	if !showAll && hidden > 0 {
		out += fmt.Sprintf("\n\nRun \"maz help --all\" to see all %d commands.", visibleCount(cmd))
	}
	return out
}

// groupSection renders one group's labelled block. It returns the section text
// (empty when the group has no visible commands) and the number of advanced
// commands it hid (nonzero only when showAll is false).
func groupSection(cmd *cobra.Command, groupID, title string, showAll bool) (string, int) {
	lines := make([]string, 0, len(cmd.Commands()))
	hidden := 0
	for _, sub := range cmd.Commands() {
		if sub.Hidden || sub.GroupID != groupID {
			continue
		}
		if !showAll && sub.Annotations[TierAnnotation] == TierAdvanced {
			hidden++
			continue
		}
		lines = append(lines, commandLine(sub))
	}
	if len(lines) == 0 {
		return "", hidden
	}
	return title + "\n" + strings.Join(lines, "\n"), hidden
}

// orphanSection lists commands whose GroupID matches no registered group, so a
// mis-registered command surfaces under MORE rather than vanishing silently.
func orphanSection(cmd *cobra.Command) string {
	lines := make([]string, 0, len(cmd.Commands()))
	for _, sub := range cmd.Commands() {
		if sub.Hidden || groupKnown(cmd, sub.GroupID) {
			continue
		}
		lines = append(lines, commandLine(sub))
	}
	if len(lines) == 0 {
		return ""
	}
	return "MORE\n" + strings.Join(lines, "\n")
}

// visibleCount counts non-hidden subcommands — the denominator in the --all hint.
func visibleCount(cmd *cobra.Command) int {
	n := 0
	for _, sub := range cmd.Commands() {
		if !sub.Hidden {
			n++
		}
	}
	return n
}

// commandLine formats one command's "  name    short" listing row.
func commandLine(sub *cobra.Command) string {
	return "  " + padRight(sub.Name(), 20) + sub.Short
}

// groupKnown reports whether id names a group registered on cmd.
func groupKnown(cmd *cobra.Command, id string) bool {
	if id == "" {
		return false
	}
	for _, g := range cmd.Groups() {
		if g.ID == id {
			return true
		}
	}
	return false
}

// wrapText wraps s at width characters, indenting continuation lines
// by indent spaces.
func wrapText(s string, width, indent int) string {
	// Preserve existing explicit newlines; only wrap within paragraphs.
	paragraphs := strings.Split(s, "\n\n")
	out := make([]strings.Builder, 0, len(paragraphs))
	for _, para := range paragraphs {
		var b strings.Builder
		words := strings.Fields(para)
		col := 0
		for i, w := range words {
			if i == 0 {
				b.WriteString(w)
				col = len(w)
				continue
			}
			if col+1+len(w) > width {
				b.WriteByte('\n')
				b.WriteString(strings.Repeat(" ", indent))
				b.WriteString(w)
				col = indent + len(w)
			} else {
				b.WriteByte(' ')
				b.WriteString(w)
				col += 1 + len(w)
			}
		}
		out = append(out, b)
	}
	parts := make([]string, len(out))
	for i, b := range out {
		parts[i] = b.String()
	}
	return strings.Join(parts, "\n\n")
}

// flagUsages returns a compact flag usage block for fs.
func flagUsages(fs interface {
	FlagUsages() string
	HasAvailableFlags() bool
},
) string {
	if !fs.HasAvailableFlags() {
		return ""
	}
	usage := fs.FlagUsages()
	// Indent each line by two spaces for visual hierarchy.
	lines := strings.Split(strings.TrimRight(usage, "\n"), "\n")
	var b bytes.Buffer
	for _, l := range lines {
		b.WriteString("  ")
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String()
}

// padRight pads s with spaces on the right to the given width.
func padRight(s string, w int) string {
	if len(s) >= w {
		return s + " "
	}
	return s + strings.Repeat(" ", w-len(s))
}
