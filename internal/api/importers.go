package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/lint"
	"moorage/internal/migrate"
	"moorage/internal/notify"
	"moorage/internal/store"
)

// Imports from other tools (M18C). Previews change nothing; apply saves through the same
// paths as the UI forms (stack save path with one history version per stack, check /
// channel / status page validation). Nothing is deployed or started on a host.

// ---- Portainer ---------------------------------------------------------------

type portainerReq struct {
	URL      string `json:"url"`
	Token    string `json:"token"`
	Insecure bool   `json:"insecure"`
	HostID   string `json:"hostId"`
	Stacks   []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"stacks"`
}

// readPortainerReq reads the request and opens the client. The token stays in the
// returned client only; errors never repeat it.
func (s *Server) readPortainerReq(w http.ResponseWriter, r *http.Request) (portainerReq, *migrate.Portainer, host.Host, bool) {
	var req portainerReq
	if !readJSON(w, r, &req) {
		return req, nil, nil, false
	}
	req.HostID = strings.TrimSpace(req.HostID)
	if req.HostID == "" {
		req.HostID = host.LocalID
	}
	h, code, msg := s.resolveHostID(r.Context(), req.HostID, true)
	if h == nil {
		writeErr(w, code, msg)
		return req, nil, nil, false
	}
	pc, err := migrate.NewPortainer(req.URL, req.Token, req.Insecure)
	req.Token = ""
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return req, nil, nil, false
	}
	return req, pc, h, true
}

// portainerErr answers a Portainer failure (its message is safe to show).
func portainerErr(w http.ResponseWriter, err error) {
	var pe *migrate.PortainerError
	if errors.As(err, &pe) {
		writeErr(w, http.StatusBadGateway, pe.Msg)
		return
	}
	writeErr(w, http.StatusBadGateway, "Talking to Portainer failed.")
}

// stackNameSets returns the managed stack names of a host and the compose projects that
// run there without being managed.
func stackNameSets(ctx context.Context, h host.Host) (managed, running map[string]bool) {
	managed, running = map[string]bool{}, map[string]bool{}
	list, _ := h.ListStacks(ctx)
	for _, st := range list {
		if st.Managed {
			managed[st.Name] = true
		} else {
			running[st.Name] = true
		}
	}
	return managed, running
}

// POST /api/import/portainer/preview
func (s *Server) portainerPreview(w http.ResponseWriter, r *http.Request) {
	_, pc, h, ok := s.readPortainerReq(w, r)
	if !ok {
		return
	}
	defer pc.Close()
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	stacks, err := pc.Stacks(ctx)
	if err != nil {
		portainerErr(w, err)
		return
	}
	endpoints := pc.Endpoints(ctx)
	managed, running := stackNameSets(ctx, h)

	files := make([]string, len(stacks))
	ferrs := make([]error, len(stacks))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, st := range stacks {
		if st.Unsupported() != "" {
			continue
		}
		wg.Add(1)
		go func(i, id int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			files[i], ferrs[i] = pc.StackFile(ctx, id)
		}(i, st.ID)
	}
	wg.Wait()

	out := make([]migrate.StackPreview, 0, len(stacks))
	seen := map[string]bool{}
	for i, st := range stacks {
		pv := migrate.PreviewStack(st, endpoints[st.EndpointID], files[i], ferrs[i], managed, running)
		if pv.Status == migrate.StatusReady {
			if seen[pv.Target] {
				pv.Status, pv.Reason = migrate.StatusExists, "Another Portainer stack in this list has the same name; pick another one."
			} else if msg := s.importCheckStack(ctx, h, pv.Target, pv.Compose, st); msg != "" {
				pv.Status, pv.Reason = migrate.StatusInvalid, msg
			}
		}
		if pv.Target != "" {
			seen[pv.Target] = true
		}
		out = append(out, pv)
	}
	writeJSON(w, http.StatusOK, map[string]any{"stacks": out})
}

// importCheckStack runs the save path's validation without saving: compose parsing and
// the linter's blocking rules. It returns "" when the stack would save.
func (s *Server) importCheckStack(ctx context.Context, h host.Host, name, compose string, st migrate.PortainerStack) string {
	env, _ := migrate.EnvFile(st)
	if err := h.Validate(ctx, name, compose, env); err != nil {
		_, msg := s.errStatus(err)
		return msg
	}
	if b := lint.Blocking(lint.Lint(ctx, compose, env, lint.Options{Name: name}), false); b != nil {
		return b.Message
	}
	return ""
}

type importResult struct {
	Kind   string `json:"kind,omitempty"`
	ID     int    `json:"id,omitempty"`
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// POST /api/import/portainer/apply
func (s *Server) portainerApply(w http.ResponseWriter, r *http.Request) {
	req, pc, h, ok := s.readPortainerReq(w, r)
	if !ok {
		return
	}
	defer pc.Close()
	if len(req.Stacks) == 0 {
		writeErr(w, http.StatusBadRequest, "Select at least one stack to import.")
		return
	}
	if len(req.Stacks) > migrate.MaxPortainerStacks {
		writeErr(w, http.StatusBadRequest, "Too many stacks in one import.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	stacks, err := pc.Stacks(ctx)
	if err != nil {
		portainerErr(w, err)
		return
	}
	byID := map[int]migrate.PortainerStack{}
	for _, st := range stacks {
		byID[st.ID] = st
	}
	endpoints := pc.Endpoints(ctx)
	user := currentUser(r).Username
	results := make([]importResult, 0, len(req.Stacks))
	used := map[string]bool{}
	var created []string
	for _, item := range req.Stacks {
		name := strings.TrimSpace(item.Name)
		res := importResult{ID: item.ID, Name: name}
		st, found := byID[item.ID]
		switch {
		case !found:
			res.Reason = "Portainer no longer lists this stack."
		case st.Unsupported() != "":
			res.Reason = st.Unsupported()
		case !migrate.ValidStackName(name):
			res.Reason = "Stack name: " + engine.ErrInvalidName.Error() + "."
		case used[name]:
			res.Reason = "Two selected stacks have the same name."
		}
		if res.Reason != "" {
			results = append(results, res)
			continue
		}
		used[name] = true
		file, err := pc.StackFile(ctx, st.ID)
		switch {
		case err != nil:
			res.Reason = "Portainer didn't return the compose file: " + err.Error()
		case strings.TrimSpace(file) == "":
			res.Reason = "Portainer returned no compose file."
		case len(file) > migrate.MaxComposeSize:
			res.Reason = "The compose file is larger than 512 KB."
		}
		if res.Reason != "" {
			results = append(results, res)
			continue
		}
		env, _ := migrate.EnvFile(st)
		note := "Imported from Portainer stack " + st.Name
		if ep := endpoints[st.EndpointID]; ep != "" {
			note += " (" + ep + ")"
		}
		if nc, isNoted := h.(host.NotedCreator); isNoted {
			err = nc.CreateNoted(ctx, name, file, env, user, note)
		} else {
			err = h.Save(ctx, name, file, env, user, true)
		}
		if err != nil {
			_, res.Reason = s.errStatus(err)
			results = append(results, res)
			continue
		}
		res.OK = true
		created = append(created, name)
		results = append(results, res)
		if s.mon != nil {
			s.mon.StackEvent(req.HostID, name, "create", user, "imported from Portainer")
		}
	}
	s.log.Info("portainer import", "host", req.HostID, "created", len(created), "requested", len(req.Stacks), "user", user)
	if len(created) > 0 {
		setAuditTarget(r, auditList(fmt.Sprintf("%d stacks on %s", len(created), req.HostID), created))
	}
	resp := map[string]any{"results": results, "created": len(created)}
	code := http.StatusOK
	if len(created) == 0 {
		// Nothing changed: a refusal, which the audit middleware doesn't record.
		code, resp["error"] = http.StatusUnprocessableEntity, "Nothing was imported."
	}
	writeJSON(w, code, resp)
}

// auditList is "<head>: a, b, c" cut to a readable length.
func auditList(head string, names []string) string {
	t := head + ": " + strings.Join(names, ", ")
	if r := []rune(t); len(r) > 200 {
		t = string(r[:197]) + "…"
	}
	return t
}

// ---- Uptime Kuma / Gatus -------------------------------------------------------

type monitorImportReq struct {
	Source   string   `json:"source"`
	Content  string   `json:"content"`
	Selected []string `json:"selected"`
	// Pause creates every check paused (the source's paused checks stay paused anyway).
	Pause bool `json:"pause"`
}

func (s *Server) readMonitorImport(w http.ResponseWriter, r *http.Request) (monitorImportReq, *migrate.Plan, bool) {
	var req monitorImportReq
	// JSON escaping can double the file's size on the wire.
	r.Body = http.MaxBytesReader(w, r.Body, 2*migrate.MaxUpload+64<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "The file couldn't be read. It can be at most 5 MB.")
		return req, nil, false
	}
	if len(req.Content) > migrate.MaxUpload {
		writeErr(w, http.StatusRequestEntityTooLarge, "The file is larger than 5 MB.")
		return req, nil, false
	}
	var (
		plan *migrate.Plan
		err  error
	)
	switch req.Source {
	case migrate.SourceKuma:
		plan, err = migrate.ParseKuma([]byte(req.Content))
	case migrate.SourceGatus:
		plan, err = migrate.ParseGatus([]byte(req.Content))
	default:
		writeErr(w, http.StatusBadRequest, "Pick what to import from: Uptime Kuma or Gatus.")
		return req, nil, false
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, sentence(err))
		return req, nil, false
	}
	return req, plan, true
}

// existingMonitoring indexes what a re-run must not create again.
type existingMonitoring struct {
	checks   map[string]int64         // by name
	channels map[string]store.Channel // by name
	slugs    map[string]bool
}

func (s *Server) existingMonitoring(ctx context.Context) (existingMonitoring, error) {
	ex := existingMonitoring{checks: map[string]int64{}, channels: map[string]store.Channel{}, slugs: map[string]bool{}}
	checks, err := s.store.Checks(ctx)
	if err != nil {
		return ex, err
	}
	for _, c := range checks {
		ex.checks[c.Name] = c.ID
	}
	chs, err := s.store.Channels(ctx)
	if err != nil {
		return ex, err
	}
	for _, c := range chs {
		ex.channels[c.Name] = c
	}
	pages, err := s.store.StatusPages(ctx)
	if err != nil {
		return ex, err
	}
	for _, p := range pages {
		ex.slugs[p.Slug] = true
	}
	return ex, nil
}

func checkInputOf(c migrate.CheckItem) checkInput {
	return checkInput{
		Name: c.Name, Kind: c.Kind, Target: c.Target, Interval: c.Interval, Timeout: c.Timeout,
		Retries: c.Retries, Settings: c.Settings, Enabled: c.Enabled,
	}
}

// preview DTOs: Exists marks what a run would skip (or reuse), Error what would fail.
type checkPreviewDTO struct {
	migrate.CheckItem
	Exists bool   `json:"exists"`
	Error  string `json:"error,omitempty"`
}

type channelPreviewDTO struct {
	migrate.ChannelItem
	Exists bool   `json:"exists"`
	Error  string `json:"error,omitempty"`
}

type pagePreviewDTO struct {
	migrate.PageItem
	Exists bool `json:"exists"`
}

// POST /api/import/monitors/preview
func (s *Server) monitorImportPreview(w http.ResponseWriter, r *http.Request) {
	_, plan, ok := s.readMonitorImport(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	ex, err := s.existingMonitoring(ctx)
	if err != nil {
		s.engErr(w, err)
		return
	}
	checks := make([]checkPreviewDTO, 0, len(plan.Checks))
	for _, c := range plan.Checks {
		d := checkPreviewDTO{CheckItem: c}
		_, d.Exists = ex.checks[c.Name]
		in := checkInputOf(c)
		if err := s.validateCheck(ctx, &in); err != nil {
			d.Error = err.Error()
		}
		checks = append(checks, d)
	}
	chans := make([]channelPreviewDTO, 0, len(plan.Channels))
	for _, c := range plan.Channels {
		d := channelPreviewDTO{ChannelItem: c}
		if old, found := ex.channels[c.Name]; found {
			d.Exists = true
			if old.Kind != c.Kind {
				d.Error = "A channel with this name but another type exists."
			}
		} else if err := notify.Validate(c.Kind, c.Config); err != nil {
			d.Error = sentence(err)
		}
		chans = append(chans, d)
	}
	pages := make([]pagePreviewDTO, 0, len(plan.Pages))
	for _, p := range plan.Pages {
		pages = append(pages, pagePreviewDTO{PageItem: p, Exists: ex.slugs[p.Slug]})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"source": plan.Source, "checks": checks, "channels": chans, "pages": pages,
		"skipped": plan.Skipped, "notes": plan.Notes,
	})
}

// POST /api/import/monitors/apply
//
// Re-running an import is safe: checks and channels are matched by name and status pages
// by slug. Existing ones are never changed or duplicated; a check that exists is skipped,
// a channel that exists (same type) is reused for the new checks, a page whose slug is
// taken is skipped. Pages point at existing checks of the same name.
func (s *Server) monitorImportApply(w http.ResponseWriter, r *http.Request) {
	req, plan, ok := s.readMonitorImport(w, r)
	if !ok {
		return
	}
	if len(req.Selected) == 0 {
		writeErr(w, http.StatusBadRequest, "Select at least one item to import.")
		return
	}
	sel := map[string]bool{}
	for _, k := range req.Selected {
		sel[k] = true
	}
	ctx := r.Context()
	ex, err := s.existingMonitoring(ctx)
	if err != nil {
		s.engErr(w, err)
		return
	}
	var results []importResult
	add := func(kind, name string, ok bool, reason string) {
		results = append(results, importResult{Kind: kind, Name: name, OK: ok, Reason: reason})
	}
	counts := map[string]int{}

	chanIDs := map[string]int64{}
	for _, c := range plan.Channels {
		if old, found := ex.channels[c.Name]; found {
			if old.Kind == c.Kind {
				chanIDs[c.Key] = old.ID // reused even when not selected: the checks keep their alerts
				if sel[c.Key] {
					add("channel", c.Name, false, "Already exists; the existing channel is used.")
				}
			} else if sel[c.Key] {
				add("channel", c.Name, false, "A channel with this name but another type exists.")
			}
			continue
		}
		if !sel[c.Key] {
			continue
		}
		if err := notify.Validate(c.Kind, c.Config); err != nil {
			add("channel", c.Name, false, sentence(err))
			continue
		}
		b, _ := json.Marshal(c.Config)
		id, err := s.store.SaveChannel(ctx, store.Channel{Name: c.Name, Kind: c.Kind, Config: string(b), Enabled: c.Enabled})
		if err != nil {
			add("channel", c.Name, false, "Saving failed.")
			s.log.Warn("import channel", "err", err)
			continue
		}
		chanIDs[c.Key] = id
		counts["channels"]++
		add("channel", c.Name, true, "")
	}

	checkIDs := map[string]int64{}
	for _, c := range plan.Checks {
		if id, found := ex.checks[c.Name]; found {
			checkIDs[c.Key] = id
			if sel[c.Key] {
				add("check", c.Name, false, "Already exists; skipped.")
			}
			continue
		}
		if !sel[c.Key] {
			continue
		}
		in := checkInputOf(c)
		in.Enabled = c.Enabled && !req.Pause
		for _, k := range c.Channels {
			if id, ok := chanIDs[k]; ok {
				in.Channels = append(in.Channels, id)
			}
		}
		if err := s.validateCheck(ctx, &in); err != nil {
			add("check", c.Name, false, err.Error())
			continue
		}
		id, err := s.store.SaveCheck(ctx, toStoreCheck(0, in))
		if err != nil {
			add("check", c.Name, false, "Saving failed.")
			s.log.Warn("import check", "err", err)
			continue
		}
		if s.mon != nil {
			s.mon.Reload(id)
		}
		checkIDs[c.Key] = id
		counts["checks"]++
		add("check", c.Name, true, "")
	}

	for _, p := range plan.Pages {
		if !sel[p.Key] {
			continue
		}
		if ex.slugs[p.Slug] {
			add("page", p.Title, false, "A status page with the URL slug "+p.Slug+" exists; skipped.")
			continue
		}
		in := statusPageInput{Slug: p.Slug, Title: p.Title, Description: p.Description}
		for _, e := range p.Entries {
			if id, ok := checkIDs[e.CheckKey]; ok {
				in.Items = append(in.Items, statusPageItemDTO{Section: e.Section, DisplayName: e.DisplayName, CheckID: id})
			}
		}
		if len(in.Items) == 0 {
			add("page", p.Title, false, "None of its checks were imported.")
			continue
		}
		if err := s.validateStatusPage(ctx, 0, &in); err != nil {
			add("page", p.Title, false, err.Error())
			continue
		}
		if _, err := s.store.SaveStatusPage(ctx, toStoreStatusPage(0, in)); err != nil {
			add("page", p.Title, false, "Saving failed.")
			s.log.Warn("import status page", "err", err)
			continue
		}
		counts["pages"]++
		add("page", p.Title, true, "")
	}
	if counts["checks"]+counts["pages"] > 0 && s.status != nil {
		s.status.Invalidate()
	}
	if counts["channels"] > 0 && s.hub != nil {
		go s.hub.PushWatchdog(context.Background())
	}
	total := counts["checks"] + counts["channels"] + counts["pages"]
	s.log.Info("monitor import", "source", plan.Source, "checks", counts["checks"], "channels", counts["channels"], "pages", counts["pages"], "user", currentUser(r).Username)
	if total > 0 {
		setAuditTarget(r, fmt.Sprintf("%s: %d checks, %d channels, %d status pages", plan.Source, counts["checks"], counts["channels"], counts["pages"]))
	}
	if results == nil {
		results = []importResult{}
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].OK && !results[j].OK })
	resp := map[string]any{"results": results, "created": counts}
	code := http.StatusOK
	if total == 0 {
		// Nothing changed: a refusal, which the audit middleware doesn't record.
		code, resp["error"] = http.StatusUnprocessableEntity, "Nothing was imported."
	}
	writeJSON(w, code, resp)
}
