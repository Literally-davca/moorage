package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sort"
	"time"

	"moorage/internal/snapshot"
	"moorage/internal/status"
	"moorage/internal/store"
)

// Static status page snapshot add-on (M18D): publishes every published status page (or
// the chosen ones) as static HTML + JSON to S3 / R2, WebDAV or GitHub Pages, after each
// status change (debounced) and periodically, so the public page survives a full outage
// of the server. The publisher never runs in demo mode.

const addonStatusSnapshot = "status-snapshot"

// newSnapshotPublisher wires the publisher to the settings, the add-on switch and the
// status page renderer. It only runs after StartStatusSnapshot.
func (s *Server) newSnapshotPublisher() *snapshot.Publisher {
	p := &snapshot.Publisher{
		Load:   s.loadStatusSnapshot,
		Render: s.renderStatusSnapshot,
		Save: func(ctx context.Context, l snapshot.Last) {
			if s.store == nil {
				return
			}
			if b, err := json.Marshal(l); err == nil {
				_ = s.store.SetSetting(ctx, store.SettingStatusSnapshotLast, string(b))
			}
		},
		Log: s.log,
	}
	if s.store != nil {
		if raw, _ := s.store.GetSetting(context.Background(), store.SettingStatusSnapshotLast); raw != "" {
			var l snapshot.Last
			if json.Unmarshal([]byte(raw), &l) == nil {
				p.SetLast(l)
			}
		}
	}
	if s.status != nil {
		s.status.OnInvalidate = func() { p.Kick("edit") }
	}
	return p
}

// StartStatusSnapshot runs the publisher until ctx ends (never in demo mode).
func (s *Server) StartStatusSnapshot(ctx context.Context) {
	if s.cfg.Demo || s.snapshot == nil || !s.snapshotRunning.CompareAndSwap(false, true) {
		return
	}
	go s.snapshot.Run(ctx)
}

// KickStatusSnapshot asks for a publish soon (a check changed state).
func (s *Server) KickStatusSnapshot() {
	if s.snapshot != nil {
		s.snapshot.Kick("change")
	}
}

func (s *Server) savedStatusSnapshot(ctx context.Context) (snapshot.Config, error) {
	var c snapshot.Config
	raw, err := s.store.GetSetting(ctx, store.SettingStatusSnapshotJSON)
	if err != nil {
		return c, err
	}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			return c, err
		}
	}
	if c.Interval == 0 {
		c.Interval = snapshot.DefaultInterval
	}
	if c.Stale == 0 {
		c.Stale = snapshot.DefaultStale
	}
	return c, nil
}

func (s *Server) loadStatusSnapshot(ctx context.Context) (snapshot.Config, bool, error) {
	if s.store == nil {
		return snapshot.Config{}, false, errors.New("no store")
	}
	c, err := s.savedStatusSnapshot(ctx)
	if err != nil {
		return c, false, err
	}
	on, err := s.store.AddonEnabled(ctx, addonStatusSnapshot)
	return c, on, err
}

// renderStatusSnapshot renders the published pages the config selects: <slug>/index.html
// and <slug>/status.json each. Unpublished pages are never rendered.
func (s *Server) renderStatusSnapshot(ctx context.Context, c snapshot.Config) ([]snapshot.File, []string, error) {
	pages, err := s.store.StatusPages(ctx)
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i].Slug < pages[j].Slug })
	var files []snapshot.File
	var slugs []string
	for _, p := range pages {
		if !p.Published || (len(c.Pages) > 0 && !slices.Contains(c.Pages, p.ID)) {
			continue
		}
		html, doc, err := s.status.Snapshot(ctx, p, c.StaleDuration())
		if err != nil {
			return nil, slugs, err
		}
		slugs = append(slugs, p.Slug)
		files = append(files,
			snapshot.File{Path: p.Slug + "/status.json", ContentType: "application/json; charset=utf-8", Data: doc},
			snapshot.File{Path: p.Slug + "/index.html", ContentType: "text/html; charset=utf-8", Data: html},
		)
	}
	return files, slugs, nil
}

