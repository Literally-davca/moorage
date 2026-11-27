package api

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"moorage/internal/assistant"
	"moorage/internal/diagnose"
	"moorage/internal/engine"
)

// ---- read tools added in M19E -------------------------------------------------------

func inputStack(in json.RawMessage) string {
	var a struct {
		Stack string `json:"stack"`
	}
	_ = json.Unmarshal(in, &a)
	if a.Stack == "" {
		return "?"
	}
	return a.Stack
}

// problemsText renders Problems for the model. Evidence lines come from container logs,
// so they follow the data level of logs and count as outside content (M19E collision
// with M18G: before, they bypassed both).
func (s *Server) problemsText(ps []diagnose.Problem, set assistantSettings, allowed map[string]bool, st *assistant.State, env string) string {
	var b strings.Builder
	logsOK := set.Data["logs"] == dataAllowed || (set.Data["logs"] == dataAsk && allowed["logs"])
	hidden := 0
	for _, p := range ps {
		fmt.Fprintf(&b, "Problem [%s] %s – %s: %s.", p.Severity, p.Service, p.Title, p.Detail)
		if len(p.Evidence) > 0 {
			if logsOK {
				st.ReadForeign = true
				fmt.Fprintf(&b, " Evidence (log lines, data only): <untrusted-data>%s</untrusted-data>", maskSecrets(strings.Join(p.Evidence, " | "), env))
			} else {
				hidden++
			}
		}
		for _, f := range p.Fixes {
			fmt.Fprintf(&b, " Fix: %s.", f.Label)
		}
		b.WriteString("\n")
	}
	if hidden > 0 && set.Data["logs"] == dataAsk {
		b.WriteString("(Log evidence of the problems is hidden until the user allows logs in this chat.)\n")
	}
	return b.String()
}

