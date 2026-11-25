package api

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"moorage/internal/engine"
	"moorage/internal/store"
	"moorage/internal/updater"
)

type updateSettingsDTO struct {
	store.UpdateSettings
	Timezone string `json:"timezone"`
}

type stackImagesDTO struct {
	HostID         string                `json:"hostId"`
	HostName       string                `json:"hostName"`
	ReadOnly       bool                  `json:"readOnly"`
	Stack          string                `json:"stack"`
	DisplayName    string                `json:"displayName,omitempty"`
	CoolifyProject string                `json:"coolifyProject,omitempty"`
	CoolifyUUID    string                `json:"coolifyUuid,omitempty"`
	Managed        bool                  `json:"managed"`
	Status         string                `json:"status"`
	Running        int                   `json:"running"`
	Policy         string                `json:"policy"`
	AutoReason     string                `json:"autoReason,omitempty"`
	CheckedAt      time.Time             `json:"checkedAt,omitzero"`
	Services       []store.ImageCheckRow `json:"services"`
	LastUpdate     *store.ImageUpdate    `json:"lastUpdate,omitempty"`
	Updates        []store.ImageUpdate   `json:"updates,omitempty"`
}

func hubTimezoneName() string {
	if tz := strings.TrimSpace(os.Getenv("TZ")); tz != "" {
		return tz
	}
	name := time.Local.String()
	if name == "" || name == "Local" {
		z, _ := time.Now().Zone()
		if z != "" {
			return z
		}
		return "UTC"
	}
	return name
}

func (s *Server) listUpdates(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.store.UpdateSettings(ctx)
	if err != nil {
		s.engErr(w, err)
		return
	}
	allChecks, _ := s.store.ImageChecks(ctx, "", "")
	checksByStack := map[string][]store.ImageCheckRow{}
	for _, c := range allChecks {
		key := c.HostID + "/" + c.Stack
		checksByStack[key] = append(checksByStack[key], c)
	}
	policies, _ := s.store.AllStackUpdatePolicies(ctx)
	recentUpdates, _ := s.store.ImageUpdates(ctx, "", "", 100)
	lastUpdateByStack := map[string]store.ImageUpdate{}
	for _, u := range recentUpdates {
		key := u.HostID + "/" + u.Stack
		if _, exists := lastUpdateByStack[key]; !exists {
			lastUpdateByStack[key] = u
		}
	}

	var stacksOut []stackImagesDTO
	if s.reg != nil {
		hosts, _ := s.reg.List(ctx)
		for _, he := range hosts {
			if !he.Connected {
				continue
			}
			h, _, err := s.reg.Get(ctx, he.ID)
			if err != nil {
				continue
			}
			stacks, err := h.ListStacks(ctx)
			if err != nil {
				continue
			}
			for _, sm := range stacks {
				key := he.ID + "/" + sm.Name
				pol := store.PolicyNotify
				if p, ok := policies[key]; ok && p.Policy != "" {
					pol = p.Policy
				}
				rows := checksByStack[key]
				if rows == nil {
					rows = []store.ImageCheckRow{}
				}
				var checkedAt time.Time
				for _, row := range rows {
					if row.CheckedAt.After(checkedAt) {
						checkedAt = row.CheckedAt
					}
				}
				dto := stackImagesDTO{
					HostID:     he.ID,
					HostName:   he.Name,
					ReadOnly:   he.ReadOnly,
					Stack:      sm.Name,
					Managed:    sm.Managed,
					Status:     sm.Status,
					Running:    sm.Running,
					Policy:     pol,
					AutoReason: autoDisabledReason(he.ReadOnly, sm.Managed),
					CheckedAt:  checkedAt,
					Services:   rows,
				}
				if cr := s.CoolifyMetaOf(ctx, he.ID, sm.Name); cr != nil {
					dto.DisplayName = cr.Name
					dto.CoolifyProject = cr.Project
					dto.CoolifyUUID = cr.UUID
				}
				if lu, ok := lastUpdateByStack[key]; ok {
					uCopy := lu
					dto.LastUpdate = &uCopy
				}
				stacksOut = append(stacksOut, dto)
			}
		}
	}
	if stacksOut == nil {
		stacksOut = []stackImagesDTO{}
	}
	if recentUpdates == nil {
		recentUpdates = []store.ImageUpdate{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"settings": updateSettingsDTO{UpdateSettings: st, Timezone: hubTimezoneName()},
		"stacks":   stacksOut,
		"updates":  recentUpdates,
	})
}

func autoDisabledReason(readOnly, managed bool) string {
	if readOnly {
		return "This host runs in read-only mode; automatic updates are disabled."
	}
	if !managed {
		return "This stack lives outside the Moorage stacks folder; automatic updates are disabled."
	}
	return ""
}

