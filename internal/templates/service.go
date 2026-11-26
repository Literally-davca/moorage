package templates

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"moorage/internal/store"
)

//go:embed all:bundled
var bundledFS embed.FS

// Source keys used in the API.
const (
	SourceBundled = "bundled"
	SourceLocal   = "local"
	remotePrefix  = "remote-"
	// MaxSources bounds the number of remote indexes.
	MaxSources = 20
	// refreshEvery is how old a remote copy may get before the daily refresh fetches it again.
	refreshEvery = 24 * time.Hour
)

// ErrNotFound means the source or the template doesn't exist.
var ErrNotFound = errors.New("template not found")

// BrokenError is returned for a template that is listed but can't be used.
type BrokenError struct{ Msg string }

func (e BrokenError) Error() string { return e.Msg }

// Entry is one template in the catalogue.
type Entry struct {
	Source      string `json:"source"`
	SourceLabel string `json:"sourceLabel"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description"`
	Category    string `json:"category"`
	Website     string `json:"website,omitempty"`
	Error       string `json:"error,omitempty"`
}

// SourceInfo describes one source of the catalogue and its problems.
type SourceInfo struct {
	Key        string    `json:"key"`
	Kind       string    `json:"kind"` // bundled, local, remote
	Label      string    `json:"label"`
	Name       string    `json:"name,omitempty"` // remote: the index's own name
	Dir        string    `json:"dir,omitempty"`  // local: the folder
	Count      int       `json:"count"`
	Error      string    `json:"error,omitempty"`
	LastOK     time.Time `json:"lastOk,omitzero"`
	Refreshing bool      `json:"refreshing,omitempty"`
}

// Catalog is every template of every source.
type Catalog struct {
	Templates []Entry      `json:"templates"`
	Sources   []SourceInfo `json:"sources"`
	LocalDir  string       `json:"localDir,omitempty"`
}

// RemoteSource is a remote index as admins manage it.
type RemoteSource struct {
	ID          int64     `json:"id"`
	Key         string    `json:"key"`
	URL         string    `json:"url"`
	Host        string    `json:"host"`
	AllowLocal  bool      `json:"allowLocal"`
	Name        string    `json:"name,omitempty"`
	LastAttempt time.Time `json:"lastAttempt,omitzero"`
	LastOK      time.Time `json:"lastOk,omitzero"`
	LastError   string    `json:"lastError,omitempty"`
	Count       int       `json:"count"`
	Refreshing  bool      `json:"refreshing"`
}

// Service is the template catalogue.
type Service struct {
	st    *store.Store
	dir   string // local templates folder; "" = off
	log   *slog.Logger
	Fetch *Fetcher

	bundled []item

	mu      sync.Mutex
	running map[int64]bool
	ctx     context.Context // background refreshes end with it
	wg      sync.WaitGroup
}

// New creates the catalogue. dir is MOORAGE_TEMPLATES_DIR ("" turns the local folder off).
func New(st *store.Store, dir string, log *slog.Logger) *Service {
	s := &Service{st: st, dir: dir, log: log, Fetch: &Fetcher{}, running: map[int64]bool{}, ctx: context.Background()}
	sub, err := fs.Sub(bundledFS, "bundled")
	if err == nil {
		s.bundled, err = loadFolders(sub, true)
	}
	if err != nil {
		log.Error("bundled templates", "err", err)
	}
	return s
}

// Start runs the daily refresh of remote indexes until ctx ends.
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	go func() {
		// Shortly after start, then hourly: fetch indexes whose copy is older than a day.
		t := time.NewTimer(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			s.refreshStale(ctx)
			t.Reset(time.Hour)
		}
	}()
}

func (s *Service) refreshStale(ctx context.Context) {
	if s.st == nil {
		return
	}
	list, err := s.st.TemplateSources(ctx)
	if err != nil {
		s.log.Warn("templates: list sources", "err", err)
		return
	}
	for _, src := range list {
		if time.Since(src.LastAttempt) >= refreshEvery {
			s.Refresh(src.ID)
		}
	}
}

// Wait blocks until running background refreshes are done (tests, shutdown).
func (s *Service) Wait() { s.wg.Wait() }

// Refresh fetches one index in the background; a refresh already running is not doubled.
func (s *Service) Refresh(id int64) {
	s.mu.Lock()
	if s.running[id] {
		s.mu.Unlock()
		return
	}
	s.running[id] = true
	ctx := s.ctx
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.running, id)
			s.mu.Unlock()
		}()
		if err := s.RefreshNow(ctx, id); err != nil {
			s.log.Warn("templates: refresh index", "source", id, "err", err)
		}
	}()
}

func (s *Service) isRunning(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running[id]
}

