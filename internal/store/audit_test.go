package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAuditPruneAfterOneYearAndDefaults(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now()
	old := AuditEntry{At: now.Add(-366 * 24 * time.Hour), Actor: "admin", Action: "stack.up", HostID: "local", Stack: "web"}
	recent := AuditEntry{At: now.Add(-364 * 24 * time.Hour), Actor: "admin", Action: "stack.down", HostID: "local", Stack: "web"}
	for _, e := range []AuditEntry{old, recent} {
		if err := st.Audit(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	// Zero time means now, an unknown outcome means ok, long text is clipped.
	if err := st.Audit(ctx, AuditEntry{Actor: strings.Repeat("x", 1000), Action: "auth.login_failed", Outcome: "weird"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PruneAudit(ctx, now.Add(-365*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	rows, err := st.AuditEntries(ctx, AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want the row older than a year pruned, got %+v", rows)
	}
	if rows[1].Action != "stack.down" {
		t.Fatalf("wrong row kept: %+v", rows[1])
	}
	fresh := rows[0]
	if time.Since(fresh.At) > time.Minute || fresh.Outcome != AuditOK || len(fresh.Actor) != maxAuditField {
		t.Fatalf("defaults not applied: %+v", fresh)
	}
}
