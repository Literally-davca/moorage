package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"moorage/internal/engine"
	"moorage/internal/host"
)

// ---- service links (M19E, M19F) ---------------------------------------------------
//
// One place builds the links to a stack's services: the stack page, the Home card
// Services, the assistant's get_service_links and the MCP tools all use it, so the
// assistant never shows a link the stack page wouldn't.

// Link sources and reasons for a missing link.
const (
	linkLabel  = "label"  // moorage.url label
	linkProxy  = "proxy"  // reverse proxy labels (add-on)
	linkPort   = "port"   // published port on the host's address
	linkExpose = "expose" // host network: a declared port the host listens on
	linkManual = "manual" // set in Moorage (Set link)

	reasonLoopback    = "loopback"     // published only on 127.0.0.1
	reasonHostNetwork = "host-network" // network_mode: host, port not detected
	reasonUnpublished = "unpublished"  // no published port
	reasonStopped     = "stopped"
	reasonNoAddress   = "no-address" // the host has no known address
)

// serviceLink is how one service of a stack is reached.
type serviceLink struct {
	Service      string           `json:"service"`
	Container    string           `json:"container,omitempty"`
	URL          string           `json:"url,omitempty"`
	Alternatives []engine.AltURL  `json:"alternatives,omitempty"`
	Source       string           `json:"source,omitempty"`
	Reason       string           `json:"reason,omitempty"`
	Note         string           `json:"note,omitempty"`
	Ports        []string         `json:"ports,omitempty"` // for Fix (loopback-only ports)
	extra        []string         // further URLs of the same service
	alts         []engine.AltURL  // kept for decorate
	src          map[string]string // url -> source
}

// manualLinks are the links an admin set in Moorage: "stack/service" -> URLs.
func (s *Server) manualLinks(ctx context.Context, hostID string) map[string][]string {
	out := map[string][]string{}
	if raw, err := s.store.GetSetting(ctx, "service_links:"+hostID); err == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return out
}

// decorateStackLinks fills the URLs of a stack and its containers like the stack page
// shows them (labels, proxy, published ports on the host's addresses, manual links) and
// returns one record per service with its source, or why it has no link.
func (s *Server) decorateStackLinks(ctx context.Context, hostID string, h host.Host, st *engine.Stack) []serviceLink {
	proxyOn, _ := s.store.AddonEnabled(ctx, addonProxyLabels)
	hostIP, altAddrs := s.hostLinkAddresses(ctx, hostID)
	manual := s.manualLinks(ctx, hostID)
	stackHasExplicit := len(st.URLs) > 0
	if proxyOn && len(st.ProxyURLs) > 0 {
		st.URLs = mergeURLs(st.URLs, st.ProxyURLs, 10)
	}
	st.ProxyURLs = nil

	var declared []engine.DeclaredPort
	var native map[int]bool
	bySvc := map[string]*serviceLink{}
	var order []string
	for i := range st.Containers {
		c := &st.Containers[i]
		svc := c.Service
		if svc == "" {
			svc = c.Name
		}
		sl := bySvc[svc]
		if sl == nil {
			sl = &serviceLink{Service: svc, Container: c.Name, src: map[string]string{}}
			bySvc[svc] = sl
			order = append(order, svc)
		}
		for _, u := range manual[st.Name+"/"+svc] {
			sl.src[u] = linkManual
			c.URLs = mergeURLs([]string{u}, c.URLs, 5)
		}
		ctrExplicit := len(c.URLs) > 0
		for _, u := range c.URLs {
			if sl.src[u] == "" {
				sl.src[u] = linkLabel
			}
		}
		if proxyOn && len(c.ProxyURLs) > 0 {
			for _, u := range c.ProxyURLs {
				if sl.src[u] == "" {
					sl.src[u] = linkProxy
				}
			}
			c.URLs = mergeURLs(c.URLs, c.ProxyURLs, 5)
		}
		if !ctrExplicit && hostIP != "" {
			purls := hostPortURLs(hostIP, c.Ports)
			for _, u := range purls {
				if sl.src[u] == "" {
					sl.src[u] = linkPort
				}
			}
			c.URLs = mergeURLs(c.URLs, purls, 5)
			alts := hostAltURLs(altAddrs, c.Ports)
			c.AltURLs = alts
			sl.alts = append(sl.alts, alts...)
			if !stackHasExplicit {
				st.URLs = mergeURLs(st.URLs, purls, 10)
				st.AltURLs = append(st.AltURLs, alts...)
			}
		}
		// M19F: network_mode: host has no mappings; a declared port the host really
		// listens on becomes the link.
		if len(c.URLs) == 0 && c.HostNetwork && hostIP != "" {
			if declared == nil {
				declared = engine.DeclaredPorts(ctx, st.Name, st.Compose, st.Env)
				native = s.nativeListening(ctx, hostID, h)
			}
			for _, d := range declared {
				p := d.Port
				if p == 0 {
					p = d.Target
				}
				if d.Service == svc && native[p] && len(c.URLs) < 5 {
					u := portLink(hostIP, p)
					sl.src[u] = linkExpose
					c.URLs = append(c.URLs, u)
				}
			}
		}
		c.ProxyURLs = nil
		for _, u := range c.URLs {
			if sl.URL == "" {
				sl.URL = u
				sl.Source = sl.src[u]
			} else if u != sl.URL {
				sl.extra = append(sl.extra, u)
			}
		}
		if sl.URL == "" && sl.Reason == "" {
			switch {
			case c.State != "running":
				sl.Reason, sl.Note = reasonStopped, "the stack is stopped"
			case c.HostNetwork:
				sl.Reason, sl.Note = reasonHostNetwork, "host network – port not detected"
			case len(c.Ports) > 0 && allLoopback(c.Ports):
				sl.Reason, sl.Note, sl.Ports = reasonLoopback, "only on 127.0.0.1 – reachable only from the host itself", c.Ports
			case len(c.Ports) > 0 && hostIP == "":
				sl.Reason, sl.Note = reasonNoAddress, "no address known for this host – set it in Hosts → Addresses"
			default:
				sl.Reason, sl.Note = reasonUnpublished, "no published port – reachable only inside the Docker network"
			}
		} else if sl.URL != "" {
			sl.Reason, sl.Note, sl.Ports = "", "", nil
		}
	}
	out := make([]serviceLink, 0, len(order))
	for _, svc := range order {
		sl := bySvc[svc]
		sl.Alternatives = sl.alts
		out = append(out, *sl)
	}
	return out
}

