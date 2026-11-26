package assistant

import (
	_ "embed"
	"strings"
)

//go:embed system_prompt.md
var systemPrompt string

// Route is one page of the web app the assistant may link to.
type Route struct {
	Path  string // after "#/", may contain {host} / {stack}
	Title string
}

// Routes is the route map of web/src/lib/state.svelte.ts (a test checks it).
var Routes = []Route{
	{"h/{host}", "Stacks of a host (home)"},
	{"h/{host}/stacks/{stack}", "Stack detail: containers, Problems, compose editor, logs, dependencies, history, snapshots"},
	{"h/{host}/new", "Create a new stack"},
	{"monitoring/checks", "Monitoring: checks"},
	{"checks/new", "Add a check"},
	{"monitoring/status-pages", "Status pages"},
	{"monitoring/maintenance", "Maintenance windows"},
	{"updates", "Image updates"},
	{"settings/hosts", "Settings: hosts and agents (ports map, disk cleanup)"},
	{"settings/general", "Settings: general (registries, defaults)"},
	{"settings/notifications", "Settings: notification channels (Discord, Telegram, e-mail, ntfy…)"},
	{"settings/addons", "Settings: add-ons (including this assistant)"},
	{"settings/backups", "Settings: backups"},
	{"settings/users", "Settings: users"},
	{"settings/audit", "Settings: audit log"},
	{"settings/templates", "Settings: template sources"},
	{"settings/import", "Settings: import from Portainer / Uptime Kuma / Gatus"},
	{"account", "Account: password, two-factor, passkeys"},
}

// SystemPrompt is the static, cacheable part.
func SystemPrompt() string {
	var b strings.Builder
	for _, r := range Routes {
		b.WriteString("- `#/" + r.Path + "` – " + r.Title + "\n")
	}
	return strings.Replace(systemPrompt, "{routes}", b.String(), 1)
}
