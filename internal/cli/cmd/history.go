package cmd

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"moorage/internal/cli/client"
)

func newHistoryCmd(opts *rootOpts) *cobra.Command {
	root := &cobra.Command{
		Use:     "history",
		Aliases: []string{"hist"},
		Short:   "Inspect git revision history, diff, and rollback stacks",
	}

	root.AddCommand(
		&cobra.Command{
			Use:     "list <stack>",
			Aliases: []string{"ls"},
			Short:   "List saved git revisions of a stack",
			Args:    cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx := cmd.Context()
				c, hostID, err := opts.connect(ctx)
				if err != nil {
					return err
				}
				var list []client.Version
				if err := c.Get(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/stacks/"+url.PathEscape(args[0])+"/history", &list); err != nil {
					return err
				}
				if opts.jsonOut {
					return printJSON(cmd.OutOrStdout(), list)
				}
				if opts.quiet {
					for _, v := range list {
						fmt.Fprintln(cmd.OutOrStdout(), v.Hash)
					}
					return nil
				}
				tw := newTab(cmd.OutOrStdout())
				fmt.Fprintln(tw, "HASH\tDATE\tAUTHOR\tMESSAGE")
				for _, v := range list {
					short := v.Short
					if short == "" && len(v.Hash) >= 7 {
						short = v.Hash[:7]
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", short, v.At.Local().Format("2006-01-02 15:04"), v.Author, v.Message)
				}
				return tw.Flush()
			},
		},
		&cobra.Command{
			Use:   "show <stack> <hash>",
			Short: "Print compose.yaml and .env of a specific revision",
			Args:  cobra.ExactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx := cmd.Context()
				c, hostID, err := opts.connect(ctx)
				if err != nil {
					return err
				}
				var v client.Version
				if err := c.Get(ctx, fmt.Sprintf("/api/hosts/%s/stacks/%s/history/%s",
					url.PathEscape(hostID), url.PathEscape(args[0]), url.PathEscape(args[1])), &v); err != nil {
					return err
				}
				if opts.jsonOut {
					return printJSON(cmd.OutOrStdout(), v)
				}
				fmt.Fprint(cmd.OutOrStdout(), v.Compose)
				return nil
			},
		},
		&cobra.Command{
			Use:   "diff <stack> <hashA> [hashB]",
			Short: "Compare compose.yaml between two revisions (or revision vs live)",
			Args:  cobra.RangeArgs(2, 3),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx := cmd.Context()
				c, hostID, err := opts.connect(ctx)
				if err != nil {
					return err
				}
				var a client.Version
				if err := c.Get(ctx, fmt.Sprintf("/api/hosts/%s/stacks/%s/history/%s",
					url.PathEscape(hostID), url.PathEscape(args[0]), url.PathEscape(args[1])), &a); err != nil {
					return err
				}
				var bText, bLabel string
				if len(args) == 3 {
					var b client.Version
					if err := c.Get(ctx, fmt.Sprintf("/api/hosts/%s/stacks/%s/history/%s",
						url.PathEscape(hostID), url.PathEscape(args[0]), url.PathEscape(args[2])), &b); err != nil {
						return err
					}
					bText = b.Compose
					bLabel = args[2]
				} else {
					var st client.Stack
					if err := c.Get(ctx, fmt.Sprintf("/api/hosts/%s/stacks/%s",
						url.PathEscape(hostID), url.PathEscape(args[0])), &st); err != nil {
						return err
					}
					bText = st.Compose
					bLabel = "live"
				}
				diff := lineDiff(args[1], bLabel, a.Compose, bText)
				if opts.jsonOut {
					return printJSON(cmd.OutOrStdout(), map[string]string{"diff": diff})
				}
				fmt.Fprint(cmd.OutOrStdout(), diff)
				return nil
			},
		},
		newRollbackCmd(opts),
	)

	return root
}