type statusSnapshotDTO struct {
	Enabled           bool           `json:"enabled"`
	Running           bool           `json:"running"`
	Provider          string         `json:"provider"`
	Pages             []int64        `json:"pages"`
	IntervalMinutes   int            `json:"intervalMinutes"`
	StaleMinutes      int            `json:"staleMinutes"`
	S3Endpoint        string         `json:"s3Endpoint"`
	S3Bucket          string         `json:"s3Bucket"`
	S3Region          string         `json:"s3Region"`
	S3AccessKey       string         `json:"s3AccessKey"`
	S3SecretKeySet    bool           `json:"s3SecretKeySet"`
	S3Prefix          string         `json:"s3Prefix"`
	WebDAVURL         string         `json:"webdavUrl"`
	WebDAVUsername    string         `json:"webdavUsername"`
	WebDAVPasswordSet bool           `json:"webdavPasswordSet"`
	GitHubRepo        string         `json:"githubRepo"`
	GitHubBranch      string         `json:"githubBranch"`
	GitHubPath        string         `json:"githubPath"`
	GitHubTokenSet    bool           `json:"githubTokenSet"`
	Target            string         `json:"target,omitempty"`
	Last              *snapshot.Last `json:"last,omitempty"`
	// Stale: the newest published copy is older than the out-of-date threshold, so
	// visitors of the static page see the notice now.
	Stale bool `json:"stale"`
}

func (s *Server) getStatusSnapshot(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, on, err := s.loadStatusSnapshot(ctx)
	if err != nil {
		s.engErr(w, err)
		return
	}
	dto := statusSnapshotDTO{
		Enabled: on, Running: s.snapshotRunning.Load() && on && c.Configured(),
		Provider: c.Provider, Pages: c.Pages, IntervalMinutes: c.Interval, StaleMinutes: c.Stale,
		S3Endpoint: c.S3Endpoint, S3Bucket: c.S3Bucket, S3Region: c.S3Region, S3AccessKey: c.S3AccessKey,
		S3SecretKeySet: c.S3SecretKey != "", S3Prefix: c.S3Prefix,
		WebDAVURL: c.WebDAVURL, WebDAVUsername: c.WebDAVUsername, WebDAVPasswordSet: c.WebDAVPassword != "",
		GitHubRepo: c.GitHubRepo, GitHubBranch: c.GitHubBranch, GitHubPath: c.GitHubPath, GitHubTokenSet: c.GitHubToken != "",
		Target: c.Describe(),
	}
	if dto.Pages == nil {
		dto.Pages = []int64{}
	}
	if l := s.snapshot.Last(); !l.At.IsZero() {
		dto.Last = &l
		dto.Stale = c.Configured() && status.SnapshotStale(l.LastSuccess, time.Now(), c.StaleDuration())
	}
	writeJSON(w, http.StatusOK, dto)
}

// statusSnapshotInput reads a config from the form: empty secrets keep the saved ones
// for the same endpoint, folder or repository.
func (s *Server) statusSnapshotInput(w http.ResponseWriter, r *http.Request) (snapshot.Config, bool) {
	var in snapshot.Config
	if !readJSON(w, r, &in) {
		return in, false
	}
	saved, err := s.savedStatusSnapshot(r.Context())
	if err != nil {
		s.engErr(w, err)
		return in, false
	}
	in.Merge(saved)
	if err := in.Normalize(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return in, false
	}
	return in, true
}

func (s *Server) putStatusSnapshot(w http.ResponseWriter, r *http.Request) {
	c, ok := s.statusSnapshotInput(w, r)
	if !ok {
		return
	}
	b, err := json.Marshal(c)
	if err != nil {
		s.engErr(w, err)
		return
	}
	if err := s.store.SetSetting(r.Context(), store.SettingStatusSnapshotJSON, string(b)); err != nil {
		s.engErr(w, err)
		return
	}
	setAuditTarget(r, c.Describe())
	s.snapshot.Kick("settings")
	s.getStatusSnapshot(w, r)
}

// testStatusSnapshot checks write access to the target in the form (a test file is
// written and removed; on GitHub a commit is created but no branch moves).
func (s *Server) testStatusSnapshot(w http.ResponseWriter, r *http.Request) {
	c, ok := s.statusSnapshotInput(w, r)
	if !ok {
		return
	}
	setAuditTarget(r, c.Describe())
	t, err := snapshot.NewTarget(c)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := t.Test(ctx); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "target": c.Describe()})
}

func (s *Server) publishStatusSnapshot(w http.ResponseWriter, r *http.Request) {
	c, _, err := s.loadStatusSnapshot(r.Context())
	if err == nil {
		setAuditTarget(r, c.Describe())
	}
	l, err := s.snapshot.PublishNow(r.Context())
	if err != nil {
		if l.At.IsZero() {
			writeErr(w, http.StatusBadRequest, capitalize(err.Error())+".")
			return
		}
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, l)
}
