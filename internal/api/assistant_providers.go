package api

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"moorage/internal/assistant"
	"moorage/internal/netguard"
)

// ---- assistant providers and models (M19E) ------------------------------------------
//
// Several providers (two Ollamas, a paid API…), each with its own key. Ollama and
// OpenAI-compatible endpoints offer every model they have; paid APIs only the models an
// admin ticked (they differ in price). The old single provider/model/baseUrl fields stay
// written for the default provider, so a downgrade keeps working.

type assistantProvider struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	BaseURL string `json:"baseUrl,omitempty"`
	// Models: ticked models of a paid API (Anthropic, OpenAI, Gemini). Ignored for
	// Ollama and OpenAI-compatible endpoints, which offer every model they report.
	Models []string `json:"models,omitempty"`
}

type assistantDefault struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

var providerIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func paidProvider(t string) bool {
	return t == assistant.ProviderAnthropic || t == assistant.ProviderOpenAI || t == assistant.ProviderGemini
}

func providerLabel(t string) string {
	switch t {
	case assistant.ProviderAnthropic:
		return "Anthropic"
	case assistant.ProviderOpenAI:
		return "OpenAI"
	case assistant.ProviderGemini:
		return "Gemini"
	case assistant.ProviderOllama:
		return "Ollama"
	}
	return "OpenAI-compatible"
}

// normalizeProviders migrates the single-provider settings and fixes the default.
func normalizeProviders(a *assistantSettings) {
	if len(a.Providers) == 0 {
		a.Providers = []assistantProvider{{ID: "default", Name: providerLabel(a.Provider), Type: a.Provider, BaseURL: a.BaseURL}}
		if a.Model != "" && paidProvider(a.Provider) {
			a.Providers[0].Models = []string{a.Model}
		}
		a.Default = assistantDefault{Provider: "default", Model: a.Model}
	}
	if a.providerByID(a.Default.Provider) == nil {
		a.Default.Provider = a.Providers[0].ID
	}
	if a.Default.Model == "" {
		a.Default.Model = assistant.DefaultModel[a.providerByID(a.Default.Provider).Type]
	}
	// legacy fields mirror the default (downgrade, status checks)
	p := a.providerByID(a.Default.Provider)
	a.Provider, a.BaseURL, a.Model = p.Type, p.BaseURL, a.Default.Model
}

func (a *assistantSettings) providerByID(id string) *assistantProvider {
	for i := range a.Providers {
		if a.Providers[i].ID == id {
			return &a.Providers[i]
		}
	}
	return nil
}

// providerKey reads the API key of a provider (the default one also has the old key).
func (s *Server) providerKey(ctx context.Context, id string) string {
	if k, _ := s.store.GetSetting(ctx, settingAssistantKey+":"+id); k != "" {
		return k
	}
	if id == "default" {
		k, _ := s.store.GetSetting(ctx, settingAssistantKey)
		return k
	}
	return ""
}

func assistantHTTP() *http.Client {
	return &http.Client{Timeout: 5 * time.Minute, Transport: &http.Transport{DialContext: netguard.DialContext(15*time.Second, true), ForceAttemptHTTP2: true}}
}

func (s *Server) providerConfig(ctx context.Context, p *assistantProvider, model string) assistant.Config {
	return assistant.Config{Provider: p.Type, Model: model, BaseURL: p.BaseURL, APIKey: s.providerKey(ctx, p.ID), HTTP: assistantHTTP()}
}

// modelCache keeps detected models for 5 minutes per provider.
type modelCacheEntry struct {
	at     time.Time
	models []assistant.ModelInfo
	err    string
}

var (
	modelCacheMu sync.Mutex
	modelCache   = map[string]modelCacheEntry{}
)

