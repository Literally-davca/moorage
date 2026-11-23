// Package sso signs users in through an OpenID Connect provider or a trusted reverse
// proxy (forward-auth). It only proves who someone is and which role their groups give;
// accounts and sessions stay in the auth package.
package sso

import (
	"strings"

	"moorage/internal/config"
)

// Role maps groups to a role on every sign-in: an admin group wins over a viewer group,
// then the default role. "" means no access.
func Role(m config.RoleMapping, groups []string) string {
	switch {
	case anyGroup(groups, m.AdminGroups):
		return "admin"
	case anyGroup(groups, m.ViewerGroups):
		return "viewer"
	case m.DefaultRole == "viewer":
		return "viewer"
	}
	return ""
}

// anyGroup compares exactly, except that one leading slash is ignored: Keycloak's
// group mapper sends full paths ("/admins") when "Full group path" is on.
func anyGroup(have, want []string) bool {
	for _, h := range have {
		h = strings.TrimPrefix(strings.TrimSpace(h), "/")
		for _, w := range want {
			if h != "" && h == strings.TrimPrefix(w, "/") {
				return true
			}
		}
	}
	return false
}

// lookupClaim finds a claim by name or by a dotted path into nested objects
// ("realm_access.roles" for Keycloak realm roles). A top-level claim whose name contains
// the dots wins.
func lookupClaim(claims map[string]any, path string) (any, bool) {
	if v, ok := claims[path]; ok {
		return v, true
	}
	var cur any = claims
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[part]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// groupsOf accepts a list of strings or a single string (one group).
func groupsOf(v any) []string {
	switch x := v.(type) {
	case string:
		if x = strings.TrimSpace(x); x != "" {
			return []string{x}
		}
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case []string:
		return x
	}
	return nil
}
