package store

import (
	"context"
	"testing"
	"time"
)

// Raw results older than the retention become hourly summaries, and uptime over a long
// period still counts them (M19M).
func TestPruneKeepsHourlySummaries(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	id, err := s.SaveCheck(ctx, Check{Name: "c", Kind: "http", Target: "https://x", Interval: 60, Timeout: 5, Retries: 1, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Hour)
	for i, st := range []string{"up", "up", "down", "up"} {
		if _, err := s.db.Exec(`INSERT INTO check_results (check_id, ts, status, latency_ms) VALUES (?, ?, ?, ?)`, id, old.Add(time.Duration(i)*time.Minute).UnixMilli(), st, 100*(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO check_results (check_id, ts, status, latency_ms) VALUES (?, ?, 'up', 10)`, id, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneResults(ctx, time.Now().Add(-30*24*time.Hour))
	if err != nil || n != 4 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	up, total, err := s.Uptime(ctx, id, time.Now().Add(-90*24*time.Hour))
	if err != nil || total != 5 || up != 0.8 {
		t.Fatalf("uptime %v of %d, %v", up, total, err)
	}
	var maxLat int
	_ = s.db.QueryRow(`SELECT max_latency_ms FROM check_results_hourly WHERE check_id = ?`, id).Scan(&maxLat)
	if maxLat != 400 {
		t.Fatalf("max latency %d", maxLat)
	}
}