// RefreshNow fetches one index and stores the result. On failure the last good copy
// stays and the error is recorded on the source.
func (s *Service) RefreshNow(ctx context.Context, id int64) error {
	src, err := s.st.TemplateSourceByID(ctx, id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	idx, list, err := s.Fetch.Fetch(ctx, src.URL, src.AllowLocal)
	now := time.Now()
	if err != nil {
		if ferr := s.st.TemplateFetchFailed(context.WithoutCancel(ctx), id, now, err.Error()); ferr != nil {
			return ferr
		}
		return err
	}
	cached := make([]store.CachedTemplate, 0, len(list))
	for _, t := range list {
		cached = append(cached, store.CachedTemplate{ID: t.ID, Meta: t.Meta, Compose: t.Compose, Env: t.Env, Err: t.Err})
	}
	if err := s.st.TemplateFetchOK(context.WithoutCancel(ctx), id, src.URL, now, idx.Name, cached); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("store index: %w", err)
	}
	return nil
}

// RemoteKey is the source key of a remote index.
func RemoteKey(id int64) string { return remotePrefix + strconv.FormatInt(id, 10) }

func parseRemoteKey(key string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimPrefix(key, remotePrefix), 10, 64)
	return n, strings.HasPrefix(key, remotePrefix) && err == nil && n > 0
}

func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// List returns every template of every source. Broken templates are listed with their error.
func (s *Service) List(ctx context.Context) (Catalog, error) {
	c := Catalog{Templates: []Entry{}, Sources: []SourceInfo{}, LocalDir: s.dir}
	c.add(SourceInfo{Key: SourceBundled, Kind: "bundled", Label: "Bundled"}, s.bundled, nil)
	if s.dir != "" {
		items, err := loadFolders(os.DirFS(s.dir), false)
		info := SourceInfo{Key: SourceLocal, Kind: "local", Label: "Local folder", Dir: s.dir}
		// Moorage creates the folder at start; an empty (or, if creating it failed,
		// missing) folder only means no local templates, not a problem to show.
		c.add(info, items, err)
	}
	if s.st == nil {
		return c, nil
	}
	srcs, err := s.st.TemplateSources(ctx)
	if err != nil {
		return c, fmt.Errorf("template sources: %w", err)
	}
	metas, err := s.st.CachedTemplateMetas(ctx)
	if err != nil {
		return c, fmt.Errorf("cached templates: %w", err)
	}
	bySource := map[int64][]item{}
	for _, m := range metas {
		bySource[m.SourceID] = append(bySource[m.SourceID], cachedItem(m))
	}
	for _, src := range srcs {
		info := SourceInfo{
			Key: RemoteKey(src.ID), Kind: "remote", Label: "from " + urlHost(src.URL), Name: src.Name,
			Error: src.LastError, LastOK: src.LastOK, Refreshing: s.isRunning(src.ID),
		}
		if src.LastAttempt.IsZero() && info.Error == "" && !info.Refreshing {
			info.Error = "Not fetched yet."
		}
		c.add(info, bySource[src.ID], nil)
	}
	return c, nil
}

// cachedItem parses a cached template.yaml for the list (files are loaded on Get).
func cachedItem(m store.CachedTemplate) item {
	it := item{id: m.ID, err: m.Err}
	if it.err != "" {
		return it
	}
	meta, err := ParseMeta([]byte(m.Meta))
	if err != nil {
		it.err = err.Error()
		return it
	}
	it.t = Template{Meta: meta}
	return it
}

func (c *Catalog) add(info SourceInfo, items []item, err error) {
	if err != nil && info.Error == "" {
		info.Error = err.Error()
	}
	info.Count = len(items)
	c.Sources = append(c.Sources, info)
	for _, it := range items {
		e := Entry{Source: info.Key, SourceLabel: info.Label, ID: it.id, Name: it.id, Error: it.err}
		if it.err == "" {
			m := it.t.Meta
			e.Name, e.Version, e.Description, e.Category, e.Website = m.Name, m.Version, m.Description, m.Category, m.Website
		}
		c.Templates = append(c.Templates, e)
	}
}

