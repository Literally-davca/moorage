package api

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/lint"
)

// ---- Home → Services and the port Fix (M19F) ----------------------------------------

type stackServiceLinks struct {
	Stack   string        `json:"stack"`
	Status  string        `json:"status"`
	Managed bool          `json:"managed"`
	Self    bool          `json:"self,omitempty"`
	Links   []serviceLink `json:"links"`
}

// hostServiceLinks: GET /api/hosts/{host}/service-links – every stack's services with
// their link, or why they have none. One container listing; a full stack read only for
// managed stacks with host-network services (their declared ports).
func (s *Server) hostServiceLinks(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, false)
	if !ok {
		return
	}
	ctx := r.Context()
	hostID := r.PathValue("host")
	list, err := h.ListStacks(ctx)
	if err != nil {
		s.engErr(w, err)
		return
	}
	cm, isCM := h.(host.ContainerManager)
	byProj := map[string][]engine.Container{}
	if isCM {
		if ctrs, err := cm.Containers(ctx); err == nil {
			for _, c := range ctrs {
				if c.Project != "" {
					byProj[c.Project] = append(byProj[c.Project], c.Container)
				}
			}
		}
	}
	out := []stackServiceLinks{}
	for _, sm := range list {
		st := engine.Stack{StackSummary: sm, Containers: byProj[sm.Name]}
		hostNet := false
		for _, c := range st.Containers {
			hostNet = hostNet || c.HostNetwork
		}
		if hostNet && sm.Managed {
			if full, err := h.GetStack(ctx, sm.Name); err == nil {
				st.Compose, st.Env = full.Compose, full.Env
			}
		}
		if sm.Self {
			// Moorage's own link is its public URL (General settings); Fix isn't offered
			if pub := strings.TrimRight(s.cfg.PublicURL, "/"); pub != "" && hostID == host.LocalID {
				st.URLs = mergeURLs([]string{pub}, st.URLs, 10)
			}
		}
		links := s.decorateStackLinks(ctx, hostID, h, &st)
		if len(links) == 0 {
			continue
		}
		out = append(out, stackServiceLinks{Stack: sm.Name, Status: sm.Status, Managed: sm.Managed, Self: sm.Self, Links: links})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Stack < out[j].Stack })
	writeJSON(w, http.StatusOK, map[string]any{"stacks": out})
}

// portFixReq: publish a loopback-only port on the VPN address or on every address.
type portFixReq struct {
	Service string `json:"service"`
	Port    string `json:"port"` // the published port as the container reports it
	Mode    string `json:"mode"` // vpn | all
	Confirm bool   `json:"confirm"`
}

// fixStackPorts: POST /api/hosts/{host}/stacks/{name}/ports/fix (admin). Without confirm
// it returns the diff; with confirm it saves a version and runs up.
func (s *Server) fixStackPorts(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, true)
	if !ok {
		return
	}
	var req portFixReq
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	hostID, name := r.PathValue("host"), r.PathValue("name")
	st, err := h.GetStack(ctx, name)
	if err != nil {
		s.engErr(w, err)
		return
	}
	if !st.Managed || st.Self {
		writeErr(w, http.StatusConflict, "Moorage changes ports only of stacks it manages (not its own: use Apply & restart). Use Set link instead.")
		return
	}
	b, ok := engine.ParsePortString(req.Port)
	if !ok || !engine.IsLoopbackIP(b.IP) {
		writeErr(w, http.StatusBadRequest, "Only a port published on 127.0.0.1 can be fixed here.")
		return
	}
	newIP := ""
	switch req.Mode {
	case "all":
	case "vpn":
		newIP = s.vpnAddress(ctx, hostID)
		if newIP == "" {
			writeErr(w, http.StatusBadRequest, "This host has no VPN address. Add it in Hosts → Addresses, or use Set link.")
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "Unknown mode.")
		return
	}
	compose, err := rewritePortLine(st.Compose, b, newIP)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	// the same port check as Deploy (M18H): a port tailscale serve holds is refused
	for _, is := range lint.Lint(ctx, compose, st.Env, lint.Options{Name: name, Running: s.runningPorts(ctx, hostID, h, name)}) {
		if is.Code == "port-taken" {
			msg := is.Message
			if strings.Contains(msg, "a process on the host") && newIP != "" {
				msg += " If tailscale serve already uses this port, keep 127.0.0.1 and use Set link."
			}
			writeErr(w, http.StatusConflict, msg)
			return
		}
	}
	diff := lineDiff(st.Compose, compose)
	if !req.Confirm {
		writeJSON(w, http.StatusOK, map[string]any{"diff": diff, "compose": compose})
		return
	}
	user := currentUser(r).Username
	if err := h.Save(ctx, name, compose, st.Env, user, false); err != nil {
		s.engErr(w, err)
		return
	}
	op, err := h.RunAction(ctx, name, "up", user)
	if err != nil {
		s.engErr(w, err)
		return
	}
	s.stackEvent(r, name, "edit", "Published "+strconv.Itoa(b.Public)+" on "+map[bool]string{true: "all addresses", false: newIP}[newIP == ""])
	setAuditStack(r, name)
	writeJSON(w, http.StatusOK, map[string]any{"diff": diff, "op": op})
}

func (s *Server) vpnAddress(ctx context.Context, hostID string) string {
	_, alts := s.hostLinkAddresses(ctx, hostID)
	for _, a := range alts {
		if a.Kind == "vpn" {
			return a.Address
		}
	}
	return ""
}

// rewritePortLine changes the bind address of one short-syntax port entry, keeping the
// rest of the file (comments, quotes, line endings) as it is.
func rewritePortLine(compose string, b engine.PortBinding, newIP string) (string, error) {
	pat := regexp.MustCompile(`^(\s*-\s*["']?)(127\.0\.0\.1|localhost):` + strconv.Itoa(b.Public) + `:` + strconv.Itoa(b.Private) + `((?:/(?:tcp|udp))?["']?\s*(?:#.*)?)(\r?)$`)
	lines := strings.Split(compose, "\n")
	found := -1
	for i, l := range lines {
		if pat.MatchString(l) {
			if found >= 0 {
				return "", fmt.Errorf("the port %d appears more than once – change it in the editor", b.Public)
			}
			found = i
		}
	}
	if found < 0 {
		if strings.Contains(compose, "${") {
			return "", fmt.Errorf("the port comes from a variable – change it in .env or compose.yaml by hand")
		}
		return "", fmt.Errorf("the port line wasn't found (long syntax?) – change it in the editor")
	}
	repl := "${1}"
	if newIP != "" {
		ip := newIP
		if strings.Contains(ip, ":") {
			ip = "[" + ip + "]"
		}
		repl += ip + ":"
	}
	repl += strconv.Itoa(b.Public) + ":" + strconv.Itoa(b.Private) + "${3}${4}"
	lines[found] = pat.ReplaceAllString(lines[found], repl)
	return strings.Join(lines, "\n"), nil
}