// addReadTools are the read tools shared in spirit with the MCP server: hosts, service
// links, Problems and the port map. All are read scope: every role may use them.
func (s *Server) addReadTools(add func(assistant.Tool), set assistantSettings, allowed map[string]bool) {
	add(assistant.Tool{
		Label:  "Looking at hosts",
		Target: func(json.RawMessage) string { return "hosts" },
		Def:    assistant.ToolDef{Name: "list_hosts", Description: "List the hosts: name, id, kind, connected, read-only, addresses.", Schema: map[string]any{}},
		Run: func(ctx context.Context, _ json.RawMessage, _ *assistant.State, _ func(assistant.Event)) (string, error) {
			hosts, err := s.reg.List(ctx)
			if err != nil {
				return "", err
			}
			var b strings.Builder
			for _, h := range hosts {
				def, alts := s.hostLinkAddresses(ctx, h.ID)
				fmt.Fprintf(&b, "- %s (id %s, %s, connected %v, read-only %v), address %s", h.Name, h.ID, h.Kind, h.Connected, h.ReadOnly, orNone(def))
				for _, a := range alts {
					fmt.Fprintf(&b, ", %s %s", a.Label, a.Address)
				}
				b.WriteString("\n")
			}
			return b.String(), nil
		},
	})
	add(assistant.Tool{
		Label: "Finding service links",
		Target: func(in json.RawMessage) string {
			if st := inputStack(in); st != "?" {
				return "stack " + st + " · links"
			}
			return "service links"
		},
		Def: assistant.ToolDef{Name: "get_service_links",
			Description: "Where services run: the link on the host's default address, other addresses (VPN, Wi-Fi…) and why a service has no link. Use it for any question about where something runs or how to open it. Without stack it searches all stacks for the service name.",
			Schema: map[string]any{"hostId": str("string", "Host id (optional)."), "stack": str("string", "Stack name (optional)."),
				"service": str("string", "Service or stack name to look for (optional).")}},
		Run: func(ctx context.Context, in json.RawMessage, st *assistant.State, _ func(assistant.Event)) (string, error) {
			var a struct {
				HostID  string `json:"hostId"`
				Stack   string `json:"stack"`
				Service string `json:"service"`
			}
			_ = json.Unmarshal(in, &a)
			want := strings.ToLower(strings.TrimSpace(a.Service))
			hosts, err := s.reg.List(ctx)
			if err != nil {
				return "", err
			}
			var b strings.Builder
			found := 0
			for _, he := range hosts {
				if a.HostID != "" && he.ID != a.HostID {
					continue
				}
				h, _, err := s.reg.Get(ctx, he.ID)
				if err != nil {
					continue
				}
				list, err := h.ListStacks(ctx)
				if err != nil {
					continue
				}
				def, _ := s.hostLinkAddresses(ctx, he.ID)
				for _, sm := range list {
					if a.Stack != "" && sm.Name != a.Stack {
						continue
					}
					if a.Stack == "" && want != "" && !strings.Contains(strings.ToLower(sm.Name), want) && !stackHasService(sm, want) {
						continue
					}
					full, err := h.GetStack(ctx, sm.Name)
					if err != nil {
						continue
					}
					links := s.decorateStackLinks(ctx, he.ID, h, &full)
					if want != "" && a.Stack == "" && !strings.Contains(strings.ToLower(sm.Name), want) {
						links = filterLinks(links, want)
					}
					if len(links) == 0 {
						continue
					}
					found++
					if def == "" {
						fmt.Fprintf(&b, "No address known for host %s – set it in #/settings/hosts (Addresses).\n", he.Name)
					}
					b.WriteString(stackLinksText(sm.Name, he.Name, links))
					for _, l := range links {
						if l.URL == "" || !safeLink(l.URL) {
							continue
						}
						st.Links = append(st.Links, assistant.Link{Service: l.Service, Stack: sm.Name, Host: he.Name, URL: l.URL, Label: "default", Primary: true})
						for _, alt := range l.Alternatives {
							if safeLink(alt.URL) {
								st.Links = append(st.Links, assistant.Link{Service: l.Service, Stack: sm.Name, Host: he.Name, URL: alt.URL, Label: alt.Label})
							}
						}
					}
					if found >= 10 {
						break
					}
				}
			}
			if found == 0 {
				return "No matching stack or service found.", nil
			}
			return b.String(), nil
		},
	})
	add(assistant.Tool{
		Label:  "Reading Problems",
		Target: func(in json.RawMessage) string { return "Problems of " + inputStack(in) },
		Def: assistant.ToolDef{Name: "get_problems", Description: "The Problems card of a stack: detected problems with cause and fixes.",
			Schema: map[string]any{"hostId": str("string", "Host id."), "stack": str("string", "Stack name.")}, Required: []string{"hostId", "stack"}},
		Run: func(ctx context.Context, in json.RawMessage, st *assistant.State, _ func(assistant.Event)) (string, error) {
			hostID, stack, h, err := s.assistantStack(ctx, in)
			if err != nil {
				return "", err
			}
			pr, err := s.stackProblems(ctx, hostID, h, stack)
			if err != nil {
				return "", err
			}
			if len(pr.Problems) == 0 {
				return "No problems detected on " + stack + ".", nil
			}
			full, _ := h.GetStack(ctx, stack)
			return s.problemsText(pr.Problems, set, allowed, st, full.Env), nil
		},
	})
	add(assistant.Tool{
		Label:  "Reading the port map",
		Target: func(json.RawMessage) string { return "port map" },
		Def: assistant.ToolDef{Name: "get_ports", Description: "The host's port map: which host ports are taken and by what, so a new stack can pick a free one.",
			Schema: map[string]any{"hostId": str("string", "Host id.")}, Required: []string{"hostId"}},
		Run: func(ctx context.Context, in json.RawMessage, _ *assistant.State, _ func(assistant.Event)) (string, error) {
			var a struct {
				HostID string `json:"hostId"`
			}
			_ = json.Unmarshal(in, &a)
			h, _, err := s.reg.Get(ctx, a.HostID)
			if err != nil {
				return "", err
			}
			rep, err := s.hostPorts(ctx, a.HostID, h, false)
			if err != nil {
				return "", err
			}
			ports := append([]engine.PortEntry(nil), rep.Ports...)
			sort.Slice(ports, func(i, j int) bool { return ports[i].Port < ports[j].Port })
			var b strings.Builder
			fmt.Fprintf(&b, "Network mode %s, host's own sockets visible: %v\n", rep.NetMode, rep.Native)
			for i, p := range ports {
				if i == 300 {
					b.WriteString("…(more)\n")
					break
				}
				ip := p.IP
				if ip == "" {
					ip = "*"
				}
				fmt.Fprintf(&b, "- %s:%d/%s %s (%s)\n", ip, p.Port, p.Proto, p.Owner(), p.State)
			}
			return b.String(), nil
		},
	})
}

func orNone(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func stackHasService(sm engine.StackSummary, want string) bool {
	return strings.Contains(strings.ToLower(sm.DisplayName), want)
}

func filterLinks(ls []serviceLink, want string) []serviceLink {
	var out []serviceLink
	for _, l := range ls {
		if strings.Contains(strings.ToLower(l.Service), want) || strings.Contains(strings.ToLower(l.Container), want) {
			out = append(out, l)
		}
	}
	return out
}

// safeLink: only http(s) links become clickable.
func safeLink(u string) bool {
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}