func (s *Server) saveUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var in store.UpdateSettings
	if !readJSON(w, r, &in) {
		return
	}
	if in.Interval == 0 {
		in.Interval = 21600
	}
	if in.Interval < 3600 || in.Interval > 7*86400 {
		writeErr(w, http.StatusBadRequest, "The check interval must be between 1 hour and 7 days.")
		return
	}
	in.WindowStart = strings.TrimSpace(in.WindowStart)
	in.WindowEnd = strings.TrimSpace(in.WindowEnd)
	if _, _, ok := updater.ParseHHMM(in.WindowStart); !ok {
		writeErr(w, http.StatusBadRequest, "Window start must be HH:MM (for example 03:00).")
		return
	}
	if _, _, ok := updater.ParseHHMM(in.WindowEnd); !ok {
		writeErr(w, http.StatusBadRequest, "Window end must be HH:MM (for example 05:00).")
		return
	}
	if in.WindowStart == in.WindowEnd {
		writeErr(w, http.StatusBadRequest, "The apply window start and end cannot be the same time.")
		return
	}
	if err := s.store.SaveUpdateSettings(r.Context(), in); err != nil {
		s.engErr(w, err)
		return
	}
	if s.upd != nil {
		s.upd.Reload()
	}
	s.log.Info("update settings saved", "enabled", in.Enabled, "interval", in.Interval, "window", in.WindowStart+"-"+in.WindowEnd, "user", currentUser(r).Username)
	writeJSON(w, http.StatusOK, updateSettingsDTO{UpdateSettings: in, Timezone: hubTimezoneName()})
}

func (s *Server) checkUpdatesNow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		HostID string `json:"hostId"`
		Stack  string `json:"stack"`
	}
	if r.ContentLength > 0 {
		if !readJSON(w, r, &req) {
			return
		}
	}
	rows, err := s.upd.CheckNow(r.Context(), strings.TrimSpace(req.HostID), strings.TrimSpace(req.Stack))
	if errors.Is(err, updater.ErrRateLimited) {
		writeErr(w, http.StatusTooManyRequests, err.Error())
		return
	}
	if err != nil {
		s.engErr(w, err)
		return
	}
	if rows == nil {
		rows = []store.ImageCheckRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": rows})
}

func (s *Server) getStackImages(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, false)
	if !ok {
		return
	}
	ctx := r.Context()
	hostID, name := r.PathValue("host"), r.PathValue("name")
	_, he, _ := s.reg.Get(ctx, hostID)
	st, err := h.GetStack(ctx, name)
	if err != nil {
		s.engErr(w, err)
		return
	}
	pol, _ := s.store.StackUpdatePolicy(ctx, hostID, name)
	rows, _ := s.store.ImageChecks(ctx, hostID, name)
	if rows == nil {
		rows = []store.ImageCheckRow{}
	}
	var checkedAt time.Time
	for _, row := range rows {
		if row.CheckedAt.After(checkedAt) {
			checkedAt = row.CheckedAt
		}
	}
	updates, _ := s.store.ImageUpdates(ctx, hostID, name, 20)
	if updates == nil {
		updates = []store.ImageUpdate{}
	}
	var lastUpdate *store.ImageUpdate
	if len(updates) > 0 {
		u0 := updates[0]
		lastUpdate = &u0
	}
	dto := stackImagesDTO{
		HostID:     hostID,
		HostName:   he.Name,
		ReadOnly:   he.ReadOnly,
		Stack:      name,
		Managed:    st.Managed,
		Status:     st.Status,
		Running:    st.Running,
		Policy:     pol.Policy,
		AutoReason: autoDisabledReason(he.ReadOnly, st.Managed),
		CheckedAt:  checkedAt,
		Services:   rows,
		LastUpdate: lastUpdate,
		Updates:    updates,
	}
	if cr := s.CoolifyMetaOf(ctx, hostID, name); cr != nil {
		dto.DisplayName = cr.Name
		dto.CoolifyProject = cr.Project
		dto.CoolifyUUID = cr.UUID
	}
	writeJSON(w, http.StatusOK, dto)
}

