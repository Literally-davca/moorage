package cmd

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/spf13/cobra"

	"moorage/internal/cli/client"
)

func newStacksCmd(opts *rootOpts) *cobra.Command {
	root := &cobra.Command{
		Use:     "stacks",
		Aliases: []string{"stack", "st"},
		Short:   "Manage Compose stacks (list, deploy, stop, restart, logs, stats)",
	}

	root.AddCommand(
		&cobra.Command{
			Use:     "list",
			Aliases: []string{"ls"},
			Short:   "List stacks on the target host",
			RunE: func(cmd *cobra.Command, _ []string) error {
				ctx := cmd.Context()
				c, hostID, err := opts.connect(ctx)
				if err != nil {
					return err
				}
				var res struct {
					Stacks []client.StackSummary `json:"stacks"`
				}
				if err := c.Get(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/stacks", &res); err != nil {
					return err
				}
				for i := range res.Stacks {
					res.Stacks[i].DisplayName = c.StackDisplayName(hostID, res.Stacks[i].Name, res.Stacks[i].DisplayName)
				}
				if opts.jsonOut {
					return printJSON(cmd.OutOrStdout(), res)
				}
				if opts.quiet {
					for _, s := range res.Stacks {
						fmt.Fprintln(cmd.OutOrStdout(), s.Name)
					}
					return nil
				}
				tw := newTab(cmd.OutOrStdout())
				fmt.Fprintln(tw, "NAME\tSTACK\tSTATUS\tCONTAINERS\tMANAGED")
				for _, s := range res.Stacks {
					disp := s.Name
					if s.DisplayName != "" && s.DisplayName != s.Name {
						disp = s.DisplayName
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%d/%d\t%v\n", disp, s.Name, s.Status, s.Running, s.Total, s.Managed)
				}
				return tw.Flush()
			},
		},
		&cobra.Command{
			Use:   "status <name>",
			Short: "Show stack containers, ports, URLs, and Coolify metadata",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx := cmd.Context()
				c, hostID, err := opts.connect(ctx)
				if err != nil {
					return err
				}
				var st client.Stack
				if err := c.Get(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/stacks/"+url.PathEscape(args[0]), &st); err != nil {
					return err
				}
				st.DisplayName = c.StackDisplayName(hostID, st.Name, st.DisplayName)
				if opts.jsonOut {
					return printJSON(cmd.OutOrStdout(), st)
				}
				out := cmd.OutOrStdout()
				title := st.Name
				if st.DisplayName != "" && st.DisplayName != st.Name {
					title = fmt.Sprintf("%s (%s)", st.DisplayName, st.Name)
				}
				fmt.Fprintf(out, "Stack:   %s on %s\nStatus:  %s (%d/%d running)\nManaged: %v\n",
					title, hostID, st.Status, st.Running, st.Total, st.Managed)
				if st.CoolifyProject != "" {
					fmt.Fprintf(out, "Coolify: %s (%s)\n", st.CoolifyProject, st.CoolifyEnv)
				}
				if len(st.URLs) > 0 {
					fmt.Fprintf(out, "URLs:    %s\n", strings.Join(st.URLs, ", "))
				}
				if len(st.Containers) > 0 {
					fmt.Fprintln(out)
					tw := newTab(out)
					fmt.Fprintln(tw, "SERVICE\tCONTAINER\tSTATE\tHEALTH\tIMAGE\tPORTS")
					for _, ctr := range st.Containers {
						fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
							ctr.Service, ctr.Name, ctr.State, ctr.Health, ctr.Image, strings.Join(ctr.Ports, ","))
					}
					return tw.Flush()
				}
				return nil
			},
		},
		newStackActionCmd(opts, "up", "Deploy / start a stack (`docker compose up -d`)", true),
		newStackActionCmd(opts, "down", "Stop and remove stack containers & networks", false, "down"),
		newStackActionCmd(opts, "restart", "Restart stack containers", false),
		newStackActionCmd(opts, "stop", "Stop running stack containers", false, "stop"),
		newStackActionCmd(opts, "start", "Start stack containers", false),
		newStackActionCmd(opts, "pull", "Pull stack images", false),
		newStackActionCmd(opts, "update", "Pull images and recreate stack containers", false),
	)

	// Delete stack
	root.AddCommand(&cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"rm"},
		Short:   "Delete a managed stack from host",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ok, err := opts.confirm(cmd, fmt.Sprintf("Delete stack %q?", args[0]))
			if err != nil || !ok {
				return err
			}
			ctx := cmd.Context()
			c, hostID, err := opts.connect(ctx)
			if err != nil {
				return err
			}
			if err := c.Delete(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/stacks/"+url.PathEscape(args[0]), nil); err != nil {
				return err
			}
			if !opts.quiet {
				fmt.Fprintf(cmd.OutOrStdout(), "Deleted stack %s.\n", args[0])
			}
			return nil
		},
	})

	// Logs
	var tail int
	var follow bool
	var since, container string
	logsCmd := &cobra.Command{
		Use:   "logs [stack]",
		Short: "Print or follow container logs for a stack or entire host",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			stackName := ""
			if len(args) > 0 {
				stackName = args[0]
			}
			ctx := cmd.Context()
			c, hostID, err := opts.connect(ctx)
			if err != nil {
				return err
			}
			q := url.Values{}
			if stackName != "" {
				q.Set("stack", stackName)
			}
			if container != "" {
				q.Set("container", container)
			}
			if since != "" {
				q.Set("since", since)
			}
			q.Set("tail", fmt.Sprintf("%d", tail))

			if !follow {
				var res struct {
					Lines []client.LogLine `json:"lines"`
				}
				if err := c.Get(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/logs/history?"+q.Encode(), &res); err != nil {
					return err
				}
				if opts.jsonOut {
					return printJSON(cmd.OutOrStdout(), res)
				}
				for _, l := range res.Lines {
					printLogLine(cmd.OutOrStdout(), l)
				}
				return nil
			}

			conn, err := c.DialWS(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/ws/logs?"+q.Encode())
			if err != nil {
				return err
			}
			defer conn.Close(websocket.StatusNormalClosure, "")
			for {
				var frame struct {
					Lines []client.LogLine `json:"lines"`
					End   string           `json:"end"`
				}
				if err := wsjson.Read(ctx, conn, &frame); err != nil {
					if errors.Is(err, io.EOF) || ctx.Err() != nil {
						return nil
					}
					return nil
				}
				for _, l := range frame.Lines {
					printLogLine(cmd.OutOrStdout(), l)
				}
				if frame.End != "" {
					return nil
				}
			}
		},
	}
	logsCmd.Flags().IntVar(&tail, "tail", 200, "number of lines from the end")
	logsCmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream live log output")
	logsCmd.Flags().StringVar(&since, "since", "", "show logs since duration or timestamp (e.g. 1h)")
	logsCmd.Flags().StringVar(&container, "container", "", "filter by container name")
	root.AddCommand(logsCmd)

	// Stats
	root.AddCommand(&cobra.Command{
		Use:   "stats <stack>",
		Short: "Show container CPU, memory, network and block I/O stats",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, hostID, err := opts.connect(ctx)
			if err != nil {
				return err
			}
			var stats []client.ContainerStat
			if err := c.Get(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/stacks/"+url.PathEscape(args[0])+"/stats", &stats); err != nil {
				return err
			}
			if opts.jsonOut {
				return printJSON(cmd.OutOrStdout(), stats)
			}
			tw := newTab(cmd.OutOrStdout())
			fmt.Fprintln(tw, "CONTAINER\tCPU %\tMEM\tMEM %\tNET RX/TX")
			for _, st := range stats {
				name := st.Service
				if name == "" {
					name = st.Name
				}
				fmt.Fprintf(tw, "%s\t%.1f%%\t%.1f MiB\t%.1f%%\t%d / %d B\n",
					name, st.CPUPercent, float64(st.MemUsage)/(1024*1024), st.MemPercent, st.NetRx, st.NetTx)
			}
			return tw.Flush()
		},
	})

	return root
}