// Get loads one template with its files. A listed but broken template returns BrokenError.
func (s *Service) Get(ctx context.Context, source, id string) (Template, Entry, error) {
	if !IDPattern.MatchString(id) {
		return Template{}, Entry{}, ErrNotFound
	}
	switch {
	case source == SourceBundled:
		return find(s.bundled, Entry{Source: source, SourceLabel: "Bundled"}, id)
	case source == SourceLocal && s.dir != "":
		fsys := os.DirFS(s.dir)
		if _, err := fs.Stat(fsys, id); err != nil {
			return Template{}, Entry{}, ErrNotFound
		}
		t, err := loadFolder(fsys, id, true)
		return found(Entry{Source: source, SourceLabel: "Local folder"}, item{id: id, t: t, err: errString(err)})
	}
	sid, ok := parseRemoteKey(source)
	if !ok || s.st == nil {
		return Template{}, Entry{}, ErrNotFound
	}
	src, err := s.st.TemplateSourceByID(ctx, sid)
	if errors.Is(err, store.ErrNotFound) {
		return Template{}, Entry{}, ErrNotFound
	}
	if err != nil {
		return Template{}, Entry{}, err
	}
	ct, err := s.st.CachedTemplate(ctx, sid, id)
	if errors.Is(err, store.ErrNotFound) {
		return Template{}, Entry{}, ErrNotFound
	}
	if err != nil {
		return Template{}, Entry{}, err
	}
	it := cachedItem(ct)
	if it.err == "" {
		it.t, err = NewTemplate(it.t.Meta, ct.Compose, ct.Env)
		it.err = errString(err)
	}
	return found(Entry{Source: source, SourceLabel: "from " + urlHost(src.URL)}, it)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func find(items []item, base Entry, id string) (Template, Entry, error) {
	for _, it := range items {
		if it.id == id {
			return found(base, it)
		}
	}
	return Template{}, Entry{}, ErrNotFound
}

func found(base Entry, it item) (Template, Entry, error) {
	base.ID, base.Name, base.Error = it.id, it.id, it.err
	if it.err != "" {
		return Template{}, base, BrokenError{Msg: it.err}
	}
	m := it.t.Meta
	base.Name, base.Version, base.Description, base.Category, base.Website = m.Name, m.Version, m.Description, m.Category, m.Website
	return it.t, base, nil
}

// Note is the first version message of a stack created from a template, e.g.
// "Created from template postgres (bundled, version 1)".
func Note(e Entry) string {
	label := e.SourceLabel
	if e.Source == SourceBundled || e.Source == SourceLocal {
		label = strings.ToLower(label)
	}
	if e.Version != "" {
		label += ", version " + e.Version
	}
	return oneLine(fmt.Sprintf("Created from template %s (%s)", e.ID, label), 200)
}

// ---- remote sources --------------------------------------------------------

// Sources lists the remote indexes with their state, for admins.
func (s *Service) Sources(ctx context.Context) ([]RemoteSource, error) {
	srcs, err := s.st.TemplateSources(ctx)
	if err != nil {
		return nil, err
	}
	metas, err := s.st.CachedTemplateMetas(ctx)
	if err != nil {
		return nil, err
	}
	counts := map[int64]int{}
	for _, m := range metas {
		counts[m.SourceID]++
	}
	out := make([]RemoteSource, 0, len(srcs))
	for _, src := range srcs {
		out = append(out, RemoteSource{
			ID: src.ID, Key: RemoteKey(src.ID), URL: src.URL, Host: urlHost(src.URL), AllowLocal: src.AllowLocal, Name: src.Name,
			LastAttempt: src.LastAttempt, LastOK: src.LastOK, LastError: src.LastError, Count: counts[src.ID], Refreshing: s.isRunning(src.ID),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// InputError is a refused source URL or setting.
type InputError struct{ Msg string }

func (e InputError) Error() string { return e.Msg }

// AddSource stores a new index and fetches it in the background.
func (s *Service) AddSource(ctx context.Context, rawURL string, allowLocal bool) (int64, error) {
	u, err := ValidateIndexURL(rawURL)
	if err != nil {
		return 0, InputError{err.Error()}
	}
	list, err := s.st.TemplateSources(ctx)
	if err != nil {
		return 0, err
	}
	if len(list) >= MaxSources {
		return 0, InputError{fmt.Sprintf("At most %d template indexes can be added.", MaxSources)}
	}
	id, err := s.st.AddTemplateSource(ctx, u, allowLocal)
	if errors.Is(err, store.ErrDuplicate) {
		return 0, InputError{"This index is already added."}
	}
	if err != nil {
		return 0, err
	}
	s.Refresh(id)
	return id, nil
}

// UpdateSource changes an index and fetches it again.
func (s *Service) UpdateSource(ctx context.Context, id int64, rawURL string, allowLocal bool) error {
	u, err := ValidateIndexURL(rawURL)
	if err != nil {
		return InputError{err.Error()}
	}
	err = s.st.UpdateTemplateSource(ctx, id, u, allowLocal)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, store.ErrDuplicate):
		return InputError{"This index is already added."}
	case err != nil:
		return err
	}
	s.Refresh(id)
	return nil
}

// DeleteSource removes an index and its cached templates.
func (s *Service) DeleteSource(ctx context.Context, id int64) error {
	err := s.st.DeleteTemplateSource(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// RefreshSource checks the index exists and fetches it in the background.
func (s *Service) RefreshSource(ctx context.Context, id int64) error {
	if _, err := s.st.TemplateSourceByID(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	s.Refresh(id)
	return nil
}
