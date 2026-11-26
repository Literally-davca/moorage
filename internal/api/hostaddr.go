package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/store"
)

// ---- host addresses (M17B): Ethernet / Wi-Fi / VPN / domain / custom ------------------
//
// Sources, in order: MOORAGE_HOST_IP of that host (locked, always the default), the saved
// list host_addresses:<hostID> (manual entries and the chosen default), the legacy
// host_ip:<hostID> value (only while no list is saved) and the interfaces the host detects
// itself in native / host-network mode.

const maxHostAddresses = 20

var addressKinds = map[string]bool{engine.AddrEthernet: true, engine.AddrWiFi: true, engine.AddrVPN: true, engine.AddrDomain: true, engine.AddrCustom: true}

func addressKindOf(addr string) string {
	if _, err := netip.ParseAddr(strings.Trim(addr, "[]")); err == nil {
		return engine.AddrCustom
	}
	return engine.AddrDomain
}

// savedHostAddresses reads host_addresses:<hostID>; ok=false when nothing is saved.
func (s *Server) savedHostAddresses(ctx context.Context, hostID string) ([]engine.HostAddress, bool) {
	if s.store == nil {
		return nil, false
	}
	raw, err := s.store.GetSetting(ctx, store.HostAddressesKey(hostID))
	if err != nil || strings.TrimSpace(raw) == "" {
		return nil, false
	}
	var list []engine.HostAddress
	if json.Unmarshal([]byte(raw), &list) != nil {
		return nil, false
	}
	return list, true
}

// mergeHostAddresses builds the address list of a host. Exactly one entry is the default
// when the list isn't empty.
func mergeHostAddresses(envIP, legacyIP string, saved []engine.HostAddress, hasSaved bool, detected []engine.HostAddress) []engine.HostAddress {
	var out []engine.HostAddress
	seen := map[string]bool{}
	add := func(a engine.HostAddress) {
		k := strings.ToLower(a.Address)
		if a.Address == "" || seen[k] {
			return
		}
		seen[k] = true
		out = append(out, a)
	}
	envIP = strings.TrimSpace(envIP)
	if envIP != "" {
		add(engine.HostAddress{ID: "env", Label: "MOORAGE_HOST_IP", Kind: addressKindOf(envIP), Address: envIP, IsDefault: true, Locked: true})
	}
	defaultAuto := ""
	if hasSaved {
		for _, a := range saved {
			if a.AutoDetected {
				if a.IsDefault {
					defaultAuto = strings.ToLower(a.Address)
				}
				continue
			}
			a.IsDefault = a.IsDefault && envIP == ""
			a.Locked, a.Missing = false, false
			add(a)
		}
	} else if ip := strings.TrimSpace(legacyIP); ip != "" {
		add(engine.HostAddress{ID: "primary", Label: "Primary", Kind: addressKindOf(ip), Address: ip, IsDefault: envIP == ""})
	}
	for _, d := range detected {
		d.AutoDetected = true
		d.IsDefault = envIP == "" && strings.ToLower(d.Address) == defaultAuto
		add(d)
	}
	if defaultAuto != "" && !seen[defaultAuto] { // the chosen interface is down right now: keep it
		for _, a := range saved {
			if a.AutoDetected && strings.ToLower(a.Address) == defaultAuto {
				a.IsDefault, a.Missing = envIP == "", true
				add(a)
			}
		}
	}
	def := -1
	for i := range out {
		if out[i].IsDefault {
			if def >= 0 {
				out[i].IsDefault = false
			} else {
				def = i
			}
		}
	}
	if def < 0 && len(out) > 0 {
		out[0].IsDefault = true
	}
	return out
}

