package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"go.yaml.in/yaml/v4"

	"moorage/internal/host"
	"moorage/internal/lint"
	"moorage/internal/monitor"
	"moorage/internal/store"
)

var customAddonIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,56}$`)

type customAddonReq struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Category    string   `json:"category"`
	Description string   `json:"description"`
	HostID      string   `json:"hostId"`
	HostIDs     []string `json:"hostIds"`
	Compose     string   `json:"compose"`
	Env         string   `json:"env"`
}

func addonEnvPrefix(addonID string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(addonID) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	p := strings.Trim(b.String(), "_")
	if p == "" {
		return "ADDON"
	}
	return p
}

func parseEnvLinesMap(raw string) map[string]string {
	m, err := dotenv.UnmarshalBytesWithLookup([]byte(raw), nil)
	if err != nil || m == nil {
		out := map[string]string{}
		for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
			t := strings.TrimSpace(line)
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			t = strings.TrimPrefix(t, "export ")
			if k, v, ok := strings.Cut(t, "="); ok {
				k = strings.TrimSpace(k)
				if k != "" {
					out[k] = strings.Trim(strings.TrimSpace(v), `"'`)
				}
			}
		}
		return out
	}
	return m
}

func removeEnvKeys(envText string, keys []string) string {
	if len(keys) == 0 || strings.TrimSpace(envText) == "" {
		return envText
	}
	drop := map[string]bool{}
	for _, k := range keys {
		drop[k] = true
	}
	var kept []string
	for _, line := range strings.Split(strings.ReplaceAll(envText, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		if t != "" && !strings.HasPrefix(t, "#") {
			cand := strings.TrimSpace(strings.TrimPrefix(t, "export "))
			if k, _, ok := strings.Cut(cand, "="); ok && drop[strings.TrimSpace(k)] {
				continue
			}
		}
		kept = append(kept, line)
	}
	return strings.TrimRight(strings.Join(kept, "\n"), "\n") + "\n"
}

func extractEnvSubset(envText string, keys []string) string {
	if len(keys) == 0 || strings.TrimSpace(envText) == "" {
		return ""
	}
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
	}
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(envText, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		cand := strings.TrimSpace(strings.TrimPrefix(t, "export "))
		if k, _, ok := strings.Cut(cand, "="); ok && want[strings.TrimSpace(k)] {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "\n") + "\n"
}

func formatEnvAssignment(k, v string) string {
	if strings.ContainsAny(v, " \t\n\"'#") {
		escaped := strings.ReplaceAll(v, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		escaped = strings.ReplaceAll(escaped, "\n", `\n`)
		return fmt.Sprintf(`%s="%s"`, k, escaped)
	}
	return k + "=" + v
}

// mergeAddonEnvWithPrefix runs lint.MoveEnv to pull literal environment values from
// composeText into addonEnv, then merges addonEnv into baseAddonsEnv (after stripping
// prevOwnedKeys). Any key colliding with another add-on's key in baseAddonsEnv is
// automatically prefixed with <ADDON_ID>_ in both the compose fragment and .env.
func mergeAddonEnvWithPrefix(
	addonID, composeText, addonEnv, baseAddonsEnv string,
	prevOwnedKeys []string,
) (rewrittenCompose, mergedEnv string, ownedKeys []string, err error) {
	expandedEnv := removeEnvKeys(baseAddonsEnv, prevOwnedKeys)
	baseMap := parseEnvLinesMap(expandedEnv)

	moved, err := lint.MoveEnv(composeText, addonEnv, true)
	if err != nil {
		return "", "", nil, err
	}
	rewrittenCompose = moved.Compose
	addonMap := parseEnvLinesMap(moved.Env)
	if len(addonMap) == 0 {
		return rewrittenCompose, strings.TrimRight(expandedEnv, "\n"), nil, nil
	}

	prefix := addonEnvPrefix(addonID)
	keys := make([]string, 0, len(addonMap))
	for k := range addonMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var appendLines []string
	for _, k := range keys {
		val := addonMap[k]
		finalKey := k
		if existingVal, exists := baseMap[k]; exists && existingVal != val {
			finalKey = prefix + "_" + k
			rewrittenCompose = replaceComposeVarRef(rewrittenCompose, k, finalKey)
		}
		ownedKeys = append(ownedKeys, finalKey)
		if _, alreadySame := baseMap[finalKey]; !alreadySame || baseMap[finalKey] != val {
			expandedEnv = removeEnvKeys(expandedEnv, []string{finalKey})
			appendLines = append(appendLines, formatEnvAssignment(finalKey, val))
		}
	}

	out := strings.TrimRight(expandedEnv, "\n")
	if len(appendLines) > 0 {
		if out != "" {
			out += "\n"
		}
		out += strings.Join(appendLines, "\n") + "\n"
	} else if out != "" {
		out += "\n"
	}
	return rewrittenCompose, out, ownedKeys, nil
}

func replaceComposeVarRef(composeText, oldKey, newKey string) string {
	composeText = strings.ReplaceAll(composeText, "${"+oldKey+"}", "${"+newKey+"}")
	composeText = strings.ReplaceAll(composeText, "${"+oldKey+":-", "${"+newKey+":-")
	composeText = strings.ReplaceAll(composeText, "${"+oldKey+"-", "${"+newKey+"-")
	composeText = strings.ReplaceAll(composeText, "${"+oldKey+":?", "${"+newKey+":?")
	return composeText
}

// POST /api/addons/custom
func (s *Server) createCustomAddon(w http.ResponseWriter, r *http.Request) {
	var req customAddonReq
	if !readJSON(w, r, &req) {
		return
	}
	id := strings.TrimSpace(req.ID)
	name := strings.TrimSpace(req.Name)
	category := strings.TrimSpace(req.Category)
	if category == "" {
		category = "Custom"
	}
	if !customAddonIDRe.MatchString(id) || builtinAddonIDs[id] {
		writeErr(w, http.StatusBadRequest, "Use a valid lowercase slug (1–57 chars: letters, digits, dash or underscore) not reserved by a built-in add-on.")
		return
	}
	if name == "" {
		name = id
	}
	if strings.TrimSpace(req.Compose) == "" {
		writeErr(w, http.StatusBadRequest, "Provide a compose.yaml fragment with at least one service.")
		return
	}

	var targetHosts []string
	seen := map[string]bool{}
	for _, hid := range append(req.HostIDs, req.HostID) {
		hid = strings.TrimSpace(hid)
		if hid != "" && !seen[hid] {
			seen[hid] = true
			targetHosts = append(targetHosts, hid)
		}
	}
	if len(targetHosts) == 0 {
		writeErr(w, http.StatusBadRequest, "Select at least one writable host.")
		return
	}

	ctx := r.Context()
	// Expand shared secret placeholders if the user inserted any via EnvEditor.
	expandedEnv := s.expandSharedSecrets(ctx, targetHosts[0], req.Env)

	issues := lint.Lint(ctx, req.Compose, expandedEnv, lint.Options{Name: addonsStackName})
	if b := lint.Blocking(issues, false); b != nil {
		writeErr(w, http.StatusUnprocessableEntity, b.Message)
		return
	}

	user := currentUser(r).Username
	for _, hostID := range targetHosts {
		if _, err := s.store.InstalledAddonByHost(ctx, id, hostID); err == nil {
			writeErr(w, http.StatusConflict, fmt.Sprintf("Add-on %q is already installed on host %s.", id, hostID))
			return
		}
		h, meta, err := s.reg.Get(ctx, hostID)
		if err != nil {
			status, msg := s.errStatus(err)
			writeErr(w, status, msg)
			return
		}
		if meta.ReadOnly {
			writeErr(w, http.StatusForbidden, host.ErrHostReadOnly.Error())
			return
		}

		hostEnv := s.expandSharedSecrets(ctx, hostID, req.Env)
		baseAddonsEnv := ""
		if cur, err := h.GetStack(ctx, addonsStackName); err == nil {
			baseAddonsEnv = cur.Env
		}
		fragCompose, mergedEnv, ownedKeys, err := mergeAddonEnvWithPrefix(id, req.Compose, hostEnv, baseAddonsEnv, nil)
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if err := s.deploySidecarToHost(ctx, h, hostID, id, "Installed custom add-on "+name, []byte(fragCompose), mergedEnv, true, user); err != nil {
			s.engErr(w, err)
			return
		}
		if err := s.store.UpsertInstalledAddon(ctx, store.InstalledAddon{
			ID:              id,
			HostID:          hostID,
			Name:            name,
			Kind:            "sidecar",
			Category:        category,
			Description:     strings.TrimSpace(req.Description),
			Version:         "custom",
			StackName:       addonsStackName,
			Enabled:         true,
			InstalledAt:     time.Now(),
			Custom:          true,
			ComposeFragment: fragCompose,
			EnvKeys:         ownedKeys,
			Source:          "custom",
		}); err != nil {
			s.engErr(w, err)
			return
		}
	}

	setAuditTarget(r, id)
	w.WriteHeader(http.StatusCreated)
}

// GET /api/addons/custom/{id}?host={hostId}
func (s *Server) getCustomAddon(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	hostQuery := strings.TrimSpace(r.URL.Query().Get("host"))
	ctx := r.Context()

	ia, err := s.resolveInstalledSidecar(ctx, id, hostQuery)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "Add-on not found.")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	envSubset := ""
	composeFrag := ia.ComposeFragment
	if s.reg != nil && ia.HostID != "" {
		if h, _, err := s.reg.Get(ctx, ia.HostID); err == nil {
			if cur, err := h.GetStack(ctx, addonsStackName); err == nil {
				envSubset = extractEnvSubset(cur.Env, ia.EnvKeys)
				if composeFrag == "" {
					composeFrag = extractAddonFragmentFromMerged(cur.Compose, ia.ID)
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":          ia.ID,
		"hostId":      ia.HostID,
		"name":        ia.Name,
		"category":    ia.Category,
		"description": ia.Description,
		"compose":     composeFrag,
		"env":         envSubset,
		"custom":      ia.Custom,
	})
}

func extractAddonFragmentFromMerged(mergedYAML, addonID string) string {
	doc, err := parseComposeMap([]byte(mergedYAML))
	if err != nil {
		return ""
	}
	svcs := asMap(doc["services"])
	picked := map[string]any{}
	for name, rawSvc := range svcs {
		svc := asMap(rawSvc)
		if svc != nil && (hasServiceAddonLabel(svc, addonID) || name == addonID) {
			picked[name] = svc
		}
	}
	if len(picked) == 0 {
		return ""
	}
	out, err := yaml.Marshal(map[string]any{"services": picked})
	if err != nil {
		return ""
	}
	return string(out)
}

// PUT /api/addons/custom/{id}
func (s *Server) updateCustomAddon(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req customAddonReq
	if !readJSON(w, r, &req) {
		return
	}
	hostID := strings.TrimSpace(r.URL.Query().Get("host"))
	if hostID == "" {
		hostID = strings.TrimSpace(req.HostID)
	}
	ctx := r.Context()

	ia, err := s.resolveInstalledSidecar(ctx, id, hostID)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "Add-on not found.")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Compose) == "" {
		writeErr(w, http.StatusBadRequest, "Provide a compose.yaml fragment with at least one service.")
		return
	}

	h, meta, err := s.reg.Get(ctx, ia.HostID)
	if err != nil {
		status, msg := s.errStatus(err)
		writeErr(w, status, msg)
		return
	}
	if meta.ReadOnly {
		writeErr(w, http.StatusForbidden, host.ErrHostReadOnly.Error())
		return
	}

	hostEnv := s.expandSharedSecrets(ctx, ia.HostID, req.Env)
	issues := lint.Lint(ctx, req.Compose, hostEnv, lint.Options{Name: addonsStackName})
	if b := lint.Blocking(issues, false); b != nil {
		writeErr(w, http.StatusUnprocessableEntity, b.Message)
		return
	}

	baseAddonsEnv := ""
	if cur, err := h.GetStack(ctx, addonsStackName); err == nil {
		baseAddonsEnv = cur.Env
	}
	fragCompose, mergedEnv, ownedKeys, err := mergeAddonEnvWithPrefix(ia.ID, req.Compose, hostEnv, baseAddonsEnv, ia.EnvKeys)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = ia.Name
	}
	category := strings.TrimSpace(req.Category)
	if category == "" {
		category = ia.Category
	}

	user := currentUser(r).Username
	if err := s.deploySidecarToHost(ctx, h, ia.HostID, ia.ID, "Updated custom add-on "+name, []byte(fragCompose), mergedEnv, true, user); err != nil {
		s.engErr(w, err)
		return
	}

	ia.Name = name
	ia.Category = category
	ia.Description = strings.TrimSpace(req.Description)
	ia.Custom = true
	ia.Enabled = true
	ia.ComposeFragment = fragCompose
	ia.EnvKeys = ownedKeys
	if err := s.store.UpsertInstalledAddon(ctx, ia); err != nil {
		s.engErr(w, err)
		return
	}
	setAuditTarget(r, id+"@"+ia.HostID)
	w.WriteHeader(http.StatusNoContent)
}

