package api

import (
	"net/http"
	"strings"

	"moorage/internal/host"
)

type socketFinding struct {
	HostID   string `json:"hostId"`
	HostName string `json:"hostName"`
	Stack    string `json:"stack"`
	Service  string `json:"service"`
	Severity string `json:"severity"` // critical | warning
	Issue    string `json:"issue"`
	Detail   string `json:"detail"`
}

// GET /api/addons/socket-proxy/scan
func (s *Server) scanSocketProxy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.reg == nil {
		writeJSON(w, http.StatusOK, map[string]any{"findings": []socketFinding{}, "snippet": socketProxySnippet(true)})
		return
	}
	hosts, _ := s.reg.List(ctx)
	var findings []socketFinding

	for _, hMeta := range hosts {
		if !hMeta.Connected {
			continue
		}
		h, _, err := s.reg.Get(ctx, hMeta.ID)
		if err != nil {
			continue
		}
		stacks, err := h.ListStacks(ctx)
		if err != nil {
			continue
		}
		for _, sm := range stacks {
			if sm.Self {
				continue // Moorage itself legitimately mounts the engine socket
			}
			st, err := h.GetStack(ctx, sm.Name)
			if err != nil || st.Compose == "" {
				continue
			}
			doc, err := parseComposeMap([]byte(st.Compose))
			if err != nil {
				continue
			}
			svcs := asMap(doc["services"])
			for svcName, rawSvc := range svcs {
				svc := asMap(rawSvc)
				if svc == nil {
					continue
				}
				// Check volumes for /var/run/docker.sock
				if vols, ok := svc["volumes"].([]any); ok {
					for _, item := range vols {
						var spec string
						switch v := item.(type) {
						case string:
							spec = v
						case map[string]any:
							src, _ := v["source"].(string)
							if ro, _ := v["read_only"].(bool); ro {
								spec = src + ":ro"
							} else {
								spec = src
							}
						}
						if strings.Contains(spec, "docker.sock") {
							isRO := strings.HasSuffix(spec, ":ro") || strings.Contains(spec, ":ro,")
							sev := "critical"
							issue := "Read-write Docker socket mount"
							if isRO {
								sev = "warning"
								issue = "Direct read-only Docker socket mount"
							}
							findings = append(findings, socketFinding{
								HostID:   hMeta.ID,
								HostName: hMeta.Name,
								Stack:    sm.Name,
								Service:  svcName,
								Severity: sev,
								Issue:    issue,
								Detail:   spec,
							})
						}
					}
				}
				// Check environment / command for unencrypted tcp://...:2375
				rawEnv := ""
				switch ev := svc["environment"].(type) {
				case map[string]any:
					for k, v := range ev {
						rawEnv += fmtEnv(k, v)
					}
				case []any:
					for _, item := range ev {
						if str, ok := item.(string); ok {
							rawEnv += str + "\n"
						}
					}
				}
				if strings.Contains(rawEnv, ":2375") && !strings.Contains(rawEnv, "socket-proxy") {
					findings = append(findings, socketFinding{
						HostID:   hMeta.ID,
						HostName: hMeta.Name,
						Stack:    sm.Name,
						Service:  svcName,
						Severity: "critical",
						Issue:    "Unencrypted TCP Docker socket (:2375)",
						Detail:   "Expose Docker via tecnativa/docker-socket-proxy on an internal network instead.",
					})
				}
			}
		}
	}

	if findings == nil {
		findings = []socketFinding{}
	}
	roMode := r.URL.Query().Get("mode") != "write"
	writeJSON(w, http.StatusOK, map[string]any{
		"findings": findings,
		"snippet":  socketProxySnippet(roMode),
	})
}

func fmtEnv(k string, v any) string {
	s, _ := v.(string)
	return k + "=" + s + "\n"
}

func socketProxySnippet(readOnly bool) string {
	post := "0"
	if !readOnly {
		post = "1"
	}
	return `services:
  docker-socket-proxy:
    image: tecnativa/docker-socket-proxy:latest
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    environment:
      CONTAINERS: 1
      IMAGES: 1
      INFO: 1
      NETWORKS: 1
      VOLUMES: 1
      POST: ` + post + `
    networks:
      - socket_proxy

networks:
  socket_proxy:
    internal: true
`
}

var _ = host.LocalID