// hostAddresses returns the merged address list of a host and its default address.
func (s *Server) hostAddresses(ctx context.Context, hostID string, e host.Entry) ([]engine.HostAddress, string) {
	legacy := ""
	saved, hasSaved := s.savedHostAddresses(ctx, hostID)
	if !hasSaved && s.store != nil {
		legacy, _ = s.store.GetSetting(ctx, store.HostIPKey(hostID))
	}
	list := mergeHostAddresses(e.HostIP, legacy, saved, hasSaved, e.DetectedAddresses)
	for _, a := range list {
		if a.IsDefault {
			return list, a.Address
		}
	}
	return list, ""
}

// alternativeAddresses are all addresses but the default.
func alternativeAddresses(list []engine.HostAddress) []engine.HostAddress {
	var out []engine.HostAddress
	for _, a := range list {
		if !a.IsDefault && !a.Missing {
			out = append(out, a)
		}
	}
	return out
}

func (s *Server) getHostAddresses(w http.ResponseWriter, r *http.Request) {
	if s.reg == nil {
		writeErr(w, http.StatusNotFound, "Host not found.")
		return
	}
	hostID := r.PathValue("host")
	_, e, err := s.reg.Get(r.Context(), hostID)
	if errors.Is(err, host.ErrUnknownHost) {
		writeErr(w, http.StatusNotFound, "Host not found.")
		return
	}
	list, def := s.hostAddresses(r.Context(), hostID, e)
	if list == nil {
		list = []engine.HostAddress{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"addresses": list, "default": def, "netMode": e.NetMode, "connected": e.Connected})
}

