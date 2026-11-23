package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"moorage/internal/status"
	"moorage/internal/store"
)

type statusPageItemDTO struct {
	Section     string `json:"section"`
	DisplayName string `json:"displayName"`
	CheckID     int64  `json:"checkId,omitempty"`
	HostID      string `json:"hostId,omitempty"`
	Stack       string `json:"stack,omitempty"`
}

type statusPageInput struct {
	Slug          string              `json:"slug"`
	Title         string              `json:"title"`
	Description   string              `json:"description"`
	Domain        string              `json:"domain"`
	Published     bool                `json:"published"`
	ShowIncidents bool                `json:"showIncidents"`
	Items         []statusPageItemDTO `json:"items"`
}

type statusPageDTO struct {
	statusPageInput
	ID           int64  `json:"id"`
	Status       string `json:"status"`
	StatusText   string `json:"statusText"`
	PublicURL    string `json:"publicUrl"`
	StatusListen string `json:"statusListen,omitempty"`
}

func (s *Server) toStatusPageDTO(ctx context.Context, p store.StatusPage) statusPageDTO {
	items := make([]statusPageItemDTO, 0, len(p.Items))
	for _, it := range p.Items {
		items = append(items, statusPageItemDTO{
			Section:     it.Section,
			DisplayName: it.DisplayName,
			CheckID:     it.CheckID,
			HostID:      it.HostID,
			Stack:       it.Stack,
		})
	}
	pub, err := s.status.Preview(ctx, p.ID)
	bannerStatus, bannerText := status.StateNoData, "No services configured yet"
	if err == nil {
		bannerStatus, bannerText = pub.BannerStatus, pub.BannerText
	}
	pubURL := "/status/" + p.Slug
	if p.Domain != "" {
		pubURL = "https://" + p.Domain + "/"
	} else if base := strings.TrimRight(s.cfg.PublicURL, "/"); base != "" {
		pubURL = base + "/status/" + p.Slug
	}
	return statusPageDTO{
		statusPageInput: statusPageInput{
			Slug:          p.Slug,
			Title:         p.Title,
			Description:   p.Description,
			Domain:        p.Domain,
			Published:     p.Published,
			ShowIncidents: p.ShowIncidents,
			Items:         items,
		},
		ID:           p.ID,
		Status:       bannerStatus,
		StatusText:   bannerText,
		PublicURL:    pubURL,
		StatusListen: s.cfg.StatusListen,
	}
}

