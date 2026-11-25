package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"moorage/internal/cli/client"
)

func newUpdatesCmd(opts *rootOpts) *cobra.Command {
	root := &cobra.Command{
		Use:     "updates",
		Aliases: []string{"update"},
		Short:   "Check, apply, and roll back stack container image updates",
	}

	root.AddCommand(
		&cobra.Command{
			Use:     "list",
			Aliases: []string{"ls"},
			Short:   "List stacks and image update availability",
			RunE: func(cmd *cobra.Command, _ []string) error {
				ctx := cmd.Context()
				c, _, err := opts.connect(ctx)
				if err != nil {
					return err
				}
				var res struct {
					Stacks []client.StackImages `json:"stacks"`
				}
				if err := c.Get(ctx, "/api/updates", &res); err != nil {
					return err
				}
				if opts.jsonOut {
					return printJSON(cmd.OutOrStdout(), res)
				}
				tw := newTab(cmd.OutOrStdout())
				fmt.Fprintln(tw, "HOST\tSTACK\tPOLICY\tUPDATES")
				for _, s := range res.Stacks {
					avail := 0
					for _, sv := range s.Services {
						if sv.UpdateAvailable || sv.DigestChanged {
							avail++
						}
					}
					disp := c.StackDisplayName(s.HostID, s.Stack, s.DisplayName)
					fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", s.HostName, disp, s.Policy.Mode, avail)
				}
				return tw.Flush()
			},
		},
		&cobra.Command{
			Use:   "check [stack]",
			Short: "Check image updates for all stacks or one stack",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx := cmd.Context()
				c, hostID, err := opts.connect(ctx)
				if err != nil {
					return err
				}
				var res any
				if len(args) == 1 {
					if err := c.Post(ctx, fmt.Sprintf("/api/hosts/%s/stacks/%s/images/check", url.PathEscape(hostID), url.PathEscape(args[0])), nil, &res); err != nil {
						return err
					}
				} else {
					if err := c.Post(ctx, "/api/updates/check", map[string]any{}, &res); err != nil {
						return err
					}
				}
				if opts.jsonOut {
					return printJSON(cmd.OutOrStdout(), res)
				}
				fmt.Fprintln(cmd.OutOrStdout(), "Image update check complete.")
				return nil
			},
		},
		&cobra.Command{
			Use:   "apply <stack>",
			Short: "Pull updated images and recreate a stack",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx := cmd.Context()
				c, hostID, err := opts.connect(ctx)
				if err != nil {
					return err
				}
				var op client.Op
				if err := c.Post(ctx, fmt.Sprintf("/api/hosts/%s/stacks/%s/images/update", url.PathEscape(hostID), url.PathEscape(args[0])), nil, &op); err != nil {
					return err
				}
				if opts.jsonOut {
					return printJSON(cmd.OutOrStdout(), op)
				}
				return streamOp(ctx, c, hostID, op.ID, cmd.OutOrStdout(), opts.quiet)
			},
		},
		&cobra.Command{
			Use:   "rollback <stack>",
			Short: "Roll back last image update on a stack",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx := cmd.Context()
				c, hostID, err := opts.connect(ctx)
				if err != nil {
					return err
				}
				var op client.Op
				if err := c.Post(ctx, fmt.Sprintf("/api/hosts/%s/stacks/%s/images/rollback", url.PathEscape(hostID), url.PathEscape(args[0])), map[string]any{}, &op); err != nil {
					return err
				}
				if opts.jsonOut {
					return printJSON(cmd.OutOrStdout(), op)
				}
				return streamOp(ctx, c, hostID, op.ID, cmd.OutOrStdout(), opts.quiet)
			},
		},
	)

	return root
}

