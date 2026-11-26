package api

import (
	"reflect"
	"testing"

	"moorage/internal/engine"
)

func defaultOf(list []engine.HostAddress) string {
	for _, a := range list {
		if a.IsDefault {
			return a.Address
		}
	}
	return ""
}

func TestMergeHostAddresses(t *testing.T) {
	detected := []engine.HostAddress{
		{ID: "auto:eth0:192.168.1.10", Label: "Ethernet (eth0)", Kind: engine.AddrEthernet, Address: "192.168.1.10"},
		{ID: "auto:tailscale0:100.64.0.2", Label: "Tailscale (tailscale0)", Kind: engine.AddrVPN, Address: "100.64.0.2"},
	}

	// Nothing saved, no legacy value: the first detected address is the default.
	got := mergeHostAddresses("", "", nil, false, detected)
	if len(got) != 2 || defaultOf(got) != "192.168.1.10" || !got[0].AutoDetected {
		t.Fatalf("detected only: %+v", got)
	}

	// Legacy host_ip wins until a list is saved; the same detected address isn't repeated.
	got = mergeHostAddresses("", "192.168.1.10", nil, false, detected)
	if len(got) != 2 || got[0].ID != "primary" || defaultOf(got) != "192.168.1.10" {
		t.Fatalf("legacy: %+v", got)
	}

	// MOORAGE_HOST_IP is locked and always the default.
	saved := []engine.HostAddress{{ID: "m-1", Label: "NAS domain", Kind: engine.AddrDomain, Address: "nas.lan", IsDefault: true}}
	got = mergeHostAddresses("10.0.0.2", "", saved, true, detected)
	if got[0].ID != "env" || !got[0].Locked || defaultOf(got) != "10.0.0.2" {
		t.Fatalf("env: %+v", got)
	}
	n := 0
	for _, a := range got {
		if a.IsDefault {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("exactly one default, got %d", n)
	}

	// A saved auto-detected default stays the default; when the interface is gone it is
	// kept and marked missing instead of silently switching links to another address.
	saved = []engine.HostAddress{
		{ID: "m-1", Label: "NAS domain", Kind: engine.AddrDomain, Address: "nas.lan"},
		{ID: "auto:tailscale0:100.64.0.2", Label: "Tailscale (tailscale0)", Kind: engine.AddrVPN, Address: "100.64.0.2", IsDefault: true, AutoDetected: true},
	}
	got = mergeHostAddresses("", "ignored", saved, true, detected)
	if defaultOf(got) != "100.64.0.2" || len(got) != 3 {
		t.Fatalf("saved auto default: %+v", got)
	}
	got = mergeHostAddresses("", "", saved, true, detected[:1])
	last := got[len(got)-1]
	if defaultOf(got) != "100.64.0.2" || !last.Missing {
		t.Fatalf("missing default: %+v", got)
	}
	if alts := alternativeAddresses(got); len(alts) != 2 {
		t.Fatalf("alternatives: %+v", alts)
	}
}

func TestCleanHostAddresses(t *testing.T) {
	list, msg := cleanHostAddresses([]engine.HostAddress{
		{ID: "env", Address: "10.0.0.2", Locked: true, IsDefault: true},                    // dropped
		{Label: "VPN", Kind: engine.AddrVPN, Address: " 100.64.0.2 "},                      // trimmed, id generated
		{ID: "auto:eth0:1", Address: "192.168.1.10", AutoDetected: true},                   // not default: not stored
		{ID: "auto:wlan0:2", Address: "192.168.1.50", AutoDetected: true, IsDefault: true}, // chosen default
		{Address: "[fd00::1]"},
	})
	if msg != "" {
		t.Fatal(msg)
	}
	if len(list) != 3 || list[0].Address != "100.64.0.2" || list[0].ID == "" || list[1].Address != "192.168.1.50" || !list[1].IsDefault {
		t.Fatalf("got %+v", list)
	}
	if list[2].Address != "fd00::1" || list[2].Kind != engine.AddrCustom || list[2].Label != "fd00::1" {
		t.Fatalf("ipv6: %+v", list[2])
	}
	for _, bad := range [][]engine.HostAddress{
		{{Address: "http://nas"}},
		{{Address: "nas.lan:8080"}},
		{{Address: "a b"}},
		{{Address: "x", Kind: "bogus"}},
		{{Address: "a", IsDefault: true}, {Address: "b", IsDefault: true}},
		{{Address: "a"}, {Address: "A"}},
	} {
		if _, msg := cleanHostAddresses(bad); msg == "" {
			t.Errorf("accepted %+v", bad)
		}
	}
	// Without a chosen default the first manual entry becomes it.
	list, _ = cleanHostAddresses([]engine.HostAddress{{Address: "nas.lan"}})
	if !list[0].IsDefault || list[0].Kind != engine.AddrDomain {
		t.Fatalf("implicit default: %+v", list)
	}
}

func TestHostPortURLs(t *testing.T) {
	ports := []string{"8080:80/tcp", "127.0.0.1:5432:5432/tcp", "[::1]:6379:6379/tcp", "53:53/udp", "100.64.0.2:8090:8090/tcp", "443:8443/tcp", "80:80/tcp"}
	got := hostPortURLs("192.168.1.10", ports)
	want := []string{"http://192.168.1.10:8080", "http://100.64.0.2:8090", "https://192.168.1.10", "http://192.168.1.10"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if got := hostPortURLs("fd00::10", []string{"8080:80/tcp"}); !reflect.DeepEqual(got, []string{"http://[fd00::10]:8080"}) {
		t.Fatalf("ipv6 host: %v", got)
	}
	if hostPortURLs("", ports) != nil {
		t.Fatal("no host address, no links")
	}

	alts := hostAltURLs([]engine.HostAddress{
		{Label: "Wi-Fi (wlan0)", Kind: engine.AddrWiFi, Address: "192.168.1.50"},
		{Label: "Tailscale", Kind: engine.AddrVPN, Address: "100.64.0.2"},
	}, ports)
	var urls []string
	for _, a := range alts {
		urls = append(urls, a.Label+" "+a.URL)
	}
	wantAlts := []string{
		"Wi-Fi (wlan0) http://192.168.1.50:8080", "Wi-Fi (wlan0) https://192.168.1.50", "Wi-Fi (wlan0) http://192.168.1.50",
		"Tailscale http://100.64.0.2:8080", "Tailscale http://100.64.0.2:8090", "Tailscale https://100.64.0.2", "Tailscale http://100.64.0.2",
	}
	if !reflect.DeepEqual(urls, wantAlts) {
		t.Fatalf("alts %v\nwant %v", urls, wantAlts)
	}
}

func TestPortUses(t *testing.T) {
	rep := engine.PortReport{Ports: []engine.PortEntry{
		{Port: 22, Proto: "tcp", Source: engine.PortSourceHost, State: engine.PortStateListening},
		{Port: 8080, Proto: "tcp", Source: engine.PortSourceStack, Stack: "web", Container: "web-1", State: engine.PortStateRunning},
		{Port: 8081, Proto: "tcp", Source: engine.PortSourceStack, Stack: "self", Container: "self-1", State: engine.PortStateRunning},
		{Port: 8096, Proto: "tcp", Source: engine.PortSourceCompose, Stack: "media", Service: "jelly", State: engine.PortStateReserved},
		{Port: 9000, Proto: "tcp", Source: engine.PortSourceContainer, Container: "old", State: "exited"},
	}}
	got := portUses(rep, "self")
	if len(got) != 3 || got[0].Owner != "a process on the host" || got[1].Owner != "web/web-1" || !got[2].Reserved {
		t.Fatalf("got %+v", got)
	}
}
