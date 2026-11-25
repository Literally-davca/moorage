package cmd

import (
	"github.com/spf13/cobra"

	"moorage/internal/cli/tui"
)

func newCLITuiCmd(opts *rootOpts) *cobra.Command {
	return &cobra.Command{
		Use:     "cli",
		Aliases: []string{"tui"},
		Short:   "Launch the interactive terminal layout (lazydocker-style)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := opts.client()
			if err != nil {
				return err
			}
			return tui.Run(ctx, c, opts.version, opts.host)
		},
	}
}
