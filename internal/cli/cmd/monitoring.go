package cmd

import (
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"moorage/internal/cli/client"
)

func newChecksCmd(opts *rootOpts) *cobra.Command {
	root := &cobra.Command{
		Use:     "checks",
		Aliases: []string{"check"},
		Short:   "Manage uptime and log monitoring checks",
	}

	root.AddCommand(
		&cobra.Command{
			Use:     "list",
			Aliases: []string{"ls"},
			Short:   "List monitoring checks and current status",
			RunE: func(cmd *cobra.Command, _ []string) error {
				ctx := cmd.Context()
				c, _, err := opts.connect(ctx)
				if err != nil {
					return err
				}
				var list []client.Check
				if err := c.Get(ctx, "/api/checks", &list); err != nil {
					return err
				}
				if opts.jsonOut {
					return printJSON(cmd.OutOrStdout(), list)
				}
				if opts.quiet {
					for _, ch := range list {
						fmt.Fprintln(cmd.OutOrStdout(), ch.ID)
					}
					return nil
				}
				tw := newTab(cmd.OutOrStdout())
				fmt.Fprintln(tw, "ID\tNAME\tKIND\tSTATUS\tENABLED\tTARGET")
				for _, ch := range list {
					target := ch.Target
					if ch.TargetDisplayName != "" {
						target = ch.TargetDisplayName
					} else if ch.Stack != "" {
						target = c.StackDisplayName(ch.HostID, ch.Stack, "")
					}
					fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%v\t%s\n", ch.ID, ch.Name, ch.Kind, ch.State.Status, ch.Enabled, target)
				}
				return tw.Flush()
			},
		},
		&cobra.Command{
			Use:   "get <id>",
			Short: "Show details of one check",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx := cmd.Context()
				c, _, err := opts.connect(ctx)
				if err != nil {
					return err
				}
				var ch client.Check
				if err := c.Get(ctx, "/api/checks/"+args[0], &ch); err != nil {
					return err
				}
				if opts.jsonOut {
					return printJSON(cmd.OutOrStdout(), ch)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Check #%d: %s (%s)\nStatus: %s (enabled=%v)\nTarget: %s\n",
					ch.ID, ch.Name, ch.Kind, ch.State.Status, ch.Enabled, ch.Target)
				if ch.State.Last != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "Last:   %s · %dms · %s\n", ch.State.Last.Status, ch.State.Last.LatencyMs, ch.State.Last.Msg)
				}
				return nil
			},
		},
		&cobra.Command{
			Use:   "test <id>",
			Short: "Run a saved check on demand",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx := cmd.Context()
				c, _, err := opts.connect(ctx)
				if err != nil {
					return err
				}
				var ch client.Check
				if err := c.Get(ctx, "/api/checks/"+args[0], &ch); err != nil {
					return err
				}
				var res any
				if err := c.Post(ctx, "/api/checks/test", ch, &res); err != nil {
					return err
				}
				if opts.jsonOut {
					return printJSON(cmd.OutOrStdout(), res)
				}
				return printJSON(cmd.OutOrStdout(), res)
			},
		},
		newCheckToggleCmd(opts, "pause", false),
		newCheckToggleCmd(opts, "resume", true),
		&cobra.Command{
			Use:     "delete <id>",
			Aliases: []string{"rm"},
			Short:   "Delete a check",
			Args:    cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				id, err := strconv.ParseInt(args[0], 10, 64)
				if err != nil || id <= 0 {
					return fmt.Errorf("invalid check id %q", args[0])
				}
				ok, err := opts.confirm(cmd, fmt.Sprintf("Delete check #%d?", id))
				if err != nil || !ok {
					return err
				}
				ctx := cmd.Context()
				c, _, err := opts.connect(ctx)
				if err != nil {
					return err
				}
				if err := c.Delete(ctx, fmt.Sprintf("/api/checks/%d", id), nil); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Deleted check #%d.\n", id)
				return nil
			},
		},
	)

	return root
}