func newRollbackCmd(opts *rootOpts) *cobra.Command {
	var deploy bool
	cmd := &cobra.Command{
		Use:   "rollback <stack> <hash>",
		Short: "Restore stack files from a git revision (and optionally deploy)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ok, err := opts.confirm(cmd, fmt.Sprintf("Rollback stack %q to revision %s?", args[0], args[1]))
			if err != nil || !ok {
				return err
			}
			ctx := cmd.Context()
			c, hostID, err := opts.connect(ctx)
			if err != nil {
				return err
			}
			var resp map[string]any
			if err := c.Post(ctx, fmt.Sprintf("/api/hosts/%s/stacks/%s/restore",
				url.PathEscape(hostID), url.PathEscape(args[0])), map[string]any{
				"hash":   args[1],
				"deploy": deploy,
			}, &resp); err != nil {
				return err
			}
			if opts.jsonOut {
				return printJSON(cmd.OutOrStdout(), resp)
			}
			if !opts.quiet {
				fmt.Fprintf(cmd.OutOrStdout(), "Rolled back %s to %s (deployed=%v).\n", args[0], args[1], deploy)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&deploy, "deploy", true, "deploy immediately after restoring files")
	return cmd
}

func newRestoreCmd(opts *rootOpts) *cobra.Command {
	root := &cobra.Command{
		Use:   "restore",
		Short: "List or restore previously deleted stacks",
	}

	root.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List deleted stacks recoverable from git history",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, hostID, err := opts.connect(ctx)
			if err != nil {
				return err
			}
			var del []client.DeletedStack
			if err := c.Get(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/deleted-stacks", &del); err != nil {
				return err
			}
			if opts.jsonOut {
				return printJSON(cmd.OutOrStdout(), del)
			}
			tw := newTab(cmd.OutOrStdout())
			fmt.Fprintln(tw, "STACK\tHASH\tDELETED AT\tAUTHOR")
			for _, d := range del {
				short := d.Hash
				if len(short) > 7 {
					short = short[:7]
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.Name, short, d.At.Local().Format("2006-01-02 15:04"), d.Author)
			}
			return tw.Flush()
		},
	})

	var deploy bool
	applyCmd := &cobra.Command{
		Use:   "stack <name> <hash>",
		Short: "Restore a deleted stack by name and git commit hash",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, hostID, err := opts.connect(ctx)
			if err != nil {
				return err
			}
			var resp map[string]any
			if err := c.Post(ctx, fmt.Sprintf("/api/hosts/%s/deleted-stacks/%s/restore",
				url.PathEscape(hostID), url.PathEscape(args[0])), map[string]any{
				"hash":   args[1],
				"deploy": deploy,
			}, &resp); err != nil {
				return err
			}
			if opts.jsonOut {
				return printJSON(cmd.OutOrStdout(), resp)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Restored stack %s (deploy=%v).\n", args[0], deploy)
			return nil
		},
	}
	applyCmd.Flags().BoolVar(&deploy, "deploy", false, "also deploy immediately after restore")
	root.AddCommand(applyCmd)

	return root
}

// lineDiff computes a clean line-by-line diff between a and b.
func lineDiff(labelA, labelB, a, b string) string {
	linesA := strings.Split(strings.ReplaceAll(a, "\r\n", "\n"), "\n")
	linesB := strings.Split(strings.ReplaceAll(b, "\r\n", "\n"), "\n")
	n, m := len(linesA), len(linesB)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if linesA[i] == linesB[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", labelA, labelB)
	i, j := 0, 0
	for i < n || j < m {
		if i < n && j < m && linesA[i] == linesB[j] {
			fmt.Fprintf(&out, "  %s\n", linesA[i])
			i++
			j++
		} else if j < m && (i >= n || dp[i][j+1] >= dp[i+1][j]) {
			fmt.Fprintf(&out, "+ %s\n", linesB[j])
			j++
		} else {
			fmt.Fprintf(&out, "- %s\n", linesA[i])
			i++
		}
	}
	return out.String()
}