func newDepsCmd(opts *rootOpts) *cobra.Command {
	root := &cobra.Command{
		Use:     "deps",
		Aliases: []string{"dep"},
		Short:   "Inspect and edit stack dependencies",
	}

	var allHosts bool
	lsCmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List stack dependency edges",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, hostID, err := opts.connect(ctx)
			if err != nil {
				return err
			}
			path := "/api/hosts/" + url.PathEscape(hostID) + "/deps"
			if allHosts {
				path = "/api/deps"
			}
			var deps client.HostDeps
			if err := c.Get(ctx, path, &deps); err != nil {
				return err
			}
			if opts.jsonOut {
				return printJSON(cmd.OutOrStdout(), deps)
			}
			tw := newTab(cmd.OutOrStdout())
			fmt.Fprintln(tw, "ID\tFROM\tTO\tSOURCE\tAUTO-RESTART")
			for _, e := range deps.Edges {
				from := c.StackDisplayName(e.From.HostID, e.From.Stack, e.From.DisplayName)
				to := c.StackDisplayName(e.To.HostID, e.To.Stack, e.To.DisplayName)
				fmt.Fprintf(tw, "%d\t%s (%s)\t%s (%s)\t%s\t%s\n",
					e.ID, from, e.From.HostID, to, e.To.HostID, e.Source, e.AutoRestart)
			}
			return tw.Flush()
		},
	}
	lsCmd.Flags().BoolVar(&allHosts, "all-hosts", false, "show edges across all connected hosts")
	root.AddCommand(lsCmd)

	root.AddCommand(&cobra.Command{
		Use:   "detect",
		Short: "Re-detect stack dependencies from compose files on the host",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, hostID, err := opts.connect(ctx)
			if err != nil {
				return err
			}
			var res any
			if err := c.Post(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/deps/detect", nil, &res); err != nil {
				return err
			}
			if opts.jsonOut {
				return printJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Dependency detection complete.")
			return nil
		},
	})

	var toHost string
	addCmd := &cobra.Command{
		Use:   "add <from-stack> <to-stack>",
		Short: "Add a manual dependency edge (<from-stack> depends on <to-stack>)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, hostID, err := opts.connect(ctx)
			if err != nil {
				return err
			}
			if toHost == "" {
				toHost = hostID
			}
			var dep client.StackDep
			if err := c.Post(ctx, fmt.Sprintf("/api/hosts/%s/stacks/%s/deps", url.PathEscape(hostID), url.PathEscape(args[0])), map[string]string{
				"hostId": toHost,
				"stack":  args[1],
			}, &dep); err != nil {
				return err
			}
			if opts.jsonOut {
				return printJSON(cmd.OutOrStdout(), dep)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Added dependency #%d (%s -> %s).\n", dep.ID, args[0], args[1])
			return nil
		},
	}
	addCmd.Flags().StringVar(&toHost, "to-host", "", "target host ID if upstream stack lives on another host")
	root.AddCommand(addCmd)

	root.AddCommand(&cobra.Command{
		Use:     "remove <from-stack> <dep-id>",
		Aliases: []string{"rm"},
		Short:   "Delete a manual dependency edge",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, hostID, err := opts.connect(ctx)
			if err != nil {
				return err
			}
			if err := c.Delete(ctx, fmt.Sprintf("/api/hosts/%s/stacks/%s/deps/%s",
				url.PathEscape(hostID), url.PathEscape(args[0]), url.PathEscape(args[1])), nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed dependency %s.\n", args[1])
			return nil
		},
	})

	return root
}

