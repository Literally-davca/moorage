package demo

import (
	"context"
	"errors"
	"testing"

	"moorage/internal/engine"
	"moorage/internal/store"
)

func TestHostIsReadOnlySampleData(t *testing.T) {
	ctx := context.Background()
	h := NewHost()
	list, err := h.ListStacks(ctx)
	if err != nil || len(list) != len(stacks) {
		t.Fatalf("stacks: %v %v", list, err)
	}
	status := map[string]string{}
	for _, s := range list {
		status[s.Name] = s.Status
	}
	if status["nextcloud"] != engine.StatusUp || status["immich"] != engine.StatusDegraded || status["homeassistant"] != engine.StatusStopped {
		t.Errorf("statuses: %v", status)
	}
	st, err := h.GetStack(ctx, "nextcloud")
	if err != nil || st.Compose == "" || len(st.Containers) != 3 {
		t.Fatalf("stack: %+v %v", st, err)
	}
	if _, err := h.GetStack(ctx, "missing"); !errors.Is(err, engine.ErrNotFound) {
		t.Errorf("missing stack: %v", err)
	}
	if err := h.Save(ctx, "x", "services: {}", "", "u", true); !errors.Is(err, engine.ErrReadOnly) {
		t.Errorf("save: %v", err)
	}
	if _, err := h.RunAction(ctx, "nextcloud", "up", "u"); !IsDemo(err) {
		t.Errorf("action: %v", err)
	}
	if lines, err := h.TailLogs(ctx, "immich", 10); err != nil || len(lines) == 0 || len(lines) > 10 {
		t.Errorf("logs: %d %v", len(lines), err)
	}
	hist, err := h.History(ctx, "jellyfin")
	if err != nil || len(hist) != 2 {
		t.Fatalf("history: %v %v", hist, err)
	}
	if _, err := h.Version(ctx, "jellyfin", hist[0].Short); err != nil {
		t.Errorf("version: %v", err)
	}
}

func TestSeedOnlyOnce(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if ok, err := Seed(ctx, db, "local"); !ok || err != nil {
		t.Fatalf("first seed: %v %v", ok, err)
	}
	if ok, err := Seed(ctx, db, "local"); ok || err != nil {
		t.Fatalf("second seed: %v %v", ok, err)
	}
	checks, _ := db.Checks(ctx)
	pages, _ := db.StatusPages(ctx)
	if len(checks) != len(stacks)+1 || len(pages) != 1 || !pages[0].Published {
		t.Errorf("seeded %d checks, pages %+v", len(checks), pages)
	}
	for _, c := range checks {
		if c.Kind != "stack" && c.Enabled {
			t.Errorf("check %q probes the network", c.Name)
		}
	}
}
