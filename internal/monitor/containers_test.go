package monitor

import (
	"context"
	"testing"
	"time"

	"moorage/internal/engine"
	"moorage/internal/store"
)

func TestDiffContainers(t *testing.T) {
	at := time.Now()
	code := func(n int) *int { return &n }
	first := []engine.Container{
		{ID: "a1", Service: "app", State: "running", Health: "healthy"},
		{ID: "d1", Service: "db", State: "running"},
	}
	snap, ev := diffContainers(nil, first, "local", "web", at)
	if len(ev) != 0 {
		t.Fatalf("the first look is only a baseline: %+v", ev)
	}

	second := []engine.Container{
		{ID: "a1", Service: "app", State: "running", Health: "unhealthy"},
		{ID: "d1", Service: "db", State: "exited", Status: "Exited (137) 5 seconds ago", ExitCode: code(137)},
	}
	snap, ev = diffContainers(snap, second, "local", "web", at)
	if len(ev) != 2 {
		t.Fatalf("want unhealthy + die, got %+v", ev)
	}
	byType := map[string]store.ContainerEvent{}
	for _, e := range ev {
		byType[e.Type] = e
	}
	if e := byType[store.ContainerUnhealthy]; e.Service != "app" || e.Detail != "unhealthy" {
		t.Fatalf("unhealthy: %+v", e)
	}
	if e := byType[store.ContainerDie]; e.Service != "db" || e.ExitCode == nil || *e.ExitCode != 137 {
		t.Fatalf("die: %+v", e)
	}

	// nothing changed: no new events
	snap, ev = diffContainers(snap, second, "local", "web", at)
	if len(ev) != 0 {
		t.Fatalf("no change, no events: %+v", ev)
	}

	third := []engine.Container{
		{ID: "a1", Service: "app", State: "running", Health: "healthy"},
		{ID: "d1", Service: "db", State: "restarting"},
	}
	_, ev = diffContainers(snap, third, "local", "web", at)
	if len(ev) != 2 {
		t.Fatalf("want healthy again + restart, got %+v", ev)
	}
}

func TestContainerEventsMergeRepeats(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	code := 1
	now := time.Now()
	for i := 0; i < 3; i++ {
		if err := st.AddContainerEvent(ctx, store.ContainerEvent{HostID: "h", Stack: "s", Service: "app", ContainerID: "c", Type: store.ContainerDie, ExitCode: &code, Time: now.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	// a different exit code is a new row
	other := 2
	_ = st.AddContainerEvent(ctx, store.ContainerEvent{HostID: "h", Stack: "s", Service: "app", ContainerID: "c", Type: store.ContainerDie, ExitCode: &other, Time: now.Add(4 * time.Minute)})
	evs, err := st.ContainerEvents(ctx, "h", "s", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || len(evs) != 2 {
		t.Fatalf("events: %+v %v", evs, err)
	}
	if evs[1].Count != 3 || !evs[1].Time.Equal(now.Add(2*time.Minute).Truncate(time.Millisecond)) {
		t.Fatalf("crash loop merged into one row with the latest time: %+v", evs[1])
	}
	if n, _ := st.PruneContainerEvents(ctx, now.Add(time.Hour)); n != 2 {
		t.Fatalf("prune: %d", n)
	}
}