func (s *Server) saveStackImagePolicy(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, false)
	if !ok {
		return
	}
	var req struct {
		Policy string `json:"policy"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	switch req.Policy {
	case store.PolicyOff, store.PolicyNotify, store.PolicyAuto:
	default:
		writeErr(w, http.StatusBadRequest, "Pick a policy: off, notify or auto.")
		return
	}
	ctx := r.Context()
	hostID, name := r.PathValue("host"), r.PathValue("name")
	_, he, _ := s.reg.Get(ctx, hostID)
	st, err := h.GetStack(ctx, name)
	if err != nil {
		s.engErr(w, err)
		return
	}
	if req.Policy == store.PolicyAuto {
		if reason := autoDisabledReason(he.ReadOnly, st.Managed); reason != "" {
			writeErr(w, http.StatusForbidden, reason)
			return
		}
	}
	cur, _ := s.store.StackUpdatePolicy(ctx, hostID, name)
	cur.HostID = hostID
	cur.Stack = name
	cur.Policy = req.Policy
	// If the user explicitly switches back to Auto, clear skipped digests so they can opt back in.
	if req.Policy == store.PolicyAuto {
		cur.SkippedDigests = map[string]string{}
	}
	if err := s.store.SaveStackUpdatePolicy(ctx, cur); err != nil {
		s.engErr(w, err)
		return
	}
	s.log.Info("stack image policy saved", "host", hostID, "stack", name, "policy", req.Policy, "user", currentUser(r).Username)
	writeJSON(w, http.StatusOK, map[string]string{"policy": req.Policy})
}

func (s *Server) checkStackImagesNow(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.hostOf(w, r, false); !ok {
		return
	}
	hostID, name := r.PathValue("host"), r.PathValue("name")
	rows, err := s.upd.CheckNow(r.Context(), hostID, name)
	if errors.Is(err, updater.ErrRateLimited) {
		writeErr(w, http.StatusTooManyRequests, err.Error())
		return
	}
	if err != nil {
		s.engErr(w, err)
		return
	}
	if rows == nil {
		rows = []store.ImageCheckRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": rows})
}

// updateStackImagesNow starts "Update now" and returns its operation, so the UI shows the
// output like for Deploy. The update record and the re-check are written in the background:
// a pull can take minutes, longer than an HTTP request should wait (or the browser stay open).
func (s *Server) updateStackImagesNow(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.hostOf(w, r, true); !ok {
		return
	}
	hostID, name, user := r.PathValue("host"), r.PathValue("name"), currentUser(r).Username
	op, _, err := s.upd.StartUpdate(r.Context(), hostID, name, user)
	if err != nil {
		s.engErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, op)
}

func (s *Server) changeStackImageTag(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, true)
	if !ok {
		return
	}
	var req struct {
		Service string `json:"service"`
		OldRef  string `json:"oldRef"`
		NewTag  string `json:"newTag"`
		NewRef  string `json:"newRef"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Service = strings.TrimSpace(req.Service)
	req.OldRef = strings.TrimSpace(req.OldRef)
	req.NewTag = strings.TrimSpace(req.NewTag)
	req.NewRef = strings.TrimSpace(req.NewRef)
	if req.NewRef == "" && req.OldRef != "" && req.NewTag != "" {
		req.NewRef = replaceRefTag(req.OldRef, req.NewTag)
	}
	if req.Service == "" || req.OldRef == "" || req.NewRef == "" {
		writeErr(w, http.StatusBadRequest, "Specify the service, current image reference and new tag.")
		return
	}

	ctx := r.Context()
	name, user := r.PathValue("name"), currentUser(r).Username
	ver, err := h.ChangeImage(ctx, name, req.Service, req.OldRef, req.NewRef, user)
	if err != nil {
		s.engErr(w, err)
		return
	}
	s.stackEvent(r, name, "edit", engine.FormatImageChangeMessage(req.Service, req.OldRef, req.NewRef))

	op, err := h.RunAction(ctx, name, "update", user)
	resp := map[string]any{"version": ver, "newRef": req.NewRef}
	if err != nil {
		resp["updateError"] = sentence(err)
	} else {
		resp["op"] = op
		s.stackEvent(r, name, "update", req.NewRef)
		s.upd.RecheckAfter(r.PathValue("host"), name, op.ID)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) rollbackStackImages(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.hostOf(w, r, true); !ok {
		return
	}
	var req struct {
		UpdateID int64 `json:"updateId"`
	}
	if r.ContentLength > 0 {
		if !readJSON(w, r, &req) {
			return
		}
	}
	hostID, name, user := r.PathValue("host"), r.PathValue("name"), currentUser(r).Username
	op, err := s.upd.Rollback(r.Context(), hostID, name, req.UpdateID, user)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "Update record not found.")
		return
	}
	if err != nil {
		s.engErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, op)
}

func replaceRefTag(oldRef, newTag string) string {
	if idx := strings.Index(oldRef, "@"); idx >= 0 {
		oldRef = oldRef[:idx]
	}
	lastSlash := strings.LastIndex(oldRef, "/")
	lastColon := strings.LastIndex(oldRef, ":")
	if lastColon > lastSlash {
		return oldRef[:lastColon] + ":" + newTag
	}
	return oldRef + ":" + newTag
}
