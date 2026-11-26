package status

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"moorage/internal/store"
)

func samplePage(now time.Time) PublicPage {
	up := 99.5
	return PublicPage{
		Slug: "main", Title: `Acme <script>alert(1)</script>`, BannerStatus: StateOperational, BannerText: "All systems operational",
		UpdatedAt: now, UpdatedText: now.Format("2 Jan 2006, 15:04 UTC"), ShowIncidents: true,
		Sections: []PublicSection{{Name: "Core", Items: []PublicItem{{
			DisplayName: "Website", Status: StateOperational, StatusLabel: "Operational",
			Uptime24h: &up, Uptime30d: &up, Uptime24hStr: "99.50%", Uptime30dStr: "99.50%",
			Days: []PublicDay{{Date: "2026-11-26", Status: StateOperational, Uptime: &up, Tooltip: "26 Nov · 99.50%"}},
		}}}},
		FeedItems: []FeedEvent{{GUID: "internal-guid", Title: "x"}},
	}
}

func TestRenderSnapshotSelfContainedWithStaleNotice(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	html, doc, err := RenderSnapshot(samplePage(now), 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	h := string(html)
	for _, want := range []string{
		`data-updated="2026-11-26T12:00:00Z"`,
		`data-stale-after="900"`,
		`id="stale"`,
		`hidden>`,
		"Data may be out of date.",
		"<style>",
		".stale[hidden]",
		`href="data:image/svg&#43;xml;base64,`, // + is entity-escaped in the attribute
		"getElementById('stale')",
		"Website",
		"&lt;script&gt;alert(1)&lt;/script&gt;",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("snapshot HTML lacks %q", want)
		}
	}
	// Self-contained: nothing is loaded from the hub, and there is no feed to link to.
	for _, bad := range []string{"/status-assets/", "feed.xml", "RSS feed", "<script>alert(1)", "@font-face", "ZgotmplZ"} {
		if strings.Contains(h, bad) {
			t.Errorf("snapshot HTML contains %q", bad)
		}
	}

	var d SnapshotDoc
	if err := json.Unmarshal(doc, &d); err != nil {
		t.Fatal(err)
	}
	if d.Format != SnapshotFormat || d.StaleAfterSeconds != 900 || !d.GeneratedAt.Equal(now) || d.Page.Slug != "main" || len(d.Page.Sections) != 1 {
		t.Fatalf("doc: %+v", d)
	}
	if strings.Contains(string(doc), "internal-guid") {
		t.Fatal("feed internals in status.json")
	}
}

func TestLivePageUnchangedBySnapshotTemplate(t *testing.T) {
	var buf strings.Builder
	if err := pageTmpl.Execute(&buf, pageView{Page: samplePage(time.Now()), FeedHref: "/status/main/feed.xml"}); err != nil {
		t.Fatal(err)
	}
	h := buf.String()
	if !strings.Contains(h, `<script src="/status-assets/status.js" defer></script>`) || !strings.Contains(h, `href="/status/main/feed.xml"`) ||
		strings.Contains(h, `id="stale"`) || strings.Contains(h, "<style>") || strings.Contains(h, "data-stale-after") {
		t.Fatalf("live page changed:\n%s", h)
	}
}

func TestSnapshotStale(t *testing.T) {
	at := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		now   time.Time
		after time.Duration
		want  bool
	}{
		{at.Add(14 * time.Minute), 15 * time.Minute, false},
		{at.Add(15 * time.Minute), 15 * time.Minute, false},
		{at.Add(15*time.Minute + time.Second), 15 * time.Minute, true},
		{at.Add(-time.Hour), 15 * time.Minute, false}, // visitor clock behind
		{at.Add(time.Hour), 0, false},                 // no threshold
	}
	for i, c := range cases {
		if got := SnapshotStale(at, c.now, c.after); got != c.want {
			t.Errorf("%d: stale=%v, want %v", i, got, c.want)
		}
	}
	if SnapshotStale(time.Time{}, at, time.Minute) {
		t.Error("never published counts as stale")
	}
}

func TestSnapshotRefusesUnpublishedPage(t *testing.T) {
	s := New(nil, nil, "")
	if _, _, err := s.Snapshot(context.Background(), store.StatusPage{Slug: "draft"}, time.Minute); !errors.Is(err, ErrNotPublished) {
		t.Fatalf("unpublished page rendered: %v", err)
	}
}