// putHostAddresses saves the manual addresses and the choice of the default (which may be
// an auto-detected one). Auto-detected entries themselves are never stored, except the
// chosen default, so it survives the interface going down for a while.
func (s *Server) putHostAddresses(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Addresses []engine.HostAddress `json:"addresses"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if s.reg == nil {
		writeErr(w, http.StatusNotFound, "Host not found.")
		return
	}
	ctx := r.Context()
	hostID := r.PathValue("host")
	if _, _, err := s.reg.Get(ctx, hostID); errors.Is(err, host.ErrUnknownHost) {
		writeErr(w, http.StatusNotFound, "Host not found.")
		return
	}
	list, msg := cleanHostAddresses(req.Addresses)
	if msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	b, _ := json.Marshal(list)
	if err := s.store.SetSetting(ctx, store.HostAddressesKey(hostID), string(b)); err != nil {
		s.engErr(w, err)
		return
	}
	// Keep the legacy key on the default so older code paths (and a downgrade) agree.
	def := ""
	for _, a := range list {
		if a.IsDefault {
			def = a.Address
		}
	}
	if err := s.store.SetSetting(ctx, store.HostIPKey(hostID), def); err != nil {
		s.engErr(w, err)
		return
	}
	setAuditTarget(r, hostID)
	w.WriteHeader(http.StatusNoContent)
}

// cleanHostAddresses validates what the UI sent and keeps what is stored.
func cleanHostAddresses(in []engine.HostAddress) ([]engine.HostAddress, string) {
	var out []engine.HostAddress
	seen := map[string]bool{}
	defaults := 0
	for _, a := range in {
		if a.Locked {
			continue // MOORAGE_HOST_IP: not stored
		}
		a.Address = strings.TrimSpace(a.Address)
		a.Label = strings.TrimSpace(a.Label)
		a.Missing = false
		if a.AutoDetected && !a.IsDefault {
			continue
		}
		if a.Address == "" {
			return nil, "Every address needs a value (IP address or host name)."
		}
		if len(a.Address) > 253 || strings.ContainsAny(a.Address, " /\\?#@") {
			return nil, "An address must be a bare IP address or host name, without spaces, slashes or a port."
		}
		if bare := strings.Trim(a.Address, "[]"); strings.Contains(bare, ":") {
			if _, err := netip.ParseAddr(bare); err != nil {
				return nil, "Leave the port out of " + a.Address + ": links use the published port of each service."
			}
			a.Address = bare
		}
		if a.Label == "" {
			a.Label = a.Address
		}
		if len([]rune(a.Label)) > 40 {
			return nil, "Keep address names to 40 characters."
		}
		if a.Kind == "" {
			a.Kind = addressKindOf(a.Address)
		}
		if !addressKinds[a.Kind] {
			return nil, "Unknown address type " + strconv.Quote(a.Kind) + "."
		}
		k := strings.ToLower(a.Address)
		if seen[k] {
			return nil, a.Address + " is listed twice."
		}
		seen[k] = true
		if a.IsDefault {
			defaults++
		}
		if a.ID == "" || a.ID == "env" {
			a.ID = newAddressID()
		}
		out = append(out, a)
	}
	if len(out) > maxHostAddresses {
		return nil, "A host can have at most " + strconv.Itoa(maxHostAddresses) + " addresses."
	}
	if defaults > 1 {
		return nil, "Only one address can be the default."
	}
	if defaults == 0 {
		for i := range out {
			if !out[i].AutoDetected {
				out[i].IsDefault = true
				break
			}
		}
	}
	if out == nil {
		out = []engine.HostAddress{}
	}
	return out, ""
}

func newAddressID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "m-" + hex.EncodeToString(b[:])
}

// setPrimaryAddress is the old "Primary IP or hostname" field of the host dialog: without
// a saved list it is the legacy key, with one it becomes (or replaces) the default.
func (s *Server) setPrimaryAddress(ctx context.Context, hostID, v string) error {
	saved, hasSaved := s.savedHostAddresses(ctx, hostID)
	if hasSaved {
		var out []engine.HostAddress
		replaced := false
		for _, a := range saved {
			if a.IsDefault && !a.AutoDetected && !replaced {
				a.Address, replaced = v, true
				if v == "" {
					continue
				}
			} else {
				a.IsDefault = false
				if a.AutoDetected {
					continue
				}
			}
			out = append(out, a)
		}
		if !replaced && v != "" {
			out = append([]engine.HostAddress{{ID: newAddressID(), Label: "Primary", Kind: addressKindOf(v), Address: v, IsDefault: true}}, out...)
		}
		if out == nil {
			out = []engine.HostAddress{}
		}
		b, _ := json.Marshal(out)
		if err := s.store.SetSetting(ctx, store.HostAddressesKey(hostID), string(b)); err != nil {
			return err
		}
	}
	return s.store.SetSetting(ctx, store.HostIPKey(hostID), v)
}

// ---- service links on host addresses ------------------------------------------------------

// portURL is http(s)://<address>:<port> for a published TCP port, "" when it can't be
// reached on that address (loopback-only, UDP, or bound to another address).
func portURL(address, raw string) string {
	b, ok := engine.ParsePortString(raw)
	if !ok || b.Proto != "tcp" || engine.IsLoopbackIP(b.IP) {
		return ""
	}
	address = strings.Trim(strings.TrimSpace(address), "[]")
	if address == "" {
		return ""
	}
	if b.IP != "" && !strings.EqualFold(b.IP, address) {
		return ""
	}
	fmtHost := address
	if strings.Contains(fmtHost, ":") {
		fmtHost = "[" + fmtHost + "]"
	}
	scheme := "http"
	if b.Public == 443 || b.Public == 8443 {
		scheme = "https"
	}
	if (scheme == "http" && b.Public == 80) || (scheme == "https" && b.Public == 443) {
		return scheme + "://" + fmtHost
	}
	return scheme + "://" + fmtHost + ":" + strconv.Itoa(b.Public)
}

// hostAltURLs builds the port links on every alternative address (at most 5 per address).
func hostAltURLs(alts []engine.HostAddress, ports []string) []engine.AltURL {
	var out []engine.AltURL
	for _, a := range alts {
		n := 0
		seen := map[string]bool{}
		for _, p := range ports {
			u := portURL(a.Address, p)
			if u == "" || seen[u] || n >= 5 {
				continue
			}
			seen[u] = true
			n++
			out = append(out, engine.AltURL{Label: a.Label, Kind: a.Kind, Address: a.Address, URL: u})
		}
	}
	return out
}