func newHostsCmd(opts *rootOpts) *cobra.Command {
	root := &cobra.Command{
		Use:     "hosts",
		Aliases: []string{"host"},
		Short:   "List, pair, remove, and prune Docker hosts",
	}

	root.AddCommand(
		&cobra.Command{
			Use:     "list",
			Aliases: []string{"ls"},
			Short:   "List hub and paired agents",
			RunE: func(cmd *cobra.Command, _ []string) error {
				ctx := cmd.Context()
				c, err := opts.client()
				if err != nil {
					return err
				}
				hosts, err := c.Hosts(ctx)
				if err != nil {
					return err
				}
				if opts.jsonOut {
					return printJSON(cmd.OutOrStdout(), hosts)
				}
				tw := newTab(cmd.OutOrStdout())
				fmt.Fprintln(tw, "ID\tNAME\tKIND\tCONNECTED\tREAD-ONLY\tVERSION")
				for _, h := range hosts {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%v\t%v\t%s\n", h.ID, h.Name, h.Kind, h.Connected, h.ReadOnly, h.Version)
				}
				return tw.Flush()
			},
		},
		&cobra.Command{
			Use:   "info",
			Short: "Show engine & system info of the target host",
			RunE: func(cmd *cobra.Command, _ []string) error {
				ctx := cmd.Context()
				c, hostID, err := opts.connect(ctx)
				if err != nil {
					return err
				}
				var info client.HostInfo
				if err := c.Get(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/info", &info); err != nil {
					return err
				}
				if opts.jsonOut {
					return printJSON(cmd.OutOrStdout(), info)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Host: %s (%s/%s)\nEngine: %s %s\nCPUs: %d · Mem: %d MiB\nStacks dir: %s\n",
					info.Name, info.OS, info.Arch, info.Engine, info.EngineVersion, info.CPUs, info.MemBytes/(1024*1024), info.StacksDir)
				return nil
			},
		},
		&cobra.Command{
			Use:   "pair <name>",
			Short: "Issue a one-time join token to pair a new agent",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx := cmd.Context()
				c, err := opts.client()
				if err != nil {
					return err
				}
				var res map[string]any
				if err := c.Post(ctx, "/api/agents", map[string]string{"name": args[0]}, &res); err != nil {
					return err
				}
				if opts.jsonOut {
					return printJSON(cmd.OutOrStdout(), res)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Join token: %v\nExpires:    %v\nAgent port: %v\n",
					res["token"], res["expires"], res["agentPort"])
				return nil
			},
		},
		&cobra.Command{
			Use:     "remove <host-id>",
			Aliases: []string{"rm"},
			Short:   "Remove a paired agent from the hub",
			Args:    cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				ok, err := opts.confirm(cmd, fmt.Sprintf("Remove agent host %q?", args[0]))
				if err != nil || !ok {
					return err
				}
				ctx := cmd.Context()
				c, err := opts.client()
				if err != nil {
					return err
				}
				if err := c.Delete(ctx, "/api/hosts/"+url.PathEscape(args[0]), nil); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Removed host %s.\n", args[0])
				return nil
			},
		},
	)

	var pruneDryRun, pruneVolumes bool
	pruneCmd := &cobra.Command{
		Use:   "prune",
		Short: "Preview or run Docker disk cleanup on the target host",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, hostID, err := opts.connect(ctx)
			if err != nil {
				return err
			}
			if pruneDryRun {
				var prev client.PrunePreview
				if err := c.Get(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/prune/preview", &prev); err != nil {
					return err
				}
				return printJSON(cmd.OutOrStdout(), prev)
			}
			ok, err := opts.confirm(cmd, fmt.Sprintf("Prune unused images/containers/cache (volumes=%v) on %s?", pruneVolumes, hostID))
			if err != nil || !ok {
				return err
			}
			var res any
			if err := c.Post(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/prune", map[string]bool{
				"containers": true,
				"images":     true,
				"networks":   true,
				"buildCache": true,
				"volumes":    pruneVolumes,
			}, &res); err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), res)
		},
	}
	pruneCmd.Flags().BoolVar(&pruneDryRun, "dry-run", false, "preview reclaimable resources without pruning")
	pruneCmd.Flags().BoolVar(&pruneVolumes, "volumes", false, "also prune unused Docker volumes")
	root.AddCommand(pruneCmd)

	return root
}

func newBackupCmd(opts *rootOpts) *cobra.Command {
	root := &cobra.Command{
		Use:   "backup",
		Short: "Export, inspect, and restore encrypted .mbk backups",
	}

	var outPath, password string
	exportCmd := &cobra.Command{
		Use:   "export",
		Short: "Export an encrypted .mbk backup archive",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(password) < 8 {
				return fmt.Errorf("--password must be at least 8 characters")
			}
			ctx := cmd.Context()
			c, err := opts.client()
			if err != nil {
				return err
			}
			body := fmt.Sprintf(`{"password":%q}`, password)
			resp, err := c.PostRaw(ctx, "POST", "/api/settings/backup/export", strings.NewReader(body), map[string]string{
				"Content-Type": "application/json",
			})
			if err != nil {
				return err
			}
			defer resp.Body.Close()

			f, err := os.Create(outPath)
			if err != nil {
				return err
			}
			defer f.Close()
			n, err := io.Copy(f, resp.Body)
			if err != nil {
				return err
			}
			if !opts.quiet {
				fmt.Fprintf(cmd.OutOrStdout(), "Exported %s (%d bytes).\n", outPath, n)
			}
			return nil
		},
	}
	exportCmd.Flags().StringVarP(&outPath, "out", "o", "moorage-backup.mbk", "output .mbk file path")
	exportCmd.Flags().StringVar(&password, "password", "", "backup encryption password (min 8 chars)")
	_ = exportCmd.MarkFlagRequired("password")
	root.AddCommand(exportCmd)

	var prevPass string
	previewCmd := &cobra.Command{
		Use:   "preview <file.mbk>",
		Short: "Decrypt and inspect an .mbk backup archive",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := opts.client()
			if err != nil {
				return err
			}
			prev, err := uploadBackupPreview(ctx, c, args[0], prevPass)
			if err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), prev)
		},
	}
	previewCmd.Flags().StringVar(&prevPass, "password", "", "backup decryption password")
	_ = previewCmd.MarkFlagRequired("password")
	root.AddCommand(previewCmd)

	var resPass string
	var restoreDB, restoreStacks bool
	restoreCmd := &cobra.Command{
		Use:   "restore <file.mbk>",
		Short: "Preview and apply an .mbk backup archive",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !restoreDB && !restoreStacks {
				restoreDB = true
				restoreStacks = true
			}
			ok, err := opts.confirm(cmd, fmt.Sprintf("Restore %s (db=%v, stacks=%v)?", args[0], restoreDB, restoreStacks))
			if err != nil || !ok {
				return err
			}
			ctx := cmd.Context()
			c, err := opts.client()
			if err != nil {
				return err
			}
			prev, err := uploadBackupPreview(ctx, c, args[0], resPass)
			if err != nil {
				return err
			}
			var applyRes map[string]any
			if err := c.Post(ctx, "/api/settings/backup/import/apply", map[string]any{
				"token":    prev.Token,
				"database": restoreDB,
				"stacks":   restoreStacks,
			}, &applyRes); err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), applyRes)
		},
	}
	restoreCmd.Flags().StringVar(&resPass, "password", "", "backup decryption password")
	restoreCmd.Flags().BoolVar(&restoreDB, "db", true, "restore hub database (takes effect on restart)")
	restoreCmd.Flags().BoolVar(&restoreStacks, "stacks", true, "restore stack compose.yaml/.env files")
	_ = restoreCmd.MarkFlagRequired("password")
	root.AddCommand(restoreCmd)

	return root
}