func (s *Server) detectModels(ctx context.Context, p *assistantProvider, fresh bool) modelCacheEntry {
	key := p.ID + "|" + p.Type + "|" + p.BaseURL
	modelCacheMu.Lock()
	e, ok := modelCache[key]
	modelCacheMu.Unlock()
	if ok && !fresh && time.Since(e.at) < 5*time.Minute {
		return e
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ms, err := assistant.ListModels(cctx, s.providerConfig(ctx, p, ""))
	ne := modelCacheEntry{at: time.Now(), models: ms}
	if err != nil {
		ne.err = err.Error()
		if ok {
			ne.models, ne.at = e.models, e.at // keep the last known list
		}
	}
	sort.Slice(ne.models, func(i, j int) bool { return ne.models[i].ID < ne.models[j].ID })
	modelCacheMu.Lock()
	modelCache[key] = ne
	modelCacheMu.Unlock()
	return ne
}

// allowedModels are the models the panel offers for a provider.
func (s *Server) allowedModels(ctx context.Context, a assistantSettings, p *assistantProvider) []string {
	seen := map[string]bool{}
	var out []string
	add := func(m string) {
		if m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	if a.Default.Provider == p.ID {
		add(a.Default.Model)
	}
	if paidProvider(p.Type) {
		for _, m := range p.Models {
			add(m)
		}
		return out
	}
	for _, m := range s.detectModels(ctx, p, false).models {
		add(m.ID)
	}
	return out
}

// pickModel resolves the provider and model of a chat request: an admin may choose any
// allowed pair, everyone else gets the default. The note explains a fallback.
func (s *Server) pickModel(ctx context.Context, a assistantSettings, admin bool, provider, model string) (*assistantProvider, string, string) {
	def := a.providerByID(a.Default.Provider)
	if !admin || (provider == "" && model == "") {
		return def, a.Default.Model, ""
	}
	p := a.providerByID(provider)
	if p == nil {
		return def, a.Default.Model, "Provider " + provider + " was removed – using " + def.Name + " · " + a.Default.Model + "."
	}
	if model == "" {
		model = a.Default.Model
	}
	for _, m := range s.allowedModels(ctx, a, p) {
		if m == model {
			return p, model, ""
		}
	}
	return def, a.Default.Model, "Model " + model + " is no longer allowed – using " + a.Default.Model + "."
}

// assistantModels: GET /api/addons/assistant/models?provider=<id>&fresh=1 (admin).
func (s *Server) assistantModels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a := s.loadAssistantSettings(ctx)
	p := a.providerByID(r.URL.Query().Get("provider"))
	if p == nil {
		writeErr(w, http.StatusNotFound, "Unknown provider.")
		return
	}
	e := s.detectModels(ctx, p, r.URL.Query().Get("fresh") == "1")
	models := e.models
	if models == nil {
		models = []assistant.ModelInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models, "error": e.err, "at": e.at, "paid": paidProvider(p.Type)})
}

// assistantTest: POST /api/addons/assistant/test {provider, model} (admin).
func (s *Server) assistantTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	a := s.loadAssistantSettings(ctx)
	p := a.providerByID(req.Provider)
	if p == nil {
		p = a.providerByID(a.Default.Provider)
	}
	model := req.Model
	if model == "" {
		model = a.Default.Model
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := assistant.Test(cctx, s.providerConfig(ctx, p, model)); err != nil {
		code, detail := assistant.ErrorCode(err)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": capitalize(err.Error()) + ".", "code": code, "detail": detail})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// validateProviders checks the provider list of a settings save.
func validateProviders(in *assistantSettings) string {
	if len(in.Providers) == 0 || len(in.Providers) > 10 {
		return "Add between 1 and 10 providers."
	}
	seen := map[string]bool{}
	for i := range in.Providers {
		p := &in.Providers[i]
		p.ID = strings.TrimSpace(p.ID)
		p.Name = strings.TrimSpace(p.Name)
		p.BaseURL = strings.TrimSpace(p.BaseURL)
		if !providerIDRe.MatchString(p.ID) || seen[p.ID] {
			return "Each provider needs a unique id (lowercase letters, digits and -)."
		}
		seen[p.ID] = true
		switch p.Type {
		case assistant.ProviderAnthropic, assistant.ProviderOpenAI, assistant.ProviderGemini, assistant.ProviderOllama, assistant.ProviderCompat:
		default:
			return "Unknown provider type."
		}
		if p.Name == "" {
			p.Name = providerLabel(p.Type)
		}
		if len(p.Name) > 60 {
			return "Provider name is too long."
		}
		if p.BaseURL != "" {
			u, err := url.Parse(p.BaseURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return "The endpoint of " + p.Name + " must be an http(s) URL."
			}
		}
		if p.Type == assistant.ProviderCompat && p.BaseURL == "" {
			return "Set the endpoint URL of " + p.Name + "."
		}
		if len(p.Models) > 50 {
			return "Tick at most 50 models per provider."
		}
		var ms []string
		for _, m := range p.Models {
			if m = strings.TrimSpace(m); m != "" && len(m) <= 120 {
				ms = append(ms, m)
			}
		}
		p.Models = ms
	}
	in.Default.Model = strings.TrimSpace(in.Default.Model)
	if in.providerByID(in.Default.Provider) == nil {
		return "Choose the default provider."
	}
	if len(in.Default.Model) > 120 {
		return "Model name is too long."
	}
	return ""
}
