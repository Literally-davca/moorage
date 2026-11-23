package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"moorage/internal/imagefind"
)

// imageLookup finds compose candidates for an image (New stack → From an image). Admin
// only: the hub makes outbound requests to registries and code hosts on its behalf.
func (s *Server) imageLookup(w http.ResponseWriter, r *http.Request) {
	var req struct{ Image string }
	if !readJSON(w, r, &req) {
		return
	}
	res, err := s.imageFinder().Lookup(r.Context(), req.Image)
	if errors.Is(err, imagefind.ErrBadImage) {
		writeErr(w, http.StatusBadRequest, "Image: "+err.Error()+".")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) imageFinder() *imagefind.Finder {
	s.finderOnce.Do(func() {
		if s.finder == nil {
			s.finder = &imagefind.Finder{Templates: s.templatesUsing}
		}
	})
	return s.finder
}

// templatesUsing lists catalogue templates whose compose file names the repository
// (with or without the docker.io/library prefix).
func (s *Server) templatesUsing(ctx context.Context, repo string) []imagefind.TemplateMatch {
	if s.tpl == nil {
		return nil
	}
	cat, err := s.tpl.List(ctx)
	if err != nil {
		return nil
	}
	short := strings.TrimPrefix(repo, "library/")
	var out []imagefind.TemplateMatch
	for _, e := range cat.Templates {
		if e.Error != "" || len(out) >= 5 {
			continue
		}
		t, _, err := s.tpl.Get(ctx, e.Source, e.ID)
		if err != nil {
			continue
		}
		if usesImage(t.Compose, repo, short) {
			out = append(out, imagefind.TemplateMatch{Source: e.Source, ID: e.ID, Name: e.Name, Description: e.Description, SourceLabel: e.SourceLabel})
		}
	}
	return out
}

// usesImage looks for "image: <repo>" (any tag or registry prefix) in a compose file.
func usesImage(compose, repo, short string) bool {
	for _, line := range strings.Split(compose, "\n") {
		l := strings.TrimSpace(line)
		if !strings.HasPrefix(l, "image:") {
			continue
		}
		img := strings.Trim(strings.TrimSpace(strings.TrimPrefix(l, "image:")), `"'`)
		name, _, _ := strings.Cut(img, "@")
		if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
			name = name[:i]
		}
		name = strings.TrimPrefix(name, "docker.io/")
		if name == repo || name == short || strings.HasSuffix(name, "/"+repo) {
			return true
		}
	}
	return false
}