func (s *Server) listStatusPages(w http.ResponseWriter, r *http.Request) {
	pages, err := s.store.StatusPages(r.Context())
	if err != nil {
		s.engErr(w, err)
		return
	}
	out := make([]statusPageDTO, 0, len(pages))
	for _, p := range pages {
		out = append(out, s.toStatusPageDTO(r.Context(), p))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getStatusPage(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.StatusPageByID(r.Context(), checkID(r))
	if err != nil {
		writeErr(w, http.StatusNotFound, "Status page not found.")
		return
	}
	writeJSON(w, http.StatusOK, s.toStatusPageDTO(r.Context(), p))
}

func (s *Server) previewStatusPage(w http.ResponseWriter, r *http.Request) {
	pub, err := s.status.Preview(r.Context(), checkID(r))
	if err != nil {
		writeErr(w, http.StatusNotFound, "Status page not found.")
		return
	}
	writeJSON(w, http.StatusOK, pub)
}

// previewDraft shows the public view of the editor's unsaved settings. Preview used to
// save first, which published half-finished edits of a page that was already public.
func (s *Server) previewDraft(w http.ResponseWriter, r *http.Request) {
	var in struct {
		statusPageInput
		ID int64 `json:"id"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if err := s.validateStatusPage(r.Context(), in.ID, &in.statusPageInput); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	pub, err := s.status.PreviewDraft(r.Context(), toStoreStatusPage(in.ID, in.statusPageInput))
	if err != nil {
		s.engErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pub)
}

func toStoreStatusPage(id int64, in statusPageInput) store.StatusPage {
	items := make([]store.StatusPageItem, 0, len(in.Items))
	for i, it := range in.Items {
		items = append(items, store.StatusPageItem{
			Position:    i,
			Section:     it.Section,
			DisplayName: it.DisplayName,
			CheckID:     it.CheckID,
			HostID:      it.HostID,
			Stack:       it.Stack,
		})
	}
	return store.StatusPage{
		ID: id, Slug: in.Slug, Title: in.Title, Description: in.Description, Domain: in.Domain,
		Published: in.Published, ShowIncidents: in.ShowIncidents, Items: items,
	}
}

func (s *Server) validateStatusPage(ctx context.Context, id int64, in *statusPageInput) error {
	in.Title = strings.TrimSpace(in.Title)
	in.Slug = strings.TrimSpace(in.Slug)
	in.Description = strings.TrimSpace(in.Description)
	in.Domain = strings.ToLower(strings.TrimSpace(in.Domain))

	switch {
	case in.Title == "" || len([]rune(in.Title)) > 100:
		return errors.New("Give the status page a title of 1–100 characters.")
	case !status.ValidSlug(in.Slug):
		return errors.New("The URL slug must be 1–63 lowercase letters, digits or hyphens, starting with a letter or digit.")
	case len([]rune(in.Description)) > 500:
		return errors.New("The description can be at most 500 characters.")
	case len(in.Items) > 100:
		return errors.New("A status page can have at most 100 items.")
	}
	if err := status.ValidateDomain(in.Domain, s.cfg.PublicURL); err != nil {
		return err
	}
	if existing, err := s.store.StatusPageBySlug(ctx, in.Slug); err == nil && existing.ID != id {
		return errors.New("Another status page already uses that URL slug.")
	}
	if in.Domain != "" {
		if existing, err := s.store.StatusPageByDomain(ctx, in.Domain); err == nil && existing.ID != id {
			return errors.New("Another status page already uses that domain.")
		}
	}

	checks, _ := s.store.Checks(ctx)
	validChecks := map[int64]bool{}
	for _, c := range checks {
		validChecks[c.ID] = true
	}

	for i := range in.Items {
		it := &in.Items[i]
		it.DisplayName = strings.TrimSpace(it.DisplayName)
		it.Section = strings.TrimSpace(it.Section)
		it.HostID = strings.TrimSpace(it.HostID)
		it.Stack = strings.TrimSpace(it.Stack)
		if it.DisplayName == "" || len([]rune(it.DisplayName)) > 100 {
			return errors.New("Every item needs a public display name of 1–100 characters.")
		}
		if len([]rune(it.Section)) > 100 {
			return errors.New("Section headings can be at most 100 characters.")
		}
		hasCheck := it.CheckID > 0
		hasStack := it.HostID != "" || it.Stack != ""
		if hasCheck == hasStack {
			return errors.New("Each item must point to either one check or one stack.")
		}
		if hasCheck {
			if !validChecks[it.CheckID] {
				return errors.New("One of the selected checks no longer exists.")
			}
			it.HostID, it.Stack = "", ""
		} else {
			if it.HostID == "" || !stackNameRe.MatchString(it.Stack) {
				return errors.New("Each stack item needs a valid host and stack.")
			}
			it.CheckID = 0
		}
	}
	return nil
}

func (s *Server) saveStatusPage(w http.ResponseWriter, r *http.Request) {
	var in statusPageInput
	if !readJSON(w, r, &in) {
		return
	}
	id := int64(0)
	if r.Method == http.MethodPut {
		id = checkID(r)
	}
	if err := s.validateStatusPage(r.Context(), id, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	newID, err := s.store.SaveStatusPage(r.Context(), toStoreStatusPage(id, in))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "Status page not found.")
		return
	}
	if err != nil {
		s.engErr(w, err)
		return
	}
	s.status.Invalidate()
	s.log.Info("status page saved", "page", newID, "slug", in.Slug, "published", in.Published, "user", currentUser(r).Username)
	writeJSON(w, http.StatusOK, map[string]int64{"id": newID})
}

func (s *Server) deleteStatusPage(w http.ResponseWriter, r *http.Request) {
	id := checkID(r)
	if err := s.store.DeleteStatusPage(r.Context(), id); err != nil {
		s.engErr(w, err)
		return
	}
	s.status.Invalidate()
	s.log.Info("status page deleted", "page", id, "user", currentUser(r).Username)
	w.WriteHeader(http.StatusNoContent)
}
