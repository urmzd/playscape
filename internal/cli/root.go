// Package cli wires the playscape command tree: serve, version, update.
package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/urmzd/playscape/internal/updater"
)

// Version metadata injected at build time via -ldflags.
type Version struct {
	Version string
	Commit  string
	Date    string
}

var format string

// Execute runs the playscape CLI and returns its exit code.
func Execute(v Version) int {
	root := &cobra.Command{
		Use:           "playscape",
		Short:         "Control plane for agent execution nodes (beta)",
		Long:          "playscape (beta) — deploy a single service, scale sandboxed agent execution\nnodes, and observe them, all against one shared workspace backend.",
		Version:       v.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&format, "format", "human", "Output format: json|human")

	root.AddCommand(newServeCmd(), newWorkCmd(), newVersionCmd(v), newUpdateCmd(v))

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func newVersionCmd(v Version) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the playscape version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if format == "json" {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]string{
					"version": v.Version,
					"commit":  v.Commit,
					"date":    v.Date,
				})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "playscape v%s\n", v.Version)
			return nil
		},
	}
}

func newUpdateCmd(v Version) *cobra.Command {
	return &cobra.Command{
		Use:   "update",
		Short: "Self-update to the latest release",
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Fprintf(os.Stderr, "current version: %s\n", v.Version)
			tag, err := updater.Update(v.Version)
			if err != nil {
				return err
			}
			if tag == "v"+v.Version || tag == v.Version {
				fmt.Fprintln(os.Stderr, "already up to date")
				return nil
			}
			fmt.Fprintf(os.Stderr, "updated: v%s → %s\n", v.Version, tag)
			return nil
		},
	}
}