// ---- Move managed stack into add-ons (FEATURE-DESIGN 1.5.4) ----------------------------

// rewriteComposeForMoveToAddons preserves existing named Docker volumes via
// `name: <origStack>_<vol>` and rewrites relative bind mounts (`./data` -> `./<addonID>/data`)
// so the container keeps all existing data without copying.
func rewriteComposeForMoveToAddons(origStack, addonID, composeYAML string) (string, error) {
	doc, err := parseComposeMap([]byte(composeYAML))
	if err != nil {
		return "", err
	}
	svcs := asMap(doc["services"])
	if len(svcs) == 0 {
		return "", errors.New("stack compose.yaml defines no services")
	}

	namedVols := map[string]bool{}
	if topVols := asMap(doc["volumes"]); topVols != nil {
		for volKey, rawCfg := range topVols {
			namedVols[volKey] = true
			cfg := asMap(rawCfg)
			if cfg == nil {
				cfg = map[string]any{}
			}
			if ext, _ := cfg["external"].(bool); !ext {
				if _, hasName := cfg["name"]; !hasName {
					cfg["name"] = origStack + "_" + volKey
				}
			}
			topVols[volKey] = cfg
		}
	}

	for _, rawSvc := range svcs {
		svc := asMap(rawSvc)
		if svc == nil {
			continue
		}
		rawVolList, ok := svc["volumes"].([]any)
		if !ok {
			continue
		}
		for i, item := range rawVolList {
			switch v := item.(type) {
			case string:
				src, rest, hasColon := strings.Cut(v, ":")
				if hasColon && isRelativeBindSource(src, namedVols) {
					rawVolList[i] = rewriteRelativeBindPath(src, addonID) + ":" + rest
				}
			case map[string]any:
				typ, _ := v["type"].(string)
				src, _ := v["source"].(string)
				if (typ == "bind" || typ == "") && src != "" && isRelativeBindSource(src, namedVols) {
					v["source"] = rewriteRelativeBindPath(src, addonID)
				}
			}
		}
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func isRelativeBindSource(src string, namedVols map[string]bool) bool {
	if src == "" || strings.HasPrefix(src, "/") || strings.HasPrefix(src, "~") || strings.HasPrefix(src, "$") {
		return false
	}
	if namedVols[src] {
		return false
	}
	return strings.HasPrefix(src, "./") || strings.HasPrefix(src, "../") || strings.Contains(src, "/") || src == "."
}

func rewriteRelativeBindPath(src, addonID string) string {
	clean := strings.TrimPrefix(src, "./")
	if clean == "." || clean == "" {
		return "./" + addonID
	}
	return "./" + addonID + "/" + clean
}

// POST /api/hosts/{host}/stacks/{name}/move-to-addons
func (s *Server) moveStackToAddons(w http.ResponseWriter, r *http.Request) {
	h, ok := s.hostOf(w, r, true)
	if !ok {
		return
	}
	hostID := r.PathValue("host")
	origStack := r.PathValue("name")
	ctx := r.Context()

	if origStack == addonsStackName {
		writeErr(w, http.StatusBadRequest, "This stack is already the add-ons stack.")
		return
	}
	if !customAddonIDRe.MatchString(origStack) || builtinAddonIDs[origStack] {
		writeErr(w, http.StatusBadRequest, "This stack name cannot be used as an add-on identifier.")
		return
	}
	if _, err := s.store.InstalledAddonByHost(ctx, origStack, hostID); err == nil {
		writeErr(w, http.StatusConflict, fmt.Sprintf("An add-on named %q is already installed on this host.", origStack))
		return
	}

	st, err := h.GetStack(ctx, origStack)
	if err != nil {
		s.engErr(w, err)
		return
	}
	if !st.Managed {
		writeErr(w, http.StatusConflict, "Only managed stacks on disk can be moved into add-ons.")
		return
	}
	if st.Self {
		writeErr(w, http.StatusConflict, "Moorage runs from this stack and cannot move itself into add-ons.")
		return
	}

	rewrittenCompose, err := rewriteComposeForMoveToAddons(origStack, origStack, st.Compose)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	var prevAddonsCompose, prevAddonsEnv string
	if curAddons, err := h.GetStack(ctx, addonsStackName); err == nil {
		prevAddonsCompose = curAddons.Compose
		prevAddonsEnv = curAddons.Env
	}
	// Pre-check service collisions BEFORE stopping the original stack!
	if _, err := mergeAddonCompose(prevAddonsCompose, origStack, []byte(rewrittenCompose)); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}

	fragCompose, mergedEnv, ownedKeys, err := mergeAddonEnvWithPrefix(origStack, rewrittenCompose, st.Env, prevAddonsEnv, nil)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	user := currentUser(r).Username
	if s.mon != nil {
		s.mon.StackEvent(hostID, origStack, "move", user, "Moving stack into add-ons")
	}

	// 1. Stop original stack without -v so named volumes stay untouched.
	if st.Running > 0 || len(st.Containers) > 0 {
		if err := runAndWait(ctx, h, origStack, "down", user); err != nil {
			s.engErr(w, err)
			return
		}
	}

	// 2. Relocate relative bind mounts on the host filesystem (<stacks>/<orig>/... -> <stacks>/add-ons/<orig>/...).
	relocated := false
	if br, ok := h.(host.BindMountRelocator); ok {
		if err := br.RelocateBindMounts(ctx, origStack, origStack, false); err != nil {
			if host.IsUnsupported(err) {
				writeErr(w, http.StatusNotImplemented, "Update the agent on this host to move stacks with bind mounts.")
				return
			}
			s.engErr(w, err)
			return
		}
		relocated = true
	}

	// 3. Merge into add-ons and run `up`. On failure, restore add-ons AND relocated bind mounts + restart origStack!
	if err := s.deploySidecarToHost(ctx, h, hostID, origStack, fmt.Sprintf("Moved stack %s into add-ons", origStack), []byte(fragCompose), mergedEnv, true, user); err != nil {
		if relocated {
			if br, ok := h.(host.BindMountRelocator); ok {
				_ = br.RelocateBindMounts(ctx, origStack, origStack, true)
			}
		}
		_ = runAndWait(ctx, h, origStack, "up", user)
		s.engErr(w, err)
		return
	}

	// 4. Delete original stack folder now that add-ons is up and healthy (records delete in history.git).
	if err := h.Delete(ctx, origStack, user); err != nil {
		s.log.Warn("delete moved stack folder", "stack", origStack, "err", err)
	}

	// 5. Record as custom sidecar in installed_addons.
	_ = s.store.UpsertInstalledAddon(ctx, store.InstalledAddon{
		ID:              origStack,
		HostID:          hostID,
		Name:            origStack,
		Kind:            "sidecar",
		Category:        "Custom",
		Description:     "Moved from standalone stack " + origStack,
		Version:         "custom",
		StackName:       addonsStackName,
		Enabled:         true,
		InstalledAt:     time.Now(),
		Custom:          true,
		ComposeFragment: fragCompose,
		EnvKeys:         ownedKeys,
		Source:          "moved",
	})

	// 6. Retarget stack checks watching (hostID, origStack) -> (hostID, "add-ons").
	s.retargetStackChecks(ctx, hostID, origStack, addonsStackName)

	writeJSON(w, http.StatusOK, map[string]string{
		"stack": addonsStackName,
		"addon": origStack,
	})
}

func (s *Server) retargetStackChecks(ctx context.Context, hostID, fromStack, toStack string) {
	if s.store == nil {
		return
	}
	checks, err := s.store.Checks(ctx)
	if err != nil {
		return
	}
	for _, c := range checks {
		changed := false
		if c.HostID == hostID && c.Stack == fromStack {
			c.Stack = toStack
			changed = true
		}
		if c.Kind == monitor.KindStack && c.HostID == hostID && c.Target == fromStack {
			c.Target = toStack
			changed = true
		}
		if c.Settings != "" {
			var st monitor.Settings
			if json.Unmarshal([]byte(c.Settings), &st) == nil && len(st.Stacks) > 0 {
				subChanged := false
				for i := range st.Stacks {
					if st.Stacks[i].HostID == hostID && st.Stacks[i].Stack == fromStack {
						st.Stacks[i].Stack = toStack
						subChanged = true
					}
				}
				if subChanged {
					if b, err := json.Marshal(st); err == nil {
						c.Settings = string(b)
						changed = true
					}
				}
			}
		}
		if changed {
			if id, err := s.store.SaveCheck(ctx, c); err == nil {
				if s.mon != nil {
					s.mon.Reload(id)
				}
			}
		}
	}
	if s.status != nil {
		s.status.Invalidate()
	}
}
