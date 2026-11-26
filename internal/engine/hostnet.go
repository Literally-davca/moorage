package engine

import (
	"net"
	"net/netip"
	"sort"
	"strings"
)

// Kinds of a host address.
const (
	AddrEthernet = "ethernet"
	AddrWiFi     = "wifi"
	AddrVPN      = "vpn"
	AddrDomain   = "domain"
	AddrCustom   = "custom"
)

// HostAddress is one address of a host that services can be reached at.
type HostAddress struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Kind         string `json:"kind"` // ethernet | wifi | vpn | domain | custom
	Address      string `json:"address"`
	Interface    string `json:"interface,omitempty"`
	IsDefault    bool   `json:"isDefault,omitempty"`
	AutoDetected bool   `json:"autoDetected,omitempty"`
}

// RuntimeNet is how this Moorage process is attached to the host network (set once at
// start from the own container's inspect data, see selfupdate.DetectRuntimeNet).
type RuntimeNet struct {
	Mode string // NetModeNative | NetModeHost | NetModeBridge ("" = unknown, treated as bridge)
	// DataHostPath is the host path of the data directory when it is a bind mount
	// ("" for a named volume or unknown). Add-ons use <DataHostPath>/backups.
	DataHostPath string
}

// SetRuntimeNet records how Moorage runs (call before serving).
func (e *Engine) SetRuntimeNet(rt RuntimeNet) {
	e.selfMu.Lock()
	e.rtNet = rt
	e.selfMu.Unlock()
}

// RuntimeNet returns what SetRuntimeNet stored.
func (e *Engine) RuntimeNet() RuntimeNet {
	if e == nil {
		return RuntimeNet{}
	}
	e.selfMu.Lock()
	defer e.selfMu.Unlock()
	return e.rtNet
}

// NetMode is the network mode of this Moorage process (bridge when unknown).
func (e *Engine) NetMode() string {
	m := e.RuntimeNet().Mode
	if m == "" {
		return NetModeBridge
	}
	return m
}

// DetectedAddresses lists this host's own interface addresses in native / host-network
// mode (nil in bridge mode, where only the container's own interface is visible).
func (e *Engine) DetectedAddresses() []HostAddress {
	if m := e.NetMode(); m != NetModeNative && m != NetModeHost {
		return nil
	}
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var in []IfaceAddrs
	for _, ifc := range ifs {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		var list []string
		for _, a := range addrs {
			list = append(list, a.String())
		}
		in = append(in, IfaceAddrs{Name: ifc.Name, Up: ifc.Flags&net.FlagUp != 0, Loopback: ifc.Flags&net.FlagLoopback != 0, Addrs: list})
	}
	return ClassifyInterfaces(in)
}

// IfaceAddrs is the input of ClassifyInterfaces (net.Interfaces, made testable).
type IfaceAddrs struct {
	Name     string
	Up       bool
	Loopback bool
	Addrs    []string // CIDR strings from net.Interface.Addrs
}

// InterfaceKind classifies an interface by its name: "" means it is skipped (loopback,
// Docker bridges and veth pairs, libvirt, unknown virtual devices).
func InterfaceKind(name string) string {
	n := strings.ToLower(name)
	has := func(prefixes ...string) bool {
		for _, p := range prefixes {
			if strings.HasPrefix(n, p) {
				return true
			}
		}
		return false
	}
	switch {
	case n == "lo" || has("docker", "br-", "veth", "virbr", "cni", "flannel", "cali", "vxlan", "podman", "lxc", "lxd", "kube", "dummy"):
		return ""
	case has("tailscale", "wg", "wt", "tun", "tap", "zt", "nebula", "utun", "ppp", "ipsec", "netbird"):
		return AddrVPN
	case has("wl", "wlan", "wifi", "ath", "ra"):
		return AddrWiFi
	case has("eth", "en", "em", "eno", "ens", "enp", "bond", "br", "vmbr", "lan", "end"):
		return AddrEthernet
	}
	return ""
}

var kindLabel = map[string]string{AddrEthernet: "Ethernet", AddrWiFi: "Wi-Fi", AddrVPN: "VPN"}

// ClassifyInterfaces turns interface addresses into auto-detected host addresses:
// loopback, link-local and multicast addresses and virtual Docker interfaces are left
// out. Ethernet comes first, then Wi-Fi, then VPN; IPv4 before IPv6.
func ClassifyInterfaces(ifs []IfaceAddrs) []HostAddress {
	var out []HostAddress
	for _, ifc := range ifs {
		if !ifc.Up || ifc.Loopback {
			continue
		}
		kind := InterfaceKind(ifc.Name)
		if kind == "" {
			continue
		}
		for _, raw := range ifc.Addrs {
			p, err := netip.ParsePrefix(raw)
			var a netip.Addr
			if err == nil {
				a = p.Addr()
			} else if a, err = netip.ParseAddr(raw); err != nil {
				continue
			}
			a = a.Unmap()
			if a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
				continue
			}
			label := kindLabel[kind] + " (" + ifc.Name + ")"
			if strings.HasPrefix(strings.ToLower(ifc.Name), "tailscale") {
				label = "Tailscale (" + ifc.Name + ")"
			} else if strings.HasPrefix(strings.ToLower(ifc.Name), "wt") {
				label = "NetBird (" + ifc.Name + ")"
			}
			out = append(out, HostAddress{ID: "auto:" + ifc.Name + ":" + a.String(), Label: label, Kind: kind, Address: a.String(), Interface: ifc.Name, AutoDetected: true})
		}
	}
	rank := map[string]int{AddrEthernet: 0, AddrWiFi: 1, AddrVPN: 2}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].Kind] != rank[out[j].Kind] {
			return rank[out[i].Kind] < rank[out[j].Kind]
		}
		ai, aj := netip.MustParseAddr(out[i].Address), netip.MustParseAddr(out[j].Address)
		if ai.Is4() != aj.Is4() {
			return ai.Is4()
		}
		return out[i].Interface < out[j].Interface
	})
	return out
}