func newStackActionCmd(opts *rootOpts, action, short string, hasStrategy bool, confirmActions ...string) *cobra.Command {
	var noWait bool
	var strategy string
	cmd := &cobra.Command{
		Use:   action + " <name>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, ca := range confirmActions {
				if ca == action {
					ok, err := opts.confirm(cmd, fmt.Sprintf("Run %s on stack %q?", action, args[0]))
					if err != nil || !ok {
						return err
					}
				}
			}
			ctx := cmd.Context()
			c, hostID, err := opts.connect(ctx)
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/api/hosts/%s/stacks/%s/actions/%s", url.PathEscape(hostID), url.PathEscape(args[0]), action)
			if strategy != "" {
				path += "?strategy=" + url.QueryEscape(strategy)
			}
			var op client.Op
			if err := c.Post(ctx, path, nil, &op); err != nil {
				return err
			}
			if opts.jsonOut {
				return printJSON(cmd.OutOrStdout(), op)
			}
			if noWait {
				if !opts.quiet {
					fmt.Fprintf(cmd.OutOrStdout(), "Started %s (%s)\n", action, op.ID)
				}
				return nil
			}
			return streamOp(ctx, c, hostID, op.ID, cmd.OutOrStdout(), opts.quiet)
		},
	}
	if action == "up" {
		cmd.Aliases = []string{"deploy"}
	}
	if hasStrategy {
		cmd.Flags().StringVar(&strategy, "strategy", "", "deploy strategy: standard or rolling")
	}
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "do not wait for operation stream to finish")
	return cmd
}

func printLogLine(w io.Writer, l client.LogLine) {
	if l.Time != "" {
		fmt.Fprintf(w, "%s ", l.Time)
	}
	if l.Service != "" {
		fmt.Fprintf(w, "[%s] ", l.Service)
	} else if l.Container != "" {
		fmt.Fprintf(w, "[%s] ", l.Container)
	}
	fmt.Fprintln(w, l.Msg)
}
