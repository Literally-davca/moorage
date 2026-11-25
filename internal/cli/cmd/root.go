package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/spf13/cobra"

	"moorage/internal/cli/client"
)

type rootOpts struct {
	host    string
	url     string
	token   string
	socket  string
	dataDir string
	jsonOut bool
	quiet   bool
	yes     bool
	version string
}

type ServerHooks struct {
	RunServer func(mode string) error
}

// Execute builds and executes the Moorage CLI tree.
func Execute(version string, args []string, hooks ServerHooks) error {
	root := newRootCmd(version, hooks)
	root.SetArgs(args)
	return root.Execute()
}

func newRootCmd(version string, hooks ServerHooks) *cobra.Command {
	opts := &rootOpts{version: version}

	root := &cobra.Command{
		Use:           "moorage",
		Short:         "Moorage – Docker Compose stacks, live logs, monitoring & CLI",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.StringVarP(&opts.host, "host", "H", "", "target host id (defaults to local or first connected host)")
	pf.StringVar(&opts.url, "url", "", "remote Moorage URL (e.g. https://moorage.example.com) or MOORAGE_URL")
	pf.StringVar(&opts.token, "token", "", "API bearer token (mrg_...) or MOORAGE_TOKEN")
	pf.StringVar(&opts.socket, "socket", "", "local Unix socket path or MOORAGE_SOCKET")
	pf.StringVar(&opts.dataDir, "data-dir", "", "Moorage data directory (for finding moorage.sock)")
	pf.BoolVar(&opts.jsonOut, "json", false, "output raw JSON")
	pf.BoolVarP(&opts.quiet, "quiet", "q", false, "minimal output (ids/names only)")
	pf.BoolVarP(&opts.yes, "yes", "y", false, "skip confirmation prompts")

	if hooks.RunServer != nil {
		root.AddCommand(
			&cobra.Command{
				Use:   "all-in-one",
				Short: "Run Moorage server + local host agent (uses MOORAGE_WEB_UI / MOORAGE_LISTEN / Unix socket)",
				RunE:  func(_ *cobra.Command, _ []string) error { return hooks.RunServer("all-in-one") },
			},
			&cobra.Command{
				Use:   "hub",
				Short: "Run Moorage hub only (other hosts run `moorage agent`)",
				RunE:  func(_ *cobra.Command, _ []string) error { return hooks.RunServer("hub") },
			},
			&cobra.Command{
				Use:   "agent",
				Short: "Run Moorage host agent and connect to MOORAGE_HUB",
				RunE:  func(_ *cobra.Command, _ []string) error { return hooks.RunServer("agent") },
			},
		)
	}

	root.AddCommand(
		&cobra.Command{
			Use:   "version",
			Short: "Print Moorage version",
			Run: func(cmd *cobra.Command, _ []string) {
				if opts.jsonOut {
					_ = printJSON(cmd.OutOrStdout(), map[string]string{"version": opts.version})
					return
				}
				fmt.Fprintln(cmd.OutOrStdout(), opts.version)
			},
		},
		newStacksCmd(opts),
		newComposeCmd(opts),
		newHistoryCmd(opts),
		newRollbackCmd(opts),
		newRestoreCmd(opts),
		newConvertCmd(opts),
		newTemplatesCmd(opts),
		newChecksCmd(opts),
		newMaintenanceCmd(opts),
		newUpdatesCmd(opts),
		newDepsCmd(opts),
		newHostsCmd(opts),
		newBackupCmd(opts),
		newSystemCmd(opts),
		newTokenCmd(opts),
		newAuditCmd(opts),
		newCLITuiCmd(opts),
	)
	return root
}

func (o *rootOpts) client() (*client.Client, error) {
	return client.New(client.Options{
		URL:     o.url,
		Token:   o.token,
		Socket:  o.socket,
		DataDir: o.dataDir,
		Host:    o.host,
	})
}

func (o *rootOpts) connect(ctx context.Context) (*client.Client, string, error) {
	c, err := o.client()
	if err != nil {
		return nil, "", err
	}
	h, err := c.ResolveHost(ctx, o.host)
	if err != nil {
		return nil, "", err
	}
	c.LoadCoolifyNames(ctx)
	return c, h, nil
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func newTab(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
}

func (o *rootOpts) confirm(cmd *cobra.Command, prompt string) (bool, error) {
	if o.yes {
		return true, nil
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "%s [y/N]: ", prompt)
	r := bufio.NewReader(cmd.InOrStdin())
	line, err := r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}

func streamOp(ctx context.Context, c *client.Client, hostID, opID string, out io.Writer, quiet bool) error {
	conn, err := c.DialWS(ctx, fmt.Sprintf("/api/hosts/%s/ws/ops/%s", hostID, opID))
	if err != nil {
		return fmt.Errorf("stream operation %s: %w", opID, err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	for {
		var ev struct {
			Lines []string `json:"lines"`
			Done  bool     `json:"done"`
			Error string   `json:"error"`
		}
		if err := wsjson.Read(ctx, conn, &ev); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return nil
		}
		if !quiet {
			for _, line := range ev.Lines {
				fmt.Fprintln(out, line)
			}
		}
		if ev.Done {
			if ev.Error != "" {
				return errors.New(ev.Error)
			}
			return nil
		}
	}
}