func uploadBackupPreview(ctx context.Context, c *client.Client, filePath, password string) (*client.ImportPreview, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	resp, err := c.PostRaw(ctx, "POST", "/api/settings/backup/import/preview", f, map[string]string{
		"Content-Type":      "application/octet-stream",
		"X-Backup-Password": password,
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var prev client.ImportPreview
	if err := json.NewDecoder(resp.Body).Decode(&prev); err != nil {
		return nil, err
	}
	return &prev, nil
}

func newSystemCmd(opts *rootOpts) *cobra.Command {
	root := &cobra.Command{
		Use:   "system",
		Short: "Check, apply, and roll back Moorage hub/agent self-updates",
	}

	root.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show hub and agent versions and available Moorage release updates",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := opts.client()
			if err != nil {
				return err
			}
			var ov client.SystemUpdatesOverview
			if err := c.Get(ctx, "/api/system/updates", &ov); err != nil {
				return err
			}
			if opts.jsonOut {
				return printJSON(cmd.OutOrStdout(), ov)
			}
			tw := newTab(cmd.OutOrStdout())
			fmt.Fprintln(tw, "NODE\tKIND\tCURRENT\tTARGET\tCAN ROLLBACK")
			for _, n := range ov.Nodes {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%v\n", n.ID, n.Kind, n.CurrentVersion, n.TargetVersion, n.CanRollback)
			}
			return tw.Flush()
		},
	})

	var apply bool
	var targets []string
	updCmd := &cobra.Command{
		Use:   "update",
		Short: "Check for Moorage updates, or apply them (--apply)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := opts.client()
			if err != nil {
				return err
			}
			if !apply {
				var ov client.SystemUpdatesOverview
				if err := c.Post(ctx, "/api/system/updates/check", map[string]any{}, &ov); err != nil {
					return err
				}
				return printJSON(cmd.OutOrStdout(), ov)
			}
			ok, err := opts.confirm(cmd, fmt.Sprintf("Apply Moorage update (targets=%v)?", targets))
			if err != nil || !ok {
				return err
			}
			var res any
			if err := c.Post(ctx, "/api/system/updates/apply", map[string]any{"targets": targets}, &res); err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), res)
		},
	}
	updCmd.Flags().BoolVar(&apply, "apply", false, "apply available Moorage updates")
	updCmd.Flags().StringSliceVar(&targets, "targets", nil, "node IDs to update (empty = all)")
	root.AddCommand(updCmd)

	root.AddCommand(&cobra.Command{
		Use:   "rollback <node>",
		Short: "Roll back a Moorage node (hub or agent ID) to its previous version",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ok, err := opts.confirm(cmd, fmt.Sprintf("Roll back Moorage node %q?", args[0]))
			if err != nil || !ok {
				return err
			}
			ctx := cmd.Context()
			c, err := opts.client()
			if err != nil {
				return err
			}
			var res any
			if err := c.Post(ctx, "/api/system/updates/"+url.PathEscape(args[0])+"/rollback", map[string]any{}, &res); err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), res)
		},
	})

	return root
}

