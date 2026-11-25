package cmd

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"moorage/internal/cli/client"
	"moorage/internal/convert"
)

func newConvertCmd(opts *rootOpts) *cobra.Command {
	var createName string
	var up bool

	cmd := &cobra.Command{
		Use:   "convert <docker run ...>",
		Short: "Convert a `docker run` command to compose.yaml (works offline)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw := strings.Join(args, " ")
			res, err := convert.DockerRun(raw)
			if err != nil {
				return err
			}
			if createName != "" {
				ctx := cmd.Context()
				c, hostID, err := opts.connect(ctx)
				if err != nil {
					return err
				}
				if err := c.Post(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/stacks", map[string]any{
					"name":    createName,
					"compose": res.YAML,
					"env":     "",
				}, nil); err != nil {
					return err
				}
				if up {
					var op client.Op
					if err := c.Post(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/stacks/"+url.PathEscape(createName)+"/actions/up", nil, &op); err != nil {
						return err
					}
					if err := streamOp(ctx, c, hostID, op.ID, cmd.OutOrStdout(), opts.quiet); err != nil {
						return err
					}
				}
			}
			if opts.jsonOut {
				return printJSON(cmd.OutOrStdout(), res)
			}
			for _, w := range res.Warnings {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", w)
			}
			fmt.Fprint(cmd.OutOrStdout(), res.YAML)
			return nil
		},
	}
	cmd.Flags().StringVar(&createName, "create", "", "create stack on host with this name")
	cmd.Flags().BoolVar(&up, "up", false, "also deploy the created stack (--create)")
	return cmd
}
