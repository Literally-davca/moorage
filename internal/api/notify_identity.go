package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"moorage/internal/host"
	"moorage/internal/netguard"
	"moorage/internal/notify"
)

// ---- rich notifications (M17B): sender identity, logo URL and previews -----------------

// settingNotifyIconBase overrides the base URL of the Moorage logos in notifications.
const settingNotifyIconBase = "notify_icon_base"

// initNotifyIdentity names the sender of every notification after the affected host and
// its role ("nas • Moorage agent", "hub • Moorage hub") and loads the logo base URL.
func (s *Server) initNotifyIdentity() {
	notify.SetSenderResolver(s.notifySender)
	if s.store != nil {
		if v, err := s.store.GetSetting(context.Background(), settingNotifyIconBase); err == nil {
			notify.SetIconBase(v)
		}
	}
}

func (s *Server) notifySender(hostID string) notify.Sender {
	ctx := context.Background()
	if hostID != "" && hostID != host.LocalID && s.reg != nil {
		if _, e, _ := s.reg.Get(ctx, hostID); e.Name != "" {
			return notify.Sender{Host: e.Name, Role: notify.RoleAgent}
		}
		return notify.Sender{}
	}
	name := ""
	if s.reg != nil {
		if _, e, err := s.reg.Get(ctx, host.LocalID); err == nil {
			name = e.Name
		}
	}
	if name == "" && s.hub != nil {
		name = s.hub.HubName
	}
	if name == "" {
		name, _ = os.Hostname()
	}
	return notify.Sender{Host: name, Role: notify.RoleHub}
}

type notifySettingsDTO struct {
	IconBase        string   `json:"iconBase"`
	DefaultIconBase string   `json:"defaultIconBase"`
	RichKinds       []string `json:"richKinds"`
	PartKeys        []string `json:"partKeys"`
	MaxLogLines     int      `json:"maxLogLines"`
}

func (s *Server) notifySettings(ctx context.Context) notifySettingsDTO {
	v, _ := s.store.GetSetting(ctx, settingNotifyIconBase)
	var rich []string
	for k := range notify.Kinds {
		if notify.SupportsRich(k) {
			rich = append(rich, k)
		}
	}
	return notifySettingsDTO{IconBase: v, DefaultIconBase: notify.DefaultIconBase, RichKinds: sortedStrings(rich), PartKeys: notify.PartKeys, MaxLogLines: notify.MaxLogLines}
}

func (s *Server) getNotifySettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.notifySettings(r.Context()))
}

func (s *Server) putNotifySettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IconBase string `json:"iconBase"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	v := strings.TrimRight(strings.TrimSpace(req.IconBase), "/")
	if v != "" {
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
			writeErr(w, http.StatusBadRequest, "The logo base URL must be an http:// or https:// address without a query, e.g. https://cdn.example.com/moorage.")
			return
		}
	}
	if err := s.store.SetSetting(r.Context(), settingNotifyIconBase, v); err != nil {
		s.engErr(w, err)
		return
	}
	notify.SetIconBase(v)
	if s.hub != nil {
		go s.hub.PushWatchdog(context.Background()) // agents' watchdog uses the same logos
	}
	writeJSON(w, http.StatusOK, s.notifySettings(r.Context()))
}

// previewNotification renders the sample message for a channel as the UI is editing it:
// the shared view (what each part shows) and the request body. Nothing is sent and no
// secret is returned (only the body, never the URL or headers).
func (s *Server) previewNotification(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind   string        `json:"kind"`
		Config notify.Config `json:"config"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !notify.Kinds[req.Kind] {
		writeErr(w, http.StatusBadRequest, "Unknown channel type.")
		return
	}
	m := notify.SampleMessage(s.effectivePublicURL(r.Context()))
	v := notify.BuildView(req.Config, m)
	out := map[string]any{"view": v, "rich": notify.SupportsRich(req.Kind) && req.Config.Format != notify.FormatPlain}
	if req.Kind == "smtp" && strings.TrimSpace(req.Config.From) == "" {
		req.Config.From = "alerts@example.com"
	}
	if req.Kind == "smtp" && strings.TrimSpace(req.Config.To) == "" {
		req.Config.To = "you@example.com"
	}
	if rq, err := notify.Render(req.Kind, req.Config, m); err == nil {
		body := string(rq.Body)
		if strings.HasPrefix(rq.ContentType, "application/json") {
			var anyv any
			if json.Unmarshal(rq.Body, &anyv) == nil {
				if b, err := json.MarshalIndent(anyv, "", "  "); err == nil {
					body = string(b)
				}
			}
		}
		if req.Kind == "pushover" {
			if vals, err := url.ParseQuery(body); err == nil {
				vals.Del("token")
				vals.Del("user")
				body = vals.Encode()
			}
		}
		out["payload"] = body
	}
	writeJSON(w, http.StatusOK, out)
}

// notifyLogo serves one logo for the preview in Settings → Notifications. The page's
// CSP allows only its own images, so Moorage fetches <base>/<role>.png itself: the
// preview then works without loosening the CSP and shows whether the URL is reachable.
// base is the value being edited (or the saved/default one); only http(s), PNG, ≤ 1 MiB.
func (s *Server) notifyLogo(w http.ResponseWriter, r *http.Request) {
	role := r.URL.Query().Get("role")
	if role != notify.RoleHub && role != notify.RoleAgent && role != notify.RoleWatchdog {
		writeErr(w, http.StatusBadRequest, "Unknown role.")
		return
	}
	base := strings.TrimRight(strings.TrimSpace(r.URL.Query().Get("base")), "/")
	if base == "" {
		base = notify.IconBase()
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		writeErr(w, http.StatusBadRequest, "The logo base URL must be an http:// or https:// address without a query.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/"+role+".png", nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid logo URL.")
		return
	}
	c := &http.Client{
		Timeout:   8 * time.Second,
		Transport: &http.Transport{DialContext: netguard.DialContext(5*time.Second, false), ForceAttemptHTTP2: true},
	}
	res, err := c.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "Logo not reachable from Moorage.")
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		writeErr(w, http.StatusBadGateway, "Logo URL answered "+res.Status+".")
		return
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20+1))
	if err != nil || len(b) > 1<<20 || http.DetectContentType(b) != "image/png" {
		writeErr(w, http.StatusBadGateway, "The logo isn't a PNG up to 1 MiB.")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(b)
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