func newTokenCmd(opts *rootOpts) *cobra.Command {
	root := &cobra.Command{
		Use:     "token",
		Aliases: []string{"tokens"},
		Short:   "Manage CLI & remote API bearer tokens",
	}

	root.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List existing API tokens",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := opts.client()
			if err != nil {
				return err
			}
			var res struct {
				Tokens []client.APIToken `json:"tokens"`
			}
			if err := c.Get(ctx, "/api/tokens", &res); err != nil {
				return err
			}
			if opts.jsonOut {
				return printJSON(cmd.OutOrStdout(), res)
			}
			tw := newTab(cmd.OutOrStdout())
			fmt.Fprintln(tw, "ID\tNAME\tPREFIX\tROLE\tSCOPES\tEXPIRES")
			for _, t := range res.Tokens {
				exp := "never"
				if !t.ExpiresAt.IsZero() {
					exp = t.ExpiresAt.Local().Format("2006-01-02")
				}
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Name, t.Prefix, t.Role, strings.Join(t.Scopes, ","), exp)
			}
			return tw.Flush()
		},
	})

	var role string
	var scopes []string
	var expiresDays int
	createCmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new API token (prints the secret once)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := opts.client()
			if err != nil {
				return err
			}
			var res struct {
				Token  client.APIToken `json:"token"`
				Secret string          `json:"secret"`
			}
			if err := c.Post(ctx, "/api/tokens", map[string]any{
				"name":        args[0],
				"role":        role,
				"scopes":      scopes,
				"expiresDays": expiresDays,
			}, &res); err != nil {
				return err
			}
			if opts.jsonOut {
				return printJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created API token %q (id=%d)\nSecret (shown once): %s\n", res.Token.Name, res.Token.ID, res.Secret)
			return nil
		},
	}
	createCmd.Flags().StringVar(&role, "role", "admin", "token role: admin or viewer")
	createCmd.Flags().StringSliceVar(&scopes, "scope", nil, "scopes: read, deploy, admin")
	createCmd.Flags().IntVar(&expiresDays, "expires", 0, "expiry in days (0 = never)")
	root.AddCommand(createCmd)

	root.AddCommand(&cobra.Command{
		Use:     "delete <id>",
		Aliases: []string{"rm", "revoke"},
		Short:   "Revoke an API token",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := opts.client()
			if err != nil {
				return err
			}
			if err := c.Delete(ctx, "/api/tokens/"+url.PathEscape(args[0]), nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted API token %s.\n", args[0])
			return nil
		},
	})

	return root
}

func newAuditCmd(opts *rootOpts) *cobra.Command {
	var stack, actor, action string
	var limit int
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Inspect the hub audit log",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := opts.connectOnlyClient()
			if err != nil {
				return err
			}
			q := url.Values{}
			q.Set("limit", fmt.Sprintf("%d", limit))
			if stack != "" {
				q.Set("stack", stack)
			}
			if actor != "" {
				q.Set("actor", actor)
			}
			if action != "" {
				q.Set("action", action)
			}
			if opts.host != "" {
				q.Set("host", opts.host)
			}
			var res struct {
				Entries []client.AuditEntry `json:"entries"`
			}
			if err := c.Get(ctx, "/api/audit?"+q.Encode(), &res); err != nil {
				return err
			}
			if opts.jsonOut {
				return printJSON(cmd.OutOrStdout(), res)
			}
			tw := newTab(cmd.OutOrStdout())
			fmt.Fprintln(tw, "TIME\tACTOR\tACTION\tHOST/STACK\tTARGET\tOUTCOME")
			for _, e := range res.Entries {
				st := e.StackDisplayName
				if st == "" && e.Stack != "" {
					st = c.StackDisplayName(e.HostID, e.Stack, "")
				}
				place := strings.Trim(e.HostID+"/"+st, "/")
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", e.At, e.Actor, e.Action, place, e.Target, e.Outcome)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&stack, "stack", "", "filter by stack name")
	cmd.Flags().StringVar(&actor, "actor", "", "filter by actor username")
	cmd.Flags().StringVar(&action, "action", "", "filter by action prefix")
	cmd.Flags().IntVar(&limit, "limit", 50, "maximum entries to show")
	return cmd
}

func (o *rootOpts) connectOnlyClient() (*client.Client, error) {
	return o.client()
}