func allLoopback(ports []string) bool {
	for _, p := range ports {
		b, ok := engine.ParsePortString(p)
		if !ok || !engine.IsLoopbackIP(b.IP) {
			return false
		}
	}
	return true
}

func portLink(ip string, port int) string {
	scheme := "http"
	if port == 443 || port == 8443 {
		scheme = "https"
	}
	hp := ip
	if strings.Contains(ip, ":") {
		hp = "[" + ip + "]"
	}
	return scheme + "://" + hp + ":" + strconv.Itoa(port)
}

// nativeListening are the TCP ports the host itself listens on, when Moorage can see
// them (native or host network mode).
func (s *Server) nativeListening(ctx context.Context, hostID string, h host.Host) map[int]bool {
	out := map[int]bool{}
	rep, err := s.hostPorts(ctx, hostID, h, false)
	if err != nil || !rep.Native {
		return out
	}
	for _, p := range rep.Ports {
		if p.Source == engine.PortSourceHost && p.Proto == "tcp" && !engine.IsLoopbackIP(p.IP) {
			out[p.Port] = true
		}
	}
	return out
}

// putStackLinks sets the manual links of one service (M19F Set link; admin).
func (s *Server) putStackLinks(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.hostOf(w, r, false); !ok {
		return
	}
	var req struct {
		Service string   `json:"service"`
		URLs    []string `json:"urls"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Service = strings.TrimSpace(req.Service)
	if req.Service == "" || len(req.Service) > 128 || len(req.URLs) > 5 {
		writeErr(w, http.StatusBadRequest, "Give a service and at most 5 links.")
		return
	}
	var urls []string
	for _, u := range req.URLs {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		pu, err := url.Parse(u)
		if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" || len(u) > 500 {
			writeErr(w, http.StatusBadRequest, "Links must be http(s) URLs.")
			return
		}
		urls = append(urls, u)
	}
	ctx := r.Context()
	hostID, stack := r.PathValue("host"), r.PathValue("name")
	m := s.manualLinks(ctx, hostID)
	key := stack + "/" + req.Service
	if len(urls) == 0 {
		delete(m, key)
	} else {
		m[key] = urls
	}
	b, _ := json.Marshal(m)
	if err := s.store.SetSetting(ctx, "service_links:"+hostID, string(b)); err != nil {
		s.engErr(w, err)
		return
	}
	setAuditStack(r, stack)
	writeJSON(w, http.StatusOK, map[string]any{"urls": urls})
}

// stackLinksText renders service links for the assistant and MCP.
func stackLinksText(stack, hostName string, links []serviceLink) string {
	var b strings.Builder
	b.WriteString("Stack " + stack + " on host " + hostName + ":\n")
	sort.SliceStable(links, func(i, j int) bool { return links[i].URL != "" && links[j].URL == "" })
	for _, l := range links {
		if l.URL == "" {
			b.WriteString("- " + l.Service + ": no link (" + l.Note + ")\n")
			continue
		}
		b.WriteString("- " + l.Service + ": " + l.URL + " (" + l.Source + ")")
		for _, a := range l.Alternatives {
			b.WriteString("; " + a.Label + " " + a.URL)
		}
		for _, u := range l.extra {
			b.WriteString("; " + u)
		}
		b.WriteString("\n")
	}
	return b.String()
}
