package demo

import (
	"context"
	"encoding/json"

	"moorage/internal/store"
)

// Seed fills an empty database with sample checks, a disabled channel and a published
// status page. It does nothing when the database already holds checks, channels or
// status pages, so a restart (or a real database by mistake) is never changed.
// Only stack checks run: they read the demo host, so the demo sends no probes anywhere.
func Seed(ctx context.Context, st *store.Store, hostID string) (bool, error) {
	checks, err := st.Checks(ctx)
	if err != nil {
		return false, err
	}
	chs, err := st.Channels(ctx)
	if err != nil {
		return false, err
	}
	pages, err := st.StatusPages(ctx)
	if err != nil {
		return false, err
	}
	if len(checks)+len(chs)+len(pages) > 0 {
		return false, nil
	}

	cfg, _ := json.Marshal(map[string]any{"url": "https://ntfy.invalid/moorage-demo"})
	chID, err := st.SaveChannel(ctx, store.Channel{Name: "Phone (ntfy, demo)", Kind: "ntfy", Config: string(cfg), Enabled: false})
	if err != nil {
		return false, err
	}

	var items []store.StatusPageItem
	for i, s := range stacks {
		id, err := st.SaveCheck(ctx, store.Check{
			Name: displayName(s.name), Kind: "stack", Target: s.name, HostID: hostID, Stack: s.name,
			Interval: 60, Timeout: 10, Retries: 2, Settings: "{}", Enabled: true, Channels: []int64{chID},
		})
		if err != nil {
			return false, err
		}
		section := "Apps"
		if s.name == "homeassistant" {
			section = "Home"
		}
		items = append(items, store.StatusPageItem{Position: i, Section: section, DisplayName: displayName(s.name), CheckID: id})
	}
	// An HTTP check that shows the form's options but never probes (disabled).
	httpSet, _ := json.Marshal(map[string]any{"status": "200-299", "keyword": "Welcome"})
	if _, err := st.SaveCheck(ctx, store.Check{
		Name: "Public website", Kind: "http", Target: "https://example.com", Interval: 300, Timeout: 10, Retries: 2,
		Settings: string(httpSet), Enabled: false,
	}); err != nil {
		return false, err
	}
	if _, err := st.SaveStatusPage(ctx, store.StatusPage{
		Slug: "demo", Title: "Demo Homelab", Description: "Sample status page of the Moorage demo.",
		Published: true, ShowIncidents: true, Items: items,
	}); err != nil {
		return false, err
	}
	return true, nil
}

func displayName(stack string) string {
	switch stack {
	case "nextcloud":
		return "Nextcloud"
	case "jellyfin":
		return "Jellyfin"
	case "vaultwarden":
		return "Vaultwarden"
	case "immich":
		return "Immich"
	case "homeassistant":
		return "Home Assistant"
	}
	return stack
}