func newCheckToggleCmd(opts *rootOpts, verb string, enable bool) *cobra.Command {
	return &cobra.Command{
		Use:   verb + " <id>",
		Short: fmt.Sprintf("%s a monitoring check", verb),
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || id <= 0 {
				return fmt.Errorf("invalid check id %q", args[0])
			}
			ctx := cmd.Context()
			c, _, err := opts.connect(ctx)
			if err != nil {
				return err
			}
			var ch client.Check
			if err := c.Get(ctx, fmt.Sprintf("/api/checks/%d", id), &ch); err != nil {
				return err
			}
			ch.Enabled = enable
			if err := c.Put(ctx, fmt.Sprintf("/api/checks/%d", id), ch, nil); err != nil {
				return err
			}
			if !opts.quiet {
				fmt.Fprintf(cmd.OutOrStdout(), "Check #%d %sd.\n", id, verb)
			}
			return nil
		},
	}
}

func newMaintenanceCmd(opts *rootOpts) *cobra.Command {
	root := &cobra.Command{
		Use:     "maintenance",
		Aliases: []string{"maint"},
		Short:   "Manage planned maintenance windows",
	}

	root.AddCommand(
		&cobra.Command{
			Use:     "list",
			Aliases: []string{"ls"},
			Short:   "List maintenance windows",
			RunE: func(cmd *cobra.Command, _ []string) error {
				ctx := cmd.Context()
				c, _, err := opts.connect(ctx)
				if err != nil {
					return err
				}
				var list []client.MaintenanceWindow
				if err := c.Get(ctx, "/api/maintenance", &list); err != nil {
					return err
				}
				if opts.jsonOut {
					return printJSON(cmd.OutOrStdout(), list)
				}
				tw := newTab(cmd.OutOrStdout())
				fmt.Fprintln(tw, "ID\tTITLE\tSTATE\tSTART\tEND")
				for _, m := range list {
					fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", m.ID, m.Title, m.State, m.Start, m.End)
				}
				return tw.Flush()
			},
		},
		&cobra.Command{
			Use:     "delete <id>",
			Aliases: []string{"rm"},
			Short:   "Delete a maintenance window",
			Args:    cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx := cmd.Context()
				c, _, err := opts.connect(ctx)
				if err != nil {
					return err
				}
				if err := c.Delete(ctx, "/api/maintenance/"+args[0], nil); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Deleted maintenance window %s.\n", args[0])
				return nil
			},
		},
	)

	var title, message, durStr string
	var all bool
	var stacks []string
	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Create a one-shot maintenance window starting now",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dur, err := time.ParseDuration(durStr)
			if err != nil || dur < 5*time.Minute {
				return fmt.Errorf("invalid --duration %q (must be >= 5m, e.g. 1h)", durStr)
			}
			ctx := cmd.Context()
			c, hostID, err := opts.connect(ctx)
			if err != nil {
				return err
			}
			var stackRefs []map[string]string
			for _, st := range stacks {
				stackRefs = append(stackRefs, map[string]string{"hostId": hostID, "stack": st})
			}
			now := time.Now().UTC()
			payload := map[string]any{
				"title":   title,
				"message": message,
				"kind":    "once",
				"start":   now.Format(time.RFC3339),
				"end":     now.Add(dur).Format(time.RFC3339),
				"enabled": true,
				"scope": map[string]any{
					"all":    all || len(stackRefs) == 0,
					"stacks": stackRefs,
				},
			}
			var res map[string]int64
			if err := c.Post(ctx, "/api/maintenance", payload, &res); err != nil {
				return err
			}
			if opts.jsonOut {
				return printJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created maintenance window #%d (%s).\n", res["id"], title)
			return nil
		},
	}
	createCmd.Flags().StringVar(&title, "title", "", "window title")
	_ = createCmd.MarkFlagRequired("title")
	createCmd.Flags().StringVar(&message, "message", "", "public status page message")
	createCmd.Flags().StringVar(&durStr, "duration", "1h", "window duration (e.g. 30m, 2h)")
	createCmd.Flags().BoolVar(&all, "all", false, "cover everything")
	createCmd.Flags().StringArrayVar(&stacks, "stack", nil, "stack name to cover (repeatable)")
	root.AddCommand(createCmd)

	return root
}
