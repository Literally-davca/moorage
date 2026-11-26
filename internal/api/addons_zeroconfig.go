package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"moorage/internal/host"
	"moorage/internal/store"
)

// ---- zero-config sidecar add-ons (M17B) ------------------------------------------------
//
// Installing a catalog sidecar fills the host-specific variables its compose fragment
// uses into add-ons/.env, without ever overwriting a value the user set:
//
//	MOORAGE_STACKS_DIR   the stacks folder of that host (mounted at the same path)
//	MOORAGE_BACKUPS_DIR  <data dir on the host>/backups when Moorage's data is a bind mount,
//	                     empty otherwise (the template falls back to a named volume)
//	HOST_IP              the default address of the host
//	TZ                   the hub's time zone, when known
//	RESTIC_PASSWORD      a random repository password, generated once
//
// The add-ons stack gets the auto update policy the first time it is created.

var envRefRe = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)`)

// referencedVars lists the ${VAR} / $VAR names a compose fragment uses.
func referencedVars(compose string) map[string]bool {
	out := map[string]bool{}
	for _, m := range envRefRe.FindAllStringSubmatch(compose, -1) {
		out[m[1]] = true
	}
	return out
}

// generatedSecretVars are filled with a random value when a fragment uses them.
var generatedSecretVars = map[string]bool{"RESTIC_PASSWORD": true}

// addonEnvResult says what installAddon added to add-ons/.env.
type addonEnvResult struct {
	Added     []string `json:"envAdded,omitempty"`
	Generated []string `json:"generatedSecrets,omitempty"`
}

// withAddonEnvDefaults appends the defaults the fragment uses and .env doesn't set yet.
func withAddonEnvDefaults(env, fragment string, defaults map[string]string) (string, addonEnvResult) {
	refs := referencedVars(fragment)
	have := parseEnvLinesMap(env)
	var res addonEnvResult
	keys := make([]string, 0, len(defaults))
	for k := range defaults {
		keys = append(keys, k)
	}
	for k := range generatedSecretVars {
		if _, ok := defaults[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var lines []string
	for _, k := range keys {
		if !refs[k] {
			continue
		}
		if _, set := have[k]; set {
			continue
		}
		v, ok := defaults[k]
		if generatedSecretVars[k] {
			v, ok = randomSecret(), true
			res.Generated = append(res.Generated, k)
		}
		if !ok {
			continue
		}
		lines = append(lines, formatEnvAssignment(k, v))
		res.Added = append(res.Added, k)
	}
	if len(lines) == 0 {
		return env, res
	}
	out := strings.TrimRight(strings.ReplaceAll(env, "\r\n", "\n"), "\n")
	if out != "" {
		out += "\n"
	}
	out += "# Filled in by Moorage when the add-on was installed (change freely)\n"
	if len(res.Generated) > 0 {
		out += "# " + strings.Join(res.Generated, ", ") + ": generated, keep a copy: without it the backups can't be restored\n"
	}
	return out + strings.Join(lines, "\n") + "\n", res
}

func randomSecret() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// addonTimeZone is TZ of the hub, the zone of time.Local or /etc/timezone ("" when unknown).
func addonTimeZone() string {
	if tz := strings.TrimSpace(os.Getenv("TZ")); tz != "" && !strings.HasPrefix(tz, ":") {
		return tz
	}
	if n := time.Local.String(); n != "" && n != "Local" && n != "UTC" {
		return n
	}
	if b, err := os.ReadFile("/etc/timezone"); err == nil {
		if tz := strings.TrimSpace(string(b)); tz != "" {
			return tz
		}
	}
	return ""
}

// addonEnvDefaults are the host-specific defaults for one host.
func (s *Server) addonEnvDefaults(ctx context.Context, hostID string, h host.Host, meta host.Entry) map[string]string {
	d := map[string]string{"MOORAGE_BACKUPS_DIR": ""}
	if dir := strings.TrimSpace(h.Info(ctx).StacksDir); dir != "" {
		d["MOORAGE_STACKS_DIR"] = dir
	}
	if dp := strings.TrimSpace(meta.DataHostPath); dp != "" {
		d["MOORAGE_BACKUPS_DIR"] = path.Join(dp, "backups")
	}
	if _, def := s.hostAddresses(ctx, hostID, meta); def != "" {
		d["HOST_IP"] = def
	}
	if tz := addonTimeZone(); tz != "" {
		d["TZ"] = tz
	}
	return d
}

// ensureAddonsAutoPolicy turns on automatic updates for a freshly created add-ons stack,
// unless the user already chose a policy for it (an earlier add-ons stack on that host).
func (s *Server) ensureAddonsAutoPolicy(ctx context.Context, hostID string) {
	if s.store == nil || hostID == "" {
		return
	}
	if has, err := s.store.HasStackUpdatePolicy(ctx, hostID, addonsStackName); err != nil || has {
		return
	}
	if err := s.store.SaveStackUpdatePolicy(ctx, store.StackUpdatePolicy{HostID: hostID, Stack: addonsStackName, Policy: store.PolicyAuto}); err != nil {
		s.log.Warn("add-ons: could not enable automatic updates", "host", hostID, "err", err)
	}
}
