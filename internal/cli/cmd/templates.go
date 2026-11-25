package cmd

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"moorage/internal/cli/client"
)

func newTemplatesCmd(opts *rootOpts) *cobra.Command {
	root := &cobra.Command{
		Use:     "templates",
		Aliases: []string{"tpl"},
		Short:   "Browse, inspect, and deploy stack templates",
	}

	root.AddCommand(
		&cobra.Command{
			Use:     "list",
			Aliases: []string{"ls"},
			Short:   "List templates from local and remote sources",
			RunE: func(cmd *cobra.Command, _ []string) error {
				ctx := cmd.Context()
				c, _, err := opts.connect(ctx)
				if err != nil {
					return err
				}
				var cat client.TemplateCatalog
				if err := c.Get(ctx, "/api/templates", &cat); err != nil {
					return err
				}
				if opts.jsonOut {
					return printJSON(cmd.OutOrStdout(), cat)
				}
				if opts.quiet {
					for _, t := range cat.Templates {
						fmt.Fprintf(cmd.OutOrStdout(), "%s/%s\n", t.Source, t.ID)
					}
					return nil
				}
				tw := newTab(cmd.OutOrStdout())
				fmt.Fprintln(tw, "ID\tNAME\tCATEGORY\tSOURCE")
				for _, t := range cat.Templates {
					fmt.Fprintf(tw, "%s/%s\t%s\t%s\t%s\n", t.Source, t.ID, t.Name, t.Category, t.SourceLabel)
				}
				return tw.Flush()
			},
		},
		&cobra.Command{
			Use:   "inspect <source>/<id>",
			Short: "Show template variables and compose preview",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				src, id, ok := strings.Cut(args[0], "/")
				if !ok {
					return fmt.Errorf("expected <source>/<id>, got %q", args[0])
				}
				ctx := cmd.Context()
				c, hostID, err := opts.connect(ctx)
				if err != nil {
					return err
				}
				var det client.TemplateDetail
				if err := c.Get(ctx, fmt.Sprintf("/api/templates/%s/%s?host=%s", url.PathEscape(src), url.PathEscape(id), url.QueryEscape(hostID)), &det); err != nil {
					return err
				}
				if opts.jsonOut {
					return printJSON(cmd.OutOrStdout(), det)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s (%s/%s)\n%s\n\nVariables:\n", det.Template.Name, det.Template.Source, det.Template.ID, det.Template.Description)
				for _, v := range det.Variables {
					def := v.Default
					if gen, ok := det.Generated[v.Name]; ok && gen != "" {
						def = gen
					}
					req := ""
					if v.Required {
						req = " [required]"
					}
					fmt.Fprintf(cmd.OutOrStdout(), "  %s=%q  (%s)%s\n", v.Name, def, v.Label, req)
				}
				return nil
			},
		},
	)

	var stackName string
	var vars []string
	var deployUp bool
	deployCmd := &cobra.Command{
		Use:   "deploy <source>/<id>",
		Short: "Render a template and create a stack on the target host",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			src, id, ok := strings.Cut(args[0], "/")
			if !ok {
				return fmt.Errorf("expected <source>/<id>, got %q", args[0])
			}
			if strings.TrimSpace(stackName) == "" {
				stackName = id
			}
			ctx := cmd.Context()
			c, hostID, err := opts.connect(ctx)
			if err != nil {
				return err
			}
			var det client.TemplateDetail
			if err := c.Get(ctx, fmt.Sprintf("/api/templates/%s/%s?host=%s", url.PathEscape(src), url.PathEscape(id), url.QueryEscape(hostID)), &det); err != nil {
				return err
			}
			vals := map[string]string{}
			for k, v := range det.Generated {
				vals[k] = v
			}
			for _, v := range det.Variables {
				if _, exists := vals[v.Name]; !exists && v.Default != "" {
					vals[v.Name] = v.Default
				}
			}
			for _, kv := range vars {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return fmt.Errorf("invalid --var %q: expected KEY=VALUE", kv)
				}
				vals[strings.TrimSpace(k)] = v
			}
			var rendered struct {
				Compose string   `json:"compose"`
				Env     string   `json:"env"`
				Notes   string   `json:"notes"`
				Warnings []string `json:"warnings"`
			}
			if err := c.Post(ctx, "/api/templates/render", map[string]any{
				"source": src,
				"id":     id,
				"host":   hostID,
				"values": vals,
			}, &rendered); err != nil {
				return err
			}
			if err := c.Post(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/stacks", map[string]any{
				"name":     stackName,
				"compose":  rendered.Compose,
				"env":      rendered.Env,
				"template": map[string]string{"source": src, "id": id},
			}, nil); err != nil {
				return err
			}
			if deployUp {
				var op client.Op
				if err := c.Post(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/stacks/"+url.PathEscape(stackName)+"/actions/up", nil, &op); err != nil {
					return err
				}
				if err := streamOp(ctx, c, hostID, op.ID, cmd.OutOrStdout(), opts.quiet); err != nil {
					return err
				}
			}
			if opts.jsonOut {
				return printJSON(cmd.OutOrStdout(), map[string]any{"stack": stackName, "host": hostID, "deployed": deployUp, "notes": rendered.Notes})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created stack %s on %s.\n", stackName, hostID)
			if rendered.Notes != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Notes: %s\n", rendered.Notes)
			}
			return nil
		},
	}
	deployCmd.Flags().StringVar(&stackName, "name", "", "stack name to create (defaults to template id)")
	deployCmd.Flags().StringArrayVar(&vars, "var", nil, "template variable KEY=VALUE (repeatable)")
	deployCmd.Flags().BoolVar(&deployUp, "up", false, "also start the stack after creating it")
	root.AddCommand(deployCmd)

	return root
}
