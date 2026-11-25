package cmd

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"moorage/internal/cli/client"
	"moorage/internal/lint"
)

func newComposeCmd(opts *rootOpts) *cobra.Command {
	root := &cobra.Command{
		Use:   "compose",
		Short: "Inspect, edit, validate, lint, and clean compose.yaml & .env",
	}

	// get
	var showEnv bool
	getCmd := &cobra.Command{
		Use:   "get <stack>",
		Short: "Print compose.yaml (or .env with --env) of a stack",
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
			if opts.jsonOut {
				return printJSON(cmd.OutOrStdout(), map[string]string{"compose": st.Compose, "env": st.Env})
			}
			if showEnv {
				fmt.Fprint(cmd.OutOrStdout(), st.Env)
			} else {
				fmt.Fprint(cmd.OutOrStdout(), st.Compose)
			}
			return nil
		},
	}
	getCmd.Flags().BoolVar(&showEnv, "env", false, "print .env instead of compose.yaml")
	root.AddCommand(getCmd)

	// set
	var composeFile, envFile string
	var createNew bool
	setCmd := &cobra.Command{
		Use:   "set <stack>",
		Short: "Write compose.yaml and/or .env from files to a stack",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if composeFile == "" && envFile == "" {
				return errors.New("pass -f/--file and/or --env-file")
			}
			ctx := cmd.Context()
			c, hostID, err := opts.connect(ctx)
			if err != nil {
				return err
			}
			var composeText, envText string
			if !createNew {
				var cur client.Stack
				if err := c.Get(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/stacks/"+url.PathEscape(args[0]), &cur); err == nil {
					composeText = cur.Compose
					envText = cur.Env
				}
			}
			if composeFile != "" {
				b, err := os.ReadFile(composeFile)
				if err != nil {
					return err
				}
				composeText = string(b)
			}
			if envFile != "" {
				b, err := os.ReadFile(envFile)
				if err != nil {
					return err
				}
				envText = string(b)
			}
			if createNew {
				if err := c.Post(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/stacks", map[string]string{
					"name":    args[0],
					"compose": composeText,
					"env":     envText,
				}, nil); err != nil {
					return err
				}
			} else {
				if err := c.Put(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/stacks/"+url.PathEscape(args[0]), map[string]string{
					"name":    args[0],
					"compose": composeText,
					"env":     envText,
				}, nil); err != nil {
					return err
				}
			}
			if !opts.quiet {
				fmt.Fprintf(cmd.OutOrStdout(), "Saved %s on %s.\n", args[0], hostID)
			}
			return nil
		},
	}
	setCmd.Flags().StringVarP(&composeFile, "file", "f", "", "path to compose.yaml")
	setCmd.Flags().StringVar(&envFile, "env-file", "", "path to .env")
	setCmd.Flags().BoolVar(&createNew, "create", false, "create a new stack instead of updating existing")
	root.AddCommand(setCmd)

	// edit
	var editEnv bool
	editCmd := &cobra.Command{
		Use:   "edit <stack>",
		Short: "Edit stack compose.yaml (or .env with --env) in $VISUAL/$EDITOR",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, hostID, err := opts.connect(ctx)
			if err != nil {
				return err
			}
			var cur client.Stack
			if err := c.Get(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/stacks/"+url.PathEscape(args[0]), &cur); err != nil {
				return err
			}
			ext := ".yaml"
			orig := cur.Compose
			if editEnv {
				ext = ".env"
				orig = cur.Env
			}
			f, err := os.CreateTemp("", "moorage-*"+ext)
			if err != nil {
				return err
			}
			tmpPath := f.Name()
			defer os.Remove(tmpPath)
			if _, err := f.WriteString(orig); err != nil {
				_ = f.Close()
				return err
			}
			_ = f.Close()

			ed := firstNonEmptyEnv("VISUAL", "EDITOR")
			if ed == "" {
				ed = "vi"
			}
			ec := exec.CommandContext(ctx, ed, tmpPath)
			ec.Stdin = cmd.InOrStdin()
			ec.Stdout = cmd.OutOrStdout()
			ec.Stderr = cmd.ErrOrStderr()
			if err := ec.Run(); err != nil {
				return err
			}
			updatedBytes, err := os.ReadFile(tmpPath)
			if err != nil {
				return err
			}
			updated := string(updatedBytes)
			if updated == orig {
				fmt.Fprintln(cmd.OutOrStdout(), "No changes.")
				return nil
			}
			newCompose, newEnv := cur.Compose, cur.Env
			if editEnv {
				newEnv = updated
			} else {
				newCompose = updated
			}
			if err := c.Put(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/stacks/"+url.PathEscape(args[0]), map[string]string{
				"name":    args[0],
				"compose": newCompose,
				"env":     newEnv,
			}, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Saved %s on %s.\n", args[0], hostID)
			return nil
		},
	}
	editCmd.Flags().BoolVar(&editEnv, "env", false, "edit .env instead of compose.yaml")
	root.AddCommand(editCmd)

	// validate
	var localMode bool
	var valEnvFile string
	valCmd := &cobra.Command{
		Use:   "validate <stack-or-file>",
		Short: "Validate & lint a stack remotely or a local compose.yaml (--local works offline)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			var issues []lint.Issue
			if localMode {
				cb, err := os.ReadFile(args[0])
				if err != nil {
					return err
				}
				var envText string
				if valEnvFile != "" {
					eb, err := os.ReadFile(valEnvFile)
					if err != nil {
						return err
					}
					envText = string(eb)
				}
				issues = lint.Lint(ctx, string(cb), envText, lint.Options{Name: "local"})
			} else {
				c, hostID, err := opts.connect(ctx)
				if err != nil {
					return err
				}
				var cur client.Stack
				if err := c.Get(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/stacks/"+url.PathEscape(args[0]), &cur); err != nil {
					return err
				}
				var res struct {
					Issues []lint.Issue `json:"issues"`
				}
				if err := c.Post(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/stacks/"+url.PathEscape(args[0])+"/validate", map[string]any{
					"compose": cur.Compose,
					"env":     cur.Env,
					"deploy":  true,
				}, &res); err != nil {
					return err
				}
				issues = res.Issues
			}
			if opts.jsonOut {
				return printJSON(cmd.OutOrStdout(), issues)
			}
			if len(issues) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "OK: no linter issues.")
				return nil
			}
			tw := newTab(cmd.OutOrStdout())
			fmt.Fprintln(tw, "SEVERITY\tCODE\tLINE\tMESSAGE")
			for _, iss := range issues {
				fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", iss.Severity, iss.Code, iss.Line, iss.Message)
			}
			_ = tw.Flush()
			if lint.HasErrors(issues) {
				return errors.New("validation failed with errors")
			}
			return nil
		},
	}
	valCmd.Flags().BoolVar(&localMode, "local", false, "validate a local file offline without connecting to Moorage")
	valCmd.Flags().StringVar(&valEnvFile, "env-file", "", "optional local .env file for --local")
	root.AddCommand(valCmd)

	// lint (--fix)
	var fixKind, fixService string
	var fixLine int
	var applySave bool
	lintCmd := &cobra.Command{
		Use:   "lint <stack>",
		Short: "Inspect linter issues on a stack or apply a 1-click quick fix (--fix)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, hostID, err := opts.connect(ctx)
			if err != nil {
				return err
			}
			var cur client.Stack
			if err := c.Get(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/stacks/"+url.PathEscape(args[0]), &cur); err != nil {
				return err
			}
			if fixKind != "" {
				var fixRes struct {
					Compose string `json:"compose"`
					Env     string `json:"env"`
					Changed bool   `json:"changed"`
				}
				if err := c.Post(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/stacks/"+url.PathEscape(args[0])+"/lint/fix", map[string]any{
					"compose": cur.Compose,
					"env":     cur.Env,
					"fix": map[string]any{
						"kind":    fixKind,
						"service": fixService,
						"line":    fixLine,
					},
				}, &fixRes); err != nil {
					return err
				}
				if applySave && fixRes.Changed {
					if err := c.Put(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/stacks/"+url.PathEscape(args[0]), map[string]string{
						"name":    args[0],
						"compose": fixRes.Compose,
						"env":     fixRes.Env,
					}, nil); err != nil {
						return err
					}
				}
				if opts.jsonOut {
					return printJSON(cmd.OutOrStdout(), fixRes)
				}
				fmt.Fprint(cmd.OutOrStdout(), fixRes.Compose)
				return nil
			}
			var res struct {
				Issues []client.LintIssue `json:"issues"`
			}
			if err := c.Post(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/stacks/"+url.PathEscape(args[0])+"/validate", map[string]any{
				"compose": cur.Compose,
				"env":     cur.Env,
				"deploy":  true,
			}, &res); err != nil {
				return err
			}
			if opts.jsonOut {
				return printJSON(cmd.OutOrStdout(), res)
			}
			tw := newTab(cmd.OutOrStdout())
			fmt.Fprintln(tw, "SEVERITY\tCODE\tFIX KIND\tSERVICE\tMESSAGE")
			for _, i := range res.Issues {
				fk := ""
				if i.Fix != nil {
					fk = i.Fix.Kind
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", i.Severity, i.Code, fk, i.Service, i.Message)
			}
			return tw.Flush()
		},
	}
	lintCmd.Flags().StringVar(&fixKind, "fix", "", "apply linter fix kind (e.g. add-mem-limit, add-healthcheck, pin-tag, add-env-var)")
	lintCmd.Flags().StringVar(&fixService, "service", "", "service name for --fix")
	lintCmd.Flags().IntVar(&fixLine, "line", 0, "line number for --fix")
	lintCmd.Flags().BoolVar(&applySave, "save", false, "save the fixed compose.yaml/.env back to the stack")
	root.AddCommand(lintCmd)

	// clean (strips # comments & blank lines outside | / > block scalars, matching UI Clean view)
	var cleanStack string
	cleanCmd := &cobra.Command{
		Use:   "clean [file]",
		Short: "Strip comments and blank lines outside YAML block scalars (offline or --stack)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var raw string
			if cleanStack != "" {
				ctx := cmd.Context()
				c, hostID, err := opts.connect(ctx)
				if err != nil {
					return err
				}
				var st client.Stack
				if err := c.Get(ctx, "/api/hosts/"+url.PathEscape(hostID)+"/stacks/"+url.PathEscape(cleanStack), &st); err != nil {
					return err
				}
				raw = st.Compose
			} else if len(args) == 1 {
				b, err := os.ReadFile(args[0])
				if err != nil {
					return err
				}
				raw = string(b)
			} else {
				return errors.New("pass a file path or --stack <name>")
			}
			cleaned := cleanYAMLText(raw)
			fmt.Fprint(cmd.OutOrStdout(), cleaned)
			if cleaned != "" && !strings.HasSuffix(cleaned, "\n") {
				fmt.Fprintln(cmd.OutOrStdout())
			}
			return nil
		},
	}
	cleanCmd.Flags().StringVar(&cleanStack, "stack", "", "read compose.yaml from a managed stack instead of a local file")
	root.AddCommand(cleanCmd)

	return root
}

func firstNonEmptyEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// cleanYAMLText mirrors web/src/lib/yamlcomments.ts: removes # comments and blank lines outside |/> block scalars.
func cleanYAMLText(text string) string {
	src := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var out []string
	blockIndent := -1
	for i := 0; i < len(src); i++ {
		line := src[i]
		trimmed := strings.TrimSpace(line)
		if blockIndent >= 0 {
			if trimmed == "" {
				keep := false
				for j := i + 1; j < len(src); j++ {
					if strings.TrimSpace(src[j]) != "" {
						keep = indentCount(src[j]) > blockIndent
						break
					}
				}
				if keep {
					out = append(out, line)
					continue
				}
				blockIndent = -1
			} else if indentCount(line) > blockIndent {
				out = append(out, line)
				continue
			} else {
				blockIndent = -1
			}
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		hash := inlineCommentStart(line)
		body := line
		if hash >= 0 {
			body = strings.TrimRight(line[:hash], " \t")
		}
		if strings.TrimSpace(body) == "" {
			continue
		}
		out = append(out, body)
		if isBlockScalarStart(body) {
			blockIndent = indentCount(line)
		}
	}
	return strings.Join(out, "\n")
}

func indentCount(line string) int {
	n := 0
	for i := 0; i < len(line); i++ {
		if line[i] == ' ' {
			n++
		} else {
			break
		}
	}
	return n
}

func inlineCommentStart(line string) int {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quote != 0 {
			if quote == '"' && c == '\\' {
				i++
				continue
			}
			if c == quote {
				if quote == '\'' && i+1 < len(line) && line[i+1] == '\'' {
					i++
					continue
				}
				quote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			prev := strings.TrimSpace(line[:i])
			if prev == "" || strings.HasSuffix(prev, ":") || strings.HasSuffix(prev, "-") || strings.HasSuffix(prev, ",") || strings.HasSuffix(prev, "[") {
				quote = c
			}
			continue
		}
		if c == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
			return i
		}
	}
	return -1
}

func isBlockScalarStart(body string) bool {
	s := strings.TrimSpace(body)
	idx := strings.IndexAny(s, "|-")
	_ = idx
	if colon := strings.LastIndex(s, ":"); colon >= 0 {
		rest := strings.TrimSpace(s[colon+1:])
		if len(rest) > 0 && (rest[0] == '|' || rest[0] == '>') {
			return true
		}
	}
	return false
}
