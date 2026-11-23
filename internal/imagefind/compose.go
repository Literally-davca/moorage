package imagefind

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"go.yaml.in/yaml/v4"

	"moorage/internal/templates"
)

// repoCompose reads the compose file of the image's source repository, when the source
// label points at a known public code host. Other hosts are not contacted: the label
// is data from the image and must not steer requests anywhere.
func (f *Finder) repoCompose(ctx context.Context, source, image string) (*Candidate, string) {
	u, err := url.Parse(strings.TrimSpace(source))
	if err != nil || u.Host == "" {
		return nil, ""
	}
	hosts := f.GitHosts
	if hosts == nil {
		hosts = defaultGitHosts
	}
	host := strings.ToLower(strings.TrimPrefix(u.Host, "www."))
	gh, ok := hosts[host]
	if !ok {
		return nil, "" // unknown host: skip quietly, the generated compose still helps
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return nil, ""
	}
	owner, repo := parts[0], strings.TrimSuffix(parts[1], ".git")
	if !safeSegment(owner) || !safeSegment(repo) {
		return nil, ""
	}
	resp, b, err := f.get(ctx, gh.DefaultBranch(owner, repo), http.Header{"Accept": {"application/json"}})
	branch := ""
	if err == nil && resp.StatusCode == http.StatusOK {
		branch = jsonField(b, "default_branch")
	}
	if branch == "" {
		return nil, fmt.Sprintf("Source repository %s/%s: couldn't read its default branch.", owner, repo)
	}
	for _, p := range composePaths {
		raw := gh.Raw(owner, repo, branch, p)
		resp, b, err := f.get(ctx, raw, nil)
		if err != nil || resp.StatusCode != http.StatusOK || len(strings.TrimSpace(string(b))) == 0 {
			continue
		}
		text, notes := adaptRepoCompose(string(b), image)
		c := &Candidate{
			Source: "repo", Title: "Compose file from " + owner + "/" + repo,
			Detail:   p + " on " + host + " (" + branch + ")",
			URL:      "https://" + host + "/" + owner + "/" + repo,
			Compose:  text,
			Warnings: append(notes, templates.Warnings(text)...),
		}
		return c, ""
	}
	return nil, ""
}

func safeSegment(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 100 {
		return false
	}
	for _, r := range s {
		if !(r == '-' || r == '_' || r == '.' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// adaptRepoCompose makes a repository's compose file usable without the repository:
// a single service that builds from source gets the looked-up image instead, and
// what can't work (other builds, missing env_file and relative files) is noted.
func adaptRepoCompose(text, image string) (string, []string) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil || len(doc.Content) == 0 {
		return text, []string{"The file isn't valid YAML; check it before creating the stack."}
	}
	var notes []string
	services := mapGet(doc.Content[0], "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return text, []string{"The file has no services."}
	}
	var builders []*yaml.Node
	for i := 0; i+1 < len(services.Content); i += 2 {
		name, svc := services.Content[i].Value, services.Content[i+1]
		if svc.Kind != yaml.MappingNode {
			continue
		}
		if mapGet(svc, "build") != nil {
			if mapGet(svc, "image") == nil {
				builders = append(builders, svc)
			} else {
				notes = append(notes, "Service "+name+" also builds from source; Moorage only pulls its image.")
			}
		}
		if mapGet(svc, "env_file") != nil {
			notes = append(notes, "Service "+name+" reads an env_file from the repository; add those variables to .env or remove env_file.")
		}
		if vols := mapGet(svc, "volumes"); vols != nil {
			for _, v := range vols.Content {
				if strings.HasPrefix(v.Value, "./") || strings.HasPrefix(v.Value, "../") {
					notes = append(notes, "Service "+name+" mounts "+v.Value+" from the repository; that file or folder won't exist.")
				}
			}
		}
	}
	changed := false
	if len(builders) == 1 {
		svc := builders[0]
		mapDel(svc, "build")
		svc.Content = append([]*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "image"},
			{Kind: yaml.ScalarNode, Value: image},
		}, svc.Content...)
		notes = append(notes, "The service that built from source now uses "+image+".")
		changed = true
	} else if len(builders) > 1 {
		notes = append(notes, "Several services build from source; give each an image before deploying.")
	}
	if !changed {
		return text, notes
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return text, notes
	}
	return string(out), notes
}

func mapGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func mapDel(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// runtimeEnv are image variables that describe the image itself, not settings.
var runtimeEnv = map[string]bool{"PATH": true, "HOME": true, "LANG": true, "LANGUAGE": true, "HOSTNAME": true, "TERM": true, "SHLVL": true, "PWD": true}

func settingVar(k string) bool {
	if runtimeEnv[k] || strings.HasPrefix(k, "LC_") || strings.Contains(k, "VERSION") || strings.HasSuffix(k, "_HOME") ||
		strings.HasSuffix(k, "_SHA256") || strings.HasPrefix(k, "GPG_") || strings.HasPrefix(k, "GOSU") {
		return false
	}
	return true
}

// generate writes a minimal compose file from the image config: exposed TCP ports
// published on the same port, a named volume per declared volume, the image's own
// settings variables in .env (commented), and its user.
func generate(name, image string, cfg ImageConfig) Candidate {
	var b strings.Builder
	fmt.Fprintf(&b, "services:\n  %s:\n    image: %s\n    restart: unless-stopped\n", name, image)
	var ports []string
	for p := range cfg.Config.ExposedPorts {
		num, proto, _ := strings.Cut(p, "/")
		if proto == "" || proto == "tcp" {
			ports = append(ports, num)
		} else {
			ports = append(ports, num+"/"+proto)
		}
	}
	sort.Strings(ports)
	if len(ports) > 0 {
		b.WriteString("    ports:\n")
		for _, p := range ports {
			num, proto, _ := strings.Cut(p, "/")
			if proto != "" {
				fmt.Fprintf(&b, "      - \"%s:%s/%s\"\n", num, num, proto)
			} else {
				fmt.Fprintf(&b, "      - \"%s:%s\"\n", num, num)
			}
		}
	}
	var vols []string
	for v := range cfg.Config.Volumes {
		vols = append(vols, v)
	}
	sort.Strings(vols)
	var volNames []string
	if len(vols) > 0 {
		b.WriteString("    volumes:\n")
		for i, v := range vols {
			vn := fmt.Sprintf("%s-data", name)
			if len(vols) > 1 {
				vn = fmt.Sprintf("%s-data-%d", name, i+1)
			}
			volNames = append(volNames, vn)
			fmt.Fprintf(&b, "      - %s:%s\n", vn, v)
		}
	}
	var env []string
	for _, kv := range cfg.Config.Env {
		k, _, _ := strings.Cut(kv, "=")
		if settingVar(k) {
			env = append(env, kv)
		}
	}
	sort.Strings(env)
	if len(env) > 0 {
		b.WriteString("    env_file:\n      - path: .env\n        required: false\n")
	}
	if len(volNames) > 0 {
		b.WriteString("volumes:\n")
		for _, vn := range volNames {
			fmt.Fprintf(&b, "  %s:\n", vn)
		}
	}
	envText := ""
	if len(env) > 0 {
		envText = "# Settings the image defines, with its defaults. Uncomment to change one.\n"
		for _, kv := range env {
			envText += "# " + kv + "\n"
		}
	}
	detail := "Ports, volumes and settings the image declares"
	if cfg.Config.User != "" {
		detail += "; runs as user " + cfg.Config.User
	}
	text := b.String()
	return Candidate{
		Source: "generated", Title: "Generated from the image", Detail: detail,
		Compose: text, Env: envText, Warnings: templates.Warnings(text),
	}
}
