package status

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"time"

	"moorage/internal/store"
)

// Static snapshots (M18D): a published status page rendered as one self-contained HTML
// file plus a JSON document, which the status-snapshot add-on uploads to S3, WebDAV or
// GitHub Pages so the page survives a full outage of the server. Both are built from
// PublicPage only, exactly like the live page, so a snapshot shows nothing the live page
// doesn't. The HTML carries the build time and shows "Data may be out of date" by itself
// once it is older than the configured threshold.

// SnapshotFormat is the version of the published status.json.
const SnapshotFormat = 1

// ErrNotPublished is returned for a page that isn't public: it is never published.
var ErrNotPublished = errors.New("status page is not published")

// SnapshotDoc is the published status.json.
type SnapshotDoc struct {
	Format            int        `json:"format"`
	GeneratedAt       time.Time  `json:"generatedAt"`
	StaleAfterSeconds int        `json:"staleAfterSeconds"`
	Page              PublicPage `json:"page"`
}

// pageView is the data of the page template: the live page or a static snapshot.
type pageView struct {
	Page     PublicPage
	FeedHref string
	Snapshot *snapshotView // nil on the live page
}

type snapshotView struct {
	CSS               template.CSS
	JS                template.JS
	Favicon           template.URL
	StaleAfterSeconds int
}

// staleJS shows the "Data may be out of date" notice once the snapshot is older than
// data-stale-after seconds, checked against the visitor's clock and again every 30 s.
const staleJS = `
(function () {
  try {
    var b = document.body;
    var at = Date.parse(b.getAttribute('data-updated'));
    var max = Number(b.getAttribute('data-stale-after')) * 1000;
    var el = document.getElementById('stale');
    if (!el || isNaN(at) || !(max > 0)) return;
    var check = function () { el.hidden = Date.now() - at <= max; };
    check();
    setInterval(check, 30000);
  } catch (_) {}
})();
`

// SnapshotStale mirrors the snapshot's own check: is a copy built at updated stale at now?
func SnapshotStale(updated, now time.Time, staleAfter time.Duration) bool {
	if updated.IsZero() || staleAfter <= 0 {
		return false
	}
	return now.Sub(updated) > staleAfter
}

// Snapshot builds p fresh (not from the cache, so a status change shows at once) and
// renders it. Unpublished pages are refused.
func (s *Service) Snapshot(ctx context.Context, p store.StatusPage, staleAfter time.Duration) (html, doc []byte, err error) {
	if !p.Published {
		return nil, nil, ErrNotPublished
	}
	data, err := s.build(ctx, p, s.now())
	if err != nil {
		return nil, nil, err
	}
	return RenderSnapshot(data, staleAfter)
}

// RenderSnapshot renders a built page as static HTML (inline CSS and JS, no requests to
// the hub, no RSS link) and as status.json.
func RenderSnapshot(data PublicPage, staleAfter time.Duration) (html, doc []byte, err error) {
	secs := int(staleAfter / time.Second)
	view := pageView{Page: data, Snapshot: &snapshotView{
		CSS:               template.CSS(statusBaseCSS),
		JS:                template.JS(statusJS + staleJS),
		Favicon:           template.URL("data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(faviconSVG))),
		StaleAfterSeconds: secs,
	}}
	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, view); err != nil {
		return nil, nil, err
	}
	doc, err = json.MarshalIndent(SnapshotDoc{
		Format:            SnapshotFormat,
		GeneratedAt:       data.UpdatedAt.UTC(),
		StaleAfterSeconds: secs,
		Page:              data,
	}, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), append(doc, '\n'), nil
}
